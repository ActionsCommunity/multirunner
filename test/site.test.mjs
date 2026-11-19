import assert from "node:assert/strict";
import { existsSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { publicHelp, withCLI } from "../scripts/site-cli.mjs";

const read = (path) => readFileSync(path, "utf8").replaceAll("\r\n", "\n");

const entities = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
const decode = (value) => value.replace(/&(#x[\da-f]+|#\d+|amp|lt|gt|quot|apos);/gi, (_, key) =>
  key.startsWith("#") ? String.fromCodePoint(Number(key.replace(/^#x/i, "0x").replace(/^#/, ""))) : entities[key]);
const pages = readdirSync("dist", { recursive: true })
  .filter((file) => file.endsWith(".html"))
  .map((file) => file.replaceAll("\\", "/"));

test("Astro targets the multirunner project path", () => {
  const config = read("astro.config.mjs");

  assert.match(config, /site:\s*"https:\/\/actionscommunity\.github\.io"/);
  assert.match(config, /base:\s*"\/multirunner\/"/);
});

test("Pages workflow deploys main with current first-party actions", () => {
  const workflow = read(".github/workflows/deploy.yml");

  assert.match(workflow, /branches:\s*\[main\]/);
  assert.match(workflow, /run:\s+pnpm install --frozen-lockfile/);
  assert.match(workflow, /actions\/upload-pages-artifact@/);
  assert.match(workflow, /actions\/deploy-pages@[a-f0-9]+\s+# v5\.0\.1/);
  assert.match(workflow, /run:\s+pnpm test/);
  assert.match(workflow, /pages:\s+write/);
  assert.match(workflow, /id-token:\s+write/);
  assert.doesNotMatch(workflow, /__[A-Z0-9_]+__/);
});

test("pull request CI runs the complete site test command", () => {
  const workflow = read(".github/workflows/test.yml");
  assert.match(workflow, /permissions:\n  contents:\s+read/);
  assert.match(workflow, /\n  site:\n/);
  assert.match(workflow, /site:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10/);
  assert.match(workflow, /node-version:\s+24/);
  assert.match(workflow, /run:\s+pnpm install --frozen-lockfile/);
  assert.match(workflow, /run:\s+pnpm test/);
});

test("site source contains real multirunner content without template copy", () => {
  const source = [
    read("src/components/AgentPluginSection.astro"),
    read("src/components/ScaleSetSection.astro"),
    read("src/layouts/Layout.astro"),
    read("src/pages/index.astro"),
    read("src/pages/commands.astro"),
  ].join("\n");
  const buildCommand = "go build ./cmd/multirunner";

  assert.match(source, /Run GitHub Actions on your own hardware\./);
  assert.doesNotMatch(source, /Self-hosted runners\. On your machine\./);
  assert.doesNotMatch(source, /Parallel jobs without the control plane\./);
  assert.ok(source.includes(buildCommand));
  assert.doesNotMatch(source, /go install github\.com\/ActionsCommunity\/multirunner/);
  assert.doesNotMatch(
    source,
    /Hello from a content collection|Islands, not bundles|Welcome to Astro|lorem/i,
  );
  assert.doesNotMatch(source, /__[A-Z0-9_]+__/);
});

test("landing page documents runner scale sets and all agent skill routers", () => {
  const home = read("dist/index.html");

  assert.match(home, /href="\/multirunner\/#features">How it works/);
  assert.match(home, /href="\/multirunner\/#get-started">Get started/);
  assert.doesNotMatch(home, />Scale sets<\/a>/);
  assert.match(home, /github\.com\/actions\/scaleset/);
  assert.match(home, /provisioning: scaleset/);
  assert.match(home, /same scale-set mechanism used by actions-runner-controller/);
  assert.match(home, /copilot plugin install ActionsCommunity\/multirunner/);
  for (const skill of [
    "multirunner-setup",
    "multirunner-host",
    "multirunner-diagnose",
    "multirunner-github",
    "multirunner-qemu",
  ]) {
    assert.ok(home.includes(skill), `Missing agent skill ${skill}`);
  }
});

test("Runner Yard design artifacts use the approved five-color system", () => {
  const contract = JSON.parse(
    read("docs/specs/github-pages-site/design/design-contract.json"),
  );
  const sidecar = JSON.parse(read(".impeccable/design.json"));
  const styles = read("src/styles/global.css").toUpperCase();
  const expectedColors = ["#F3F7FA", "#101C2A", "#174FC4", "#C6400C", "#596978"];

  assert.deepEqual(Object.values(contract.direction.palette), expectedColors);
  for (const color of expectedColors) {
    assert.ok(styles.includes(color), `Global styles missing ${color}`);
    assert.ok(
      Object.values(sidecar.extensions.colorMeta).some(
        (entry) => entry.canonical === color,
      ),
      `Design sidecar missing ${color}`,
    );
  }
  assert.match(read("DESIGN.md"), /Creative North Star: "The Runner Yard"/);
});

test("manual renders exact public binary help, and both YAML examples validate offline", () => {
  const manual = read("dist/commands/index.html");
  const documented = new Map([...manual.matchAll(/<pre data-cli="([^"]*)"[^>]*><code\b[^>]*>([\s\S]*?)<\/code><\/pre>/g)]
    .map(([, path, value]) => [path, decode(value)]));
  const examples = [...read("dist/index.html").matchAll(/<pre data-config-example="([^"]+)"[^>]*><code\b[^>]*>([\s\S]*?)<\/code><\/pre>/g)];
  assert.deepEqual(examples.map(([, mode]) => mode).sort(), ["pool", "scaleset"]);
  withCLI((cli, dir) => {
    const help = publicHelp(cli);
    assert.ok(help.some(({path}) => path.join(" ") === "service start"));
    assert.ok(help.some(({path}) => path.join(" ") === "completion bash"));
    assert.deepEqual(documented, new Map(help.map(({path, help}) => [path.join(" "), help])));
    for (const [, mode, yaml] of examples) {
      const file = `${mode}.yaml`;
      writeFileSync(join(dir, file), decode(yaml));
      const result = cli(["run", "--dry-run", "--config", file], {
        env: {...process.env, GITHUB_PAT: "offline-docs-test-fixture"},
      });
      assert.match(result, new RegExp(`provisioning[^\\n]*${mode}`, "i"));
    }
  });
});

test("operations illustrates current image pins and the actual metrics routes", () => {
  const html = read("dist/operations/index.html");
  const versions = JSON.parse(read("images/versions.json"));
  const { repository, tag } = versions.minimal.linux_base;
  for (const value of [
    `${repository}:${tag}`,
    versions.minimal.runner.version,
    ...Object.values(versions.dotnet.channels).map(({version}) => version),
    "/health", "/metrics",
  ]) {
    assert.ok(html.includes(value), `Operations page missing ${value}`);
  }
  assert.doesNotMatch(html, /\/healthz/);
});

test("built local links, anchors, and responsive image assets resolve under the Pages prefix", () => {
  const origin = "https://actionscommunity.github.io";
  assert.deepEqual(pages.sort(), ["commands/index.html", "index.html", "operations/index.html"]);
  for (const file of pages) {
    const html = read(`dist/${file}`);
    const ids = [...html.matchAll(/ id="([^"]+)"/g)].map(([, id]) => id);
    assert.equal(new Set(ids).size, ids.length, `${file}: duplicate ids`);
    const urls = [...html.matchAll(/\s(href|src|srcset)="([^"]+)"/g)].flatMap(([, attr, value]) =>
      attr === "srcset" ? value.split(",").map((candidate) => candidate.trim().split(/\s/)[0]) : [value]);
    for (const raw of urls) {
      const url = new URL(decode(raw), `${origin}/multirunner/${file.replace(/index\.html$/, "")}`);
      if (url.origin !== origin) continue;
      assert.ok(url.pathname.startsWith("/multirunner/"), `${file}: ${raw} escapes Pages base`);
      const path = `dist/${decodeURIComponent(url.pathname.slice("/multirunner/".length))}${url.pathname.endsWith("/") ? "index.html" : ""}`;
      assert.ok(existsSync(path), `${file}: missing ${raw}`);
      if (url.hash) {
        const anchors = [...read(path).matchAll(/ id="([^"]+)"/g)].map(([, id]) => decode(id));
        assert.ok(anchors.includes(decodeURIComponent(url.hash.slice(1))), `${file}: missing ${raw}`);
      }
    }
  }
});

test("built pages provide keyboard skip navigation and visible focus styling", () => {
  const globalStyles = read("src/styles/global.css");

  for (const file of pages) {
    const page = read(`dist/${file}`);
    assert.match(page, /class="skip-link" href="#main-content"/);
    assert.match(page, /<main id="main-content">/);
  }
  assert.match(globalStyles, /:focus-visible\s*\{/);
  assert.match(globalStyles, /outline:\s*3px solid var\(--orange\)/);
  assert.match(globalStyles, /max-width:\s*520px[\s\S]+nav-mobile-optional/);
  assert.match(read("src/pages/index.astro"), /\.split > \*,[\s\S]+min-width:\s*0/);
  assert.match(
    read("src/components/AgentPluginSection.astro"),
    /\.agent-intro,[\s\S]+\.skill-directory[\s\S]+min-width:\s*0/,
  );
});

test("every referenced project image and its replacement manifest exists", () => {
  const expectedImages = new Map([
    ["public/images/logo.png", [512, 512]],
    ["public/images/og.png", [1200, 630]],
    ["public/images/favicon.png", [64, 64]],
    ["src/assets/hero.png", [1536, 1024]],
  ]);

  for (const [file, expectedDimensions] of expectedImages) {
    assert.ok(existsSync(file), `Missing ${file}`);
    const image = readFileSync(file);
    assert.equal(image.subarray(1, 4).toString(), "PNG", `${file} is not PNG`);
    assert.deepEqual(
      [image.readUInt32BE(16), image.readUInt32BE(20)],
      expectedDimensions,
      `${file} dimensions changed`,
    );
  }
  assert.ok(existsSync("public/images/IMAGES.md"));

  const layout = read("src/layouts/Layout.astro");
  const home = read("src/pages/index.astro");
  assert.match(layout, /images\/og\.png/);
  assert.match(layout, /images\/favicon\.png/);
  assert.match(home, /assets\/hero\.png/);
  for (const placeholder of ["logo.svg", "og.svg", "favicon.svg", "hero-terminal.svg"]) {
    assert.equal(existsSync(`public/images/${placeholder}`), false);
  }
});
