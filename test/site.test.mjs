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
  const layout = read("src/layouts/Layout.astro");

  for (const page of pages) {
    assert.match(page, /class="skip-link" href="#main-content"/);
    assert.match(page, /<main id="main-content">/);
  }
  assert.match(layout, /:focus-visible\s*\{/);
  assert.match(layout, /outline:\s*3px solid var\(--lime\)/);
  assert.match(read("src/pages/index.astro"), /\.split > \*,[\s\S]+min-width:\s*0/);
  assert.match(
    read("src/components/AgentPluginSection.astro"),
    /\.agent-intro,[\s\S]+\.skill-directory[\s\S]+min-width:\s*0/,
  );
});

test("every referenced project image and its replacement manifest exists", () => {
  for (const file of [
    "public/images/logo.svg",
    "public/images/og.svg",
    "public/images/favicon.svg",
    "public/images/hero-terminal.svg",
    "public/images/IMAGES.md",
  ]) {
    assert.ok(existsSync(file), `Missing ${file}`);
  }

  const layout = read("src/layouts/Layout.astro");
  const home = read("src/pages/index.astro");
  assert.match(layout, /images\/logo\.svg/);
  assert.match(layout, /images\/og\.svg/);
  assert.match(layout, /images\/favicon\.svg/);
  assert.match(home, /images\/hero-terminal\.svg/);
});
