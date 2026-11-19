# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Developers and platform engineers who need GitHub Actions jobs to run on
hardware they control. They operate Linux and Windows workloads across local
workstations, lab machines, and dedicated runner hosts.

## Product Purpose

multirunner runs multiple fresh GitHub Actions self-hosted runners on one host.
Each runner accepts one job, exits, and is replaced. Success means users can run
parallel jobs without manually maintaining a fleet of long-lived runner
installations.

## Positioning

One binary and one YAML file manage ephemeral runner pools, local Actions cache,
git mirrors, and runner scale-set demand. The product uses the official GitHub
Actions runner and `actions/scaleset` technology without requiring Kubernetes
or a separate control plane.

## Operating Context

Users install or build the CLI, connect GitHub credentials, define runner pools,
validate the plan, and run multirunner directly or as an operating-system
service. Linux jobs use Docker or Podman. Windows jobs use containerd with
runhcs or QEMU virtual machines. Focused agent skills support setup, host
management, diagnosis, GitHub configuration, and QEMU operations.

## Capabilities and Constraints

- Linux and Windows runner workloads
- Fixed pools, autoscaling, and GitHub Actions runner scale sets
- Docker, Podman, containerd, runhcs, and QEMU backends
- Windows, Linux, and macOS host support
- Local Actions cache, git mirror, health, and Prometheus metrics
- Static Astro project site deployed below `/multirunner/` on GitHub Pages
- No claim that tagged release `v0.0.2` contains runner scale-set support

## Brand Commitments

The product name is `multirunner` and the owner is ActionsCommunity. The voice
is direct, technical, and specific. It does not use abstract AI-generated
marketing language. A new logo, image system, and color system are part of the
current redesign; no incumbent visual asset is binding.

## Evidence on Hand

- Product and setup details: `README.md`
- Operator reference: `skills/docs/cli-reference.md`
- Host and QEMU references: `skills/docs/`
- Current site content: `src/`
- Current artwork: placeholders only in `public/images/`
- No customer logos, testimonials, usage metrics, or production screenshots are
  available and none may be fabricated

## Product Principles

- Keep runners disposable.
- Put users in control of their own hardware and credentials.
- Reuse official GitHub runner and scale-set technology.
- Make every state-changing operation previewable and explicit.
- Prefer one host process over a new control plane.

## Accessibility & Inclusion

The public site must meet WCAG 2.1 AA, support keyboard navigation, respect
reduced motion, remain readable at 200 percent zoom, and preserve clear contrast
in all generated imagery and UI states.
