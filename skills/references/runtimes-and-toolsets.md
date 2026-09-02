# Runtimes and toolsets

Choose the topology from the workload, host, isolation boundary, and required
tools. Don't change a backend merely because another runtime is installed.

## Runtime selection

| Pool OS | Backend | Runtime unit | Tool selection |
|---|---|---|---|
| Linux | `docker` or omitted | Ephemeral Linux container | `image`, or a Linux `image_tier` |
| Windows | `docker` or omitted | Ephemeral Windows container | `image`, or a Windows `image_tier` |
| Windows | `containerd` | Ephemeral Windows container through runhcs | `image`, or a Windows `image_tier` |
| Windows | `qemu` | Ephemeral overlay from a golden Windows guest | Tools baked into the golden image |

An explicit `image` wins over `image_tier`. Published Linux tiers are `minimal`,
`native-build`, `node`, `dotnet`, `rust`, and `go`. Published Windows tiers are
`minimal`, `node`, `dotnet`, `buildtools`, and the published
`buildtools:<line>` variants. An unknown syntactically valid tier resolves to a
local `multirunner/runner-<os>-<tier>:dev` image, so select one only when that
image is intentionally built and present. `multirunner detect --path <checkout>
--os <linux-or-windows>` can recommend a container tier without mutation.

## QEMU golden tools

QEMU ignores `image` and non-minimal `image_tier`. Select golden tools with
`dotnet[:major]`, `node[:major]`, `go`, or `buildtools[:line]`:

```text
multirunner bake --iso <windows-iso> --iso-sha256 <sha256> --golden <qcow2> --tools dotnet,node:24,buildtools:17
```

The equivalent pool field is:

```yaml
qemu:
  golden: "<qcow2>"
  tools: [dotnet, "node:24", "buildtools:17"]
```

`qemu.bake_iso` enables managed rebuilds. `qemu.bake_iso_sha256` verifies the
media. A non-default `qemu.runner_version` also requires
`qemu.runner_sha256`. The configured tools participate in the golden
fingerprint. Show the ISO, checksums, tools, disk impact, accelerator, and
rebuild effect before approval.

For updates, compare current and proposed binary, image digest, image tier, and
QEMU tool fingerprint. Pull or rebuild only the approved pools, never prune
unrelated images, and finish with doctor and canary verification.
