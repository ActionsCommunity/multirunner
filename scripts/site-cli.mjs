import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const cwd = fileURLToPath(new URL("../", import.meta.url));

export function withCLI(callback) {
  const dir = mkdtempSync(join(tmpdir(), "multirunner-site-"));
  const bin = join(dir, process.platform === "win32" ? "multirunner.exe" : "multirunner");
  try {
    execFileSync("go", ["build", "-buildvcs=false", "-o", bin, "./cmd/multirunner"], {
      cwd,
      stdio: "inherit",
    });
    return callback((args, options = {}) => execFileSync(bin, args, {
      cwd: dir,
      encoding: "utf8",
      ...options,
    }).replaceAll("\r\n", "\n").trimEnd(), dir);
  } finally {
    rmSync(dir, { force: true, recursive: true });
  }
}

export function publicHelp(cli) {
  function visit(path) {
    const help = cli([...path, "--help"]);
    const list = help.match(/^Available Commands:\n((?: {2}\S[^\n]*\n?)+)/m);
    if (help.includes("Available Commands:") && !list) {
      throw new Error(`Could not parse subcommands of multirunner ${path.join(" ")}`);
    }
    return [
      { path, help },
      ...(list ? [...list[1].matchAll(/^ {2}(\S+)/gm)]
        .flatMap(([, name]) => visit([...path, name])) : []),
    ];
  }
  return visit([]);
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const entries = withCLI(publicHelp);
  const dir = new URL("../.astro/", import.meta.url);
  mkdirSync(dir, { recursive: true });
  writeFileSync(new URL("site-cli.json", dir), JSON.stringify(entries));
}
