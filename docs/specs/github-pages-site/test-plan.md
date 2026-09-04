# Test Plan: GitHub Pages site

## Status: COVERED
## Spec: docs/specs/github-pages-site/spec.md
## Created: 2026-09-02
## Updated: 2026-09-03

---

## Coverage Strategy

Node's built-in test runner inspects source, workflow, image, and built output
invariants. `npm test` builds the site before assertions, which covers Astro
rendering and generated routes. The existing Go suite confirms the new root
toolchain does not affect multirunner. Browser verification covers real HTTP
loading, internal navigation, nested-route refresh, image loading, and layout
overflow at the preview viewport.

## Planned Tests

| ID | Behavior to verify | Source | Level | Test file or command | Status |
|----|--------------------|--------|-------|----------------------|--------|
| T1 | Astro builds the landing and command pages | AC-1, AC-3 | integration | `npm test` | automated |
| T2 | Config targets `actionscommunity.github.io` with base `/multirunner/` | AC-4 | unit | `test/site.test.mjs` | automated |
| T3 | Pages workflow targets main with pinned official actions, required permissions, and lockfile verification | AC-5 | unit | `test/site.test.mjs` | automated |
| T4 | Site source uses a working current-main build command and contains no template copy or sentinels | AC-1, AC-2 | unit | `test/site.test.mjs` | automated |
| T5 | Built root-relative URLs retain the `/multirunner/` prefix | AC-4 | integration | `test/site.test.mjs` | automated |
| T6 | Built command reference stays in parity with public and generated Cobra commands and required flags | AC-3 | integration | `test/site.test.mjs` | automated |
| T7 | Production images exist at exact dimensions and no placeholder SVG remains | AC-6 | unit | `test/site.test.mjs` | automated |
| T8 | Landing and nested routes load over HTTP, including direct command-route refresh | AC-4 | end-to-end | browser canvas and HTTP preview checks | automated |
| T9 | Site images load and desktop pages have no horizontal overflow | AC-1 | end-to-end | browser canvas runtime inspection | automated |
| T10 | Template generator, repository digest, and image-manifest helpers remain valid | AC-4, AC-6 | integration | `npm --prefix C:\Users\jong\.agents\skills\create-gh-pages-site test` | automated |
| T11 | Existing multirunner behavior remains green | Non-regression | integration | `go test ./...` with invocation-scoped Git safety config | automated |
| T12 | Site dependencies have no known high-severity vulnerabilities | Security | integration | `npm audit --omit=dev --audit-level=high` | automated |
| T13 | Pull requests and deployment run complete site tests with read-only permissions, strict install scripts, and a timeout | AC-5 | integration | `test/site.test.mjs` | automated |
| T14 | Every built page provides skip navigation and explicit keyboard focus styling | Accessibility | integration | `test/site.test.mjs` | automated |
| T15 | Landing page includes the scale-set technology, plugin install, supported clients, and all five skills | AC-8, AC-9 | integration | `test/site.test.mjs` | automated |
| T16 | Runner Yard contract, built tokens, design sidecar, and image palette stay synchronized | AC-1, AC-6, AC-7 | integration | `test/site.test.mjs` | automated |

## Functionality Inventory

| # | Functionality introduced | Location | Covered by | Status |
|---|--------------------------|----------|------------|--------|
| F1 | Astro site identity, metadata, navigation, and shared responsive layout | `src/layouts/Layout.astro:1` | T1, T2, T5, T8, T9, T14 | covered |
| F2 | Repository-derived landing, install, quick start, features, and backends | `src/pages/index.astro:1` | T1, T4, T8, T9 | covered |
| F2a | Actions runner scale set technology and configuration | `src/components/ScaleSetSection.astro:1` | T15 | covered |
| F2b | Cross-agent plugin installation and skill directory | `src/components/AgentPluginSection.astro:1` | T15 | covered |
| F3 | Public command and flag reference | `src/pages/commands.astro:1` | T1, T6, T8 | covered |
| F4 | Project-site URL configuration | `astro.config.mjs:1` | T2, T5, T8 | covered |
| F5 | Main-branch Pages deployment | `.github/workflows/deploy.yml:1` | T3 | covered |
| F5a | Pull request site quality gate | `.github/workflows/test.yml:26` | T13 | covered |
| F6 | Generated logo, favicon, hero, social card, and provenance record | `public/images/IMAGES.md:1` | T7, T10, T16 | covered |
| F7 | Dependency lock and Astro build scripts | `package.json:1` | T1, T12 | covered |
| F8 | Isolation from the existing Go application | `.gitignore:21` | T11 | covered |
| F9 | Runner Yard tokens, local fonts, design contract, and design sidecar | `src/styles/global.css:1` | T14, T16 | covered |

## Gaps & Additions

The late-entry reconciliation added `test/site.test.mjs` because the initial
verification commands were not persistent repository automation. No functionality
gaps remain.
