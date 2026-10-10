# Dependency maintenance exceptions

The dependency refresh deliberately leaves these packages unchanged:

- `glob@11.1.0` is a deprecated build-only dependency selected by
  `vite-plugin-pwa@2.0.0 -> workbox-build@7.4.1`, which currently declares
  `glob@^11.0.1`. The supported replacement is `glob@13`, outside that range.
  Overriding a build tool across two major versions is not a safe patch/minor
  update. Track Workbox and remove this exception when it supports a maintained
  Glob release. `pnpm audit` currently reports no advisory for this path.
- `github.com/docker/docker@v28.5.2+incompatible` remains graph-only through
  `github.com/actions/scaleset@v0.4.0`, whose latest release still requires the
  legacy module. Multirunner runtime and test imports use the supported
  `github.com/moby/moby/api` and `github.com/moby/moby/client` modules; the
  legacy module is not compiled into `cmd/multirunner`. Remove this exception
  when `actions/scaleset` publishes a migrated release.
- `google.golang.org/grpc@v1.83.2` is retained as an explicit indirect
  requirement to keep the graph-only `actions/scaleset` dependency above its
  vulnerable lower version. It is not currently compiled into
  `cmd/multirunner`; keep the floor until `actions/scaleset` requires an equal
  or newer release.
- `typescript@7` is a major compiler migration rather than a safe dependency
  refresh. The console remains on the latest compatible TypeScript 6 release.
- `@types/node@26` targets a different Node major. The console supports Node 24
  and retains the current Node 24 type line.
