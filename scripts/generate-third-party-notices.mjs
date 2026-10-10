import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDir, "..");
const outputPath = path.join(repositoryRoot, "THIRD_PARTY_NOTICES.md");
const checkOnly = process.argv.includes("--check");
const isWindows = process.platform === "win32";

function run(command, args, cwd = repositoryRoot) {
  const result = spawnSync(command, args, {
    cwd,
    encoding: "utf8",
    env: process.env,
  });
  if (result.status !== 0) {
    throw new Error(
      `${command} ${args.join(" ")} failed:\n${result.stderr || result.stdout}`,
    );
  }
  return result.stdout;
}

function parseJSONStream(input) {
  const values = [];
  let start = -1;
  let depth = 0;
  let quoted = false;
  let escaped = false;
  for (let index = 0; index < input.length; index += 1) {
    const character = input[index];
    if (start < 0) {
      if (character === "{") {
        start = index;
        depth = 1;
      }
      continue;
    }
    if (quoted) {
      if (escaped) {
        escaped = false;
      } else if (character === "\\") {
        escaped = true;
      } else if (character === '"') {
        quoted = false;
      }
      continue;
    }
    if (character === '"') {
      quoted = true;
    } else if (character === "{") {
      depth += 1;
    } else if (character === "}") {
      depth -= 1;
      if (depth === 0) {
        values.push(JSON.parse(input.slice(start, index + 1)));
        start = -1;
      }
    }
  }
  if (start >= 0 || quoted || depth !== 0) {
    throw new Error("unterminated JSON value in command output");
  }
  return values;
}

function normalizeText(value) {
  return (
    value
      .replaceAll("\r\n", "\n")
      .replaceAll("\r", "\n")
      .split("\n")
      .map((line) => line.trimEnd())
      .join("\n")
      .trimEnd() + "\n"
  );
}

function licenseFiles(directory) {
  const names = fs
    .readdirSync(directory, { withFileTypes: true })
    .filter(
      (entry) =>
        entry.isFile() &&
        /^(license|licence|copying|notice)([._-].*)?$/i.test(entry.name),
    )
    .map((entry) => entry.name)
    .sort((left, right) => left.localeCompare(right));
  return names.map((name) => ({
    name,
    text: normalizeText(fs.readFileSync(path.join(directory, name), "utf8")),
  }));
}

function detectedLicense(files) {
  const text = files.map((file) => file.text).join("\n");
  if (/Mozilla Public License,? version 2\.0/i.test(text)) return "MPL-2.0";
  if (/Apache License(?:,)?\s+(?:Version )?2\.0/i.test(text)) return "Apache-2.0";
  if (/Permission is hereby granted, free of charge/i.test(text)) return "MIT";
  if (/Redistribution and use in source and binary forms/i.test(text)) {
    return /Neither the name|Neither the names/i.test(text)
      ? "BSD-3-Clause"
      : "BSD-2-Clause";
  }
  if (/ISC License/i.test(text)) return "ISC";
  if (
    /origin of this software must not be misrepresented/i.test(text) &&
    /Altered source versions must be plainly marked/i.test(text)
  ) {
    return "Zlib";
  }
  if (/Blue Oak Model License/i.test(text)) return "BlueOak-1.0.0";
  if (/Creative Commons Attribution 4\.0/i.test(text)) return "CC-BY-4.0";
  if (/Creative Commons Zero/i.test(text)) return "CC0-1.0";
  return "See bundled license text";
}

function goDependencies() {
  run(process.env.GO || "go", ["mod", "download", "all"]);
  const modules = parseJSONStream(
    run(process.env.GO || "go", ["list", "-m", "-json", "all"]),
  );
  return modules
    .filter((module) => !module.Main)
    .map((module) => {
      const effective = module.Replace || module;
      if (!effective.Dir) {
        throw new Error(`Go module ${module.Path} has no downloaded directory`);
      }
      const files = licenseFiles(effective.Dir);
      if (files.length === 0) {
        throw new Error(`Go module ${module.Path}@${module.Version} has no license file`);
      }
      return {
        ecosystem: "Go",
        name: module.Path,
        version: module.Version || effective.Version || "replacement",
        license: detectedLicense(files),
        source: `https://pkg.go.dev/${module.Path}@${module.Version}`,
        files,
      };
    });
}

function npmDependencies() {
  const pnpm = process.env.PNPM || "pnpm";
  const pnpmArgs = [
    "--dir",
    "apps/console",
    "list",
    "--prod",
    "--json",
    "--depth",
    "Infinity",
  ];
  const command = isWindows ? process.execPath : pnpm;
  const args = isWindows
    ? [
        path.join(
          path.dirname(process.execPath),
          "node_modules",
          "corepack",
          "dist",
          "pnpm.js",
        ),
        ...pnpmArgs,
      ]
    : pnpmArgs;
  const roots = JSON.parse(
    run(command, args, repositoryRoot),
  );
  const dependencies = new Map();
  function visit(node) {
    for (const dependency of Object.values(node.dependencies || {})) {
      const key = `${dependency.from || dependency.name}@${dependency.version}`;
      if (!dependencies.has(key)) {
        const manifest = JSON.parse(
          fs.readFileSync(path.join(dependency.path, "package.json"), "utf8"),
        );
        const files = licenseFiles(dependency.path);
        if (files.length === 0) {
          throw new Error(`npm package ${manifest.name}@${manifest.version} has no license file`);
        }
        const declared =
          typeof manifest.license === "string"
            ? manifest.license
            : detectedLicense(files);
        dependencies.set(key, {
          ecosystem: "npm",
          name: manifest.name,
          version: manifest.version,
          license: declared,
          source: dependency.resolved || `https://www.npmjs.com/package/${manifest.name}`,
          files,
        });
      }
      visit(dependency);
    }
  }
  for (const root of roots) visit(root);
  return [...dependencies.values()];
}

function tableValue(value) {
  return value.replaceAll("|", "\\|").replaceAll("\n", " ");
}

function generate() {
  const dependencies = [...goDependencies(), ...npmDependencies()].sort((left, right) =>
    `${left.ecosystem}:${left.name}@${left.version}`.localeCompare(
      `${right.ecosystem}:${right.name}@${right.version}`,
    ),
  );
  const lines = [
    "# Third-Party Notices",
    "",
    "<!-- Generated by scripts/generate-third-party-notices.mjs. Do not edit manually. -->",
    "",
    "This deterministic bundle records the license and notice texts shipped with the",
    "resolved Go module graph and the production browser dependency graph.",
    "",
    "Regenerate with `node scripts/generate-third-party-notices.mjs` and verify with",
    "`node scripts/generate-third-party-notices.mjs --check`.",
    "",
    "## Inventory",
    "",
    "| Ecosystem | Component | Version | Declared license |",
    "| --- | --- | --- | --- |",
  ];
  for (const dependency of dependencies) {
    lines.push(
      `| ${tableValue(dependency.ecosystem)} | ${tableValue(dependency.name)} | ` +
        `${tableValue(dependency.version)} | ${tableValue(dependency.license)} |`,
    );
  }
  lines.push("", "## License and notice texts", "");
  for (const dependency of dependencies) {
    lines.push(
      `### ${dependency.ecosystem}: ${dependency.name}@${dependency.version}`,
      "",
      `Declared license: \`${dependency.license}\``,
      "",
      `Source: ${dependency.source}`,
      "",
    );
    for (const file of dependency.files) {
      lines.push(
        `#### ${file.name}`,
        "",
        "````text",
        file.text.trimEnd(),
        "````",
        "",
      );
    }
  }
  return `${lines.join("\n").trimEnd()}\n`;
}

const generated = generate();
if (checkOnly) {
  const existing = fs.existsSync(outputPath) ? normalizeText(fs.readFileSync(outputPath, "utf8")) : "";
  if (existing !== generated) {
    console.error("THIRD_PARTY_NOTICES.md is stale; regenerate it.");
    process.exit(1);
  }
  console.log("THIRD_PARTY_NOTICES.md is current.");
} else {
  fs.writeFileSync(outputPath, generated, "utf8");
  console.log(`Wrote ${path.relative(repositoryRoot, outputPath)}.`);
}
