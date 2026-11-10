import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

const read = (path) => readFileSync(path, "utf8").replaceAll("\r\n", "\n");

function publicCommandNamesFromGo() {
  const source = [
    read("cmd/multirunner/main.go"),
    read("cmd/multirunner/detect.go"),
  ].join("\n");
  const names = [...source.matchAll(/\bUse:\s+"([^"]+)"/g)]
    .map((match) => match[1].split(/\s/)[0])
    .filter((name) => name !== "multirunner" && !name.startsWith("_"));

  return [...new Set([...names, "completion", "help"])].sort();
}

function publicOptionNamesFromReference() {
  const reference = read("skills/docs/cli-reference.md");
  const publicCommands = reference
    .split("## Public commands", 2)[1]
    .split("## Hidden QEMU developer helpers", 1)[0];

  return [...new Set(publicCommands.match(/--[a-z][a-z0-9-]*/g))].sort();
}

test("Astro targets the multirunner project path", () => {
  const config = read("astro.config.mjs");

  assert.match(config, /site:\s*"https:\/\/actionscommunity\.github\.io"/);
  assert.match(config, /base:\s*"\/multirunner\/"/);
});

test("Pages workflow deploys main with current first-party actions", () => {
  const workflow = read(".github/workflows/deploy.yml");

  assert.match(workflow, /branches:\s*\[main\]/);
  assert.match(
    workflow,
    /run:\s+npm ci --ignore-scripts --no-audit --no-fund/,
  );
  assert.match(workflow, /withastro\/action@[a-f0-9]+\s+# v6\.1\.2/);
  assert.match(workflow, /actions\/deploy-pages@[a-f0-9]+\s+# v5\.0\.1/);
  assert.match(workflow, /build-cmd:\s+npm test/);
  assert.match(workflow, /pages:\s+write/);
  assert.match(workflow, /id-token:\s+write/);
  assert.doesNotMatch(workflow, /__[A-Z0-9_]+__/);
});

test("pull request CI runs the complete site test command", () => {
  const workflow = read(".github/workflows/test.yml");
  const npmConfig = read(".npmrc");
  const packageJson = JSON.parse(read("package.json"));

  assert.match(workflow, /permissions:\n  contents:\s+read/);
  assert.match(workflow, /\n  site:\n/);
  assert.match(workflow, /site:\n    runs-on: ubuntu-latest\n    timeout-minutes: 10/);
  assert.match(workflow, /node-version:\s+24/);
  assert.match(workflow, /run:\s+npm ci --no-audit --no-fund/);
  assert.match(workflow, /run:\s+npm test/);
  assert.match(npmConfig, /^strict-allow-scripts=true$/m);
  assert.deepEqual(packageJson.allowScripts, {
    "esbuild@0.28.2": true,
    "fsevents@2.3.3": true,
  });
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
  assert.doesNotMatch(source, /download (?:a release|multirunner)/i);
  assert.doesNotMatch(source, /href=\{releasesUrl\}/);
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

test("built pages exist and expose every documented public command", () => {
  assert.ok(existsSync("dist/index.html"));
  assert.ok(existsSync("dist/commands/index.html"));

  const commandsPage = read("dist/commands/index.html");
  const documentedCommands = [
    ...commandsPage.matchAll(/<section id="([^"]+)" class="command"/g),
  ].map((match) => match[1]).sort();

  assert.deepEqual(documentedCommands, publicCommandNamesFromGo());
  for (const option of publicOptionNamesFromReference()) {
    assert.ok(commandsPage.includes(option), `Missing documented option ${option}`);
  }

  const versions = JSON.parse(read("images/versions.json"));
  assert.ok(
    commandsPage.includes(`Current default: ${versions.minimal.runner.version}`),
    "Runner version does not match images/versions.json",
  );
  assert.match(
    commandsPage,
    /https:\/\/github\.com\/actionscommunity\/multirunner\/blob\/main\/skills\/docs\/cli-reference\.md/,
  );
});

test("built root-relative links retain the GitHub project prefix", () => {
  const pages = [read("dist/index.html"), read("dist/commands/index.html")];

  for (const page of pages) {
    const rootRelativeUrls = [
      ...page.matchAll(/(?:href|src)="(\/[^"#?]*)/g),
    ].map((match) => match[1]);

    assert.ok(rootRelativeUrls.length > 0);
    assert.ok(
      rootRelativeUrls.every((url) => url.startsWith("/multirunner/")),
      `Found URL without /multirunner/ prefix: ${rootRelativeUrls.join(", ")}`,
    );
  }
});

test("built pages provide keyboard skip navigation and visible focus styling", () => {
  const pages = [read("dist/index.html"), read("dist/commands/index.html")];
  const globalStyles = read("src/styles/global.css");

  for (const page of pages) {
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
