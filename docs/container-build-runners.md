# Container build runners

Container image builds need a stronger trust boundary than general CI. A job
with access to a Docker daemon can create privileged containers, mount daemon
filesystems, inspect other containers, and control every image and volume on
that daemon.

Do not mount the general host Docker socket into the normal Linux pool. Docker
socket access is effectively root authority over that Docker host.

## Recommended topology

Use one dedicated Docker daemon for container-build runners:

1. The normal Linux and Windows pools remain on their existing daemons.
2. A pinned Docker-in-Docker container provides a separate builder daemon.
3. The builder daemon exposes mutual TLS only on host loopback.
4. One multirunner pool connects to that daemon, binds to one repository, and
   carries only the `container-build` label.
5. The runner receives the builder daemon socket through `enable_dind`.
6. Publishing workflows remain manual and keep registry credentials in their
   repository only.
7. Ordinary jobs in the protected repository request a separate `general-ci`
   label that the builder runner does not carry.

The Docker-in-Docker container is privileged. Compromise of a build job grants
root-equivalent authority over the dedicated builder daemon. It does not grant
Docker API access to the daemon that runs general multirunner workloads.

## Provision the daemon

Run the installer from an elevated PowerShell 7 session:

```powershell
.\scripts\install-container-build-daemon.ps1
```

The script creates these durable resources:

| Resource | Purpose |
| --- | --- |
| `multirunner-container-build-docker` | Pinned Docker 29.8.0 daemon with restart policy |
| `multirunner-container-build-certs` | Server and client mutual TLS certificates |
| `multirunner-container-build-data` | Builder images and cache |
| `C:\multirunner\container-build\tls` | ACL-protected client certificates for the service |

The client certificate and key stay on the host. They are never injected into
runner containers or GitHub workflow environments. The installer also builds
the pinned container-capable runner image inside the dedicated daemon and
prints its immutable `sha256` image ID.

Pass `-SkipImageBuild` only when the exact image already exists in the builder
daemon. Use `-Replace` to recreate the daemon, and add `-RotateCertificates` to
replace its mutual TLS identity.

To rebuild the image manually, build the Linux image chain against the
dedicated daemon. The final tag must match the pool configuration:

```powershell
$tls = @(
  '--tlsverify',
  '--tlscacert', 'C:\multirunner\container-build\tls\ca.pem',
  '--tlscert', 'C:\multirunner\container-build\tls\cert.pem',
  '--tlskey', 'C:\multirunner\container-build\tls\key.pem',
  '--host', 'tcp://127.0.0.1:23760'
)

docker @tls build -f images/linux/Dockerfile `
  -t multirunner/runner-linux:minimal .
docker @tls build -f images/linux/flavors/native-build.Dockerfile `
  --build-arg PARENT=multirunner/runner-linux:minimal `
  -t multirunner/runner-linux:native-build .
docker @tls build -f images/linux/flavors/node.Dockerfile `
  --build-arg PARENT=multirunner/runner-linux:native-build `
  -t multirunner/runner-linux:container-build .

docker @tls image inspect `
  --format '{{.Id}}' `
  multirunner/runner-linux:container-build
```

## Configure the pool

Add a size-one pool. Use only the custom label so generic Linux jobs cannot
consume a builder runner.

```yaml
pools:
  - name: container-build
    os: linux
    size: 1
    image: sha256:<runner-image-id-from-installer>
    labels: [container-build]
    repository: jongio/backyahdbbq
    workflows:
      - .github/workflows/build-api-image.yml
    workflow_event: workflow_dispatch
    workflow_actor: jongio
    work_folder: _work
    name_prefix: multirunner
    docker:
      host: "tcp://127.0.0.1:23760"
      tls:
        ca: 'C:\multirunner\container-build\tls\ca.pem'
        cert: 'C:\multirunner\container-build\tls\cert.pem'
        key: 'C:\multirunner\container-build\tls\key.pem'
      enable_dind: true
```

Use `runs-on: [container-build]` only in trusted workflows. Labels select
runners but are not an authorization system. The `repository` binding is a
second check in the autoscaler and launcher, so the pool cannot register to
another repository even if that repository requests the same label. The
workflow path, event, and actor tuple is resolved from GitHub's workflow-run
record before launch. A pull-request workflow that requests the label is
rejected before a runner is registered.

Add `general-ci` to the normal Linux pool and require it on every ordinary
self-hosted workflow in the protected repository:

```yaml
# Normal shared Linux pool
labels: [self-hosted, linux, x64, general-ci]

# Ordinary repository job
runs-on: [self-hosted, Linux, X64, general-ci]
```

This prevents a generic job from matching a builder runner even if GitHub adds
default self-hosted, OS, or architecture labels during JIT registration.

Use the immutable image ID printed by the installer. A mutable tag would let a
job with daemon access replace the image used by the next privileged runner.

## Workflow boundary

Container publication workflows should meet all of these requirements:

- `workflow_dispatch` only.
- Build a commit from the protected default branch history.
- Use `contents: read` and `packages: write` only.
- Read registry credentials only after source validation.
- Never expose registry credentials to `pull_request` jobs.
- Record and upload the immutable `sha256` image reference.
- Do not deploy or promote as part of runner verification.

## Rotation and recovery

Re-run the installer normally after runner image changes. Stop the multirunner
service and use `-Replace` when the Docker-in-Docker image or daemon settings
change. Replacement fails while the service, another controller process, or a
builder child container is active. Add `-RotateCertificates` when rotating
mutual TLS credentials. The script always preserves the builder data volume.

Run `multirunner doctor` before restarting the service. TLS paths are part of
the pool configuration, so interactive diagnostics and the Windows service use
the same credentials. If the builder daemon, certificates, or immutable runner
image are unavailable, preflight must fail rather than starting a pool that
cannot provide its declared capability.
