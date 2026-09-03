---
author: "@jongio"
status: approved
---

# GitHub Pages site

## Problem

multirunner has a detailed repository README, but it has no public project site.
People evaluating the tool must scan a long technical document before they can
understand the core value, supported execution backends, or first setup steps.
The repository also lacks an automatic GitHub Pages deployment path.

A project site must work below `https://actionscommunity.github.io/multirunner/`. Building
for the domain root would leave styles, images, and internal links broken after
deployment.

## Goals

- Explain multirunner's purpose and core capabilities on a focused landing page.
- Publish installation and quick-start content from the repository.
- Explain the GitHub Actions runner scale set technology that drives scale-set mode.
- Document the cross-agent plugin and its five focused skill routers.
- Provide a reference for every public CLI command and its flags.
- Build all internal URLs for the `/multirunner/` project path.
- Deploy automatically through the official GitHub Pages Actions flow.
- Clearly label artwork that still needs final project assets.

## Non-Goals

- Replace the full README or configuration example.
- Add interactive application behavior or a client-side router.
- Create final branding, screenshots, or a custom domain.
- Change multirunner's Go CLI behavior.

## Solution

Add an Astro static site at the repository root. Astro supplies static generation
with explicit `site` and `base` configuration, so Pages receives prebuilt HTML
whose internal URLs include `/multirunner/`.

The landing page uses the repository pitch, install command, quick-start
configuration, supported backends, and operating model. Dedicated sections explain
the `github.com/actions/scaleset` integration and the plugin shared by Copilot CLI,
Claude Code, and Codex. A separate command reference reflects the Cobra command
tree and documented flags. Links to deeper configuration, releases, skills, and
source remain on GitHub rather than duplicating all repository documentation.

Deployment uses `withastro/action` to build and upload the Pages artifact, followed
by `actions/deploy-pages`. Action references are pinned to immutable commit SHAs.
The workflow runs only for the `main` branch and uses the least permissions needed
by each job.

The site references labeled SVG placeholders for its logo, favicon, social card,
and terminal demonstration. `public/images/IMAGES.md` records filenames and
recommended dimensions so final artwork can replace each asset without changing
page markup.

## Alternatives Considered

- **Static HTML**: This would avoid a build tool, but repeated layout and reference
  content would be harder to maintain as the documentation grows.
- **React and Vite**: The site has no application state or client routing, so a
  browser runtime would add cost without user value.
- **Jekyll**: GitHub supports it natively, but Astro provides a clearer modern
  content path while still emitting plain static files for Pages.
- **A deployment branch**: A `gh-pages` branch adds state and maintenance that the
  first-party Pages artifact workflow does not need.

## Acceptance Criteria

1. The landing page explains multirunner using claims and examples grounded in
   the repository.
2. The CLI reference covers every public Cobra command and global option.
3. Astro builds the landing and command routes without template demo content.
4. Every root-relative built URL starts with `/multirunner/`.
5. Pull requests test the site, and the main-branch Pages workflow verifies the
   lockfile and tests before artifact upload.
6. Required artwork has a served placeholder and a documented replacement path.
7. Desktop and mobile routes render without overflow, broken images, console
   errors, or inaccessible keyboard entry.
8. The landing page explains Actions runner scale sets and links to the upstream
   `actions/scaleset` technology.
9. The plugin section includes the verified install command, supported agents,
   and all five skill routers.

## Impact Scan

- **Go application**: No runtime or public CLI behavior changes.
- **Repository tooling**: Adds Node 24 and npm for site builds while retaining the
  existing Go CI matrix.
- **CI/CD**: Adds a site job to the test workflow and a separate Pages deployment.
- **Public surface**: Adds two static routes and project metadata for search and
  social previews.
- **Security**: Adds npm and action supply-chain inputs, constrained by lockfile
  verification, strict lifecycle-script allowlisting, SHA pins, and scoped tokens.
- **Operations**: Pages and repository homepage settings remain control-plane
  actions performed only after explicit approval.

## Convention Discovery

- Existing repository copy, install examples, configuration, commands, and flags
  remain the content source of truth.
- GitHub Actions use the repository's `main` branch and explicit CI jobs.
- The selected template uses Astro's `site`, `base`, and `BASE_URL` conventions
  for GitHub project sites.
- Site tests use Node's built-in test runner, avoiding a second test framework.
- Generated artwork follows the Pages skill's `public/images/IMAGES.md` contract.

## Quality Gates

- `npm ci --no-audit --no-fund`
- `npm test`
- `go vet ./...`
- `go build ./...`
- `go test -short ./...`
- `actionlint` for both changed workflows
- `npm audit --omit=dev --audit-level=high`
- Playwright desktop, mobile, keyboard, console, network, and nested-route smoke
- Template-copy, sentinel, base-path, secret, anti-slop, documentation, and link checks

## Pre-Completion Interview

- Site format: Astro CLI documentation and marketing site.
- Deployment identity: `actionscommunity/multirunner`.
- Pages base path: `/multirunner/`.
- Artwork: retain the four labeled placeholders for this version.
- Open questions: none.

## Gut-Check Results

- **Greenfield**: Astro is appropriate for a content-oriented project site whose
  command and reference material can grow beyond two pages.
- **Simplicity**: The output is static HTML with no client runtime, state library,
  API, or custom deployment branch.
- **Dependency**: Astro adds build dependencies, but owns project-base URL handling
  and page composition that would otherwise be repeated by hand.
- **Reversibility**: Pages receives plain files, so replacing the generator later
  does not change the public URL contract.
- **Verdict**: Keep the Astro design. Confidence: high.

## Risks & Rabbit Holes

- Every new internal link or asset must keep using Astro's generated base path.
- Social platforms commonly ignore SVG preview cards. The placeholder manifest
  calls for replacing `og.svg` with a raster image and updating the meta tag.
- CLI documentation can drift as Cobra flags change. Repository tests derive the
  public command inventory from Cobra declarations, while detailed flag prose
  remains intentionally maintained by hand.
- Tagged release `v0.0.2` predates runner scale sets. The site points readers to
  the current source build until a release includes the advertised capability.
- Final artwork is intentionally outside this change and remains visibly labeled.

## Done Definition

- All nine acceptance criteria are satisfied.
- The covered test plan has zero functionality gaps.
- Build, tests, workflow lint, dependency audit, and browser smoke pass.
- Required Pages files exist under the repository root.
- No critical or high code finding remains.
- Control-plane changes wait for explicit user approval.

<!-- Pipeline tracking (auto-managed, not part of product spec) -->
## Pipeline Status

Phase: CERTIFYING
