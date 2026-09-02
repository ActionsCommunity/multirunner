# Linux host

Use this reference after the shared host assessment.

## Supported choices

| Workload | Backend | When to select it |
|---|---|---|
| Linux containers | `docker` or omitted | Use a reachable Docker API. A Podman Docker-compatible socket is also supported when it passes doctor. |
| Windows virtual machines | `qemu` | Use a licensed Windows Server ISO and an x86-64 QEMU guest. KVM is the accelerated choice on x86-64 Linux. |

The `containerd` backend is the Windows containerd and runhcs implementation,
not a Linux container backend. Don't select it for Linux pools.

## Read-only assessment

Check architecture, distribution, free disk and memory, service state, and the
configured Docker-compatible endpoint. For QEMU, locate
`qemu-system-x86_64`, check `/dev/kvm` access, and confirm firmware availability.
Run `multirunner doctor --config <path>` only after reviewing the redacted
config.

multirunner has no Linux package-install command. Show the exact distribution
package action and ask before elevation or installation. Don't enable a daemon,
change socket permissions, add group membership, or start a service without
specific approval.

For health and troubleshooting, verify runtime reachability, daemon OS, image
architecture, storage, DNS, and KVM access before proposing a change.
