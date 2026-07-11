import { readdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { build } from "esbuild";
import { spawn } from "node:child_process";

const outdir = resolve(".test-dist");
await rm(outdir, { recursive: true, force: true });
await build({
  entryPoints: ["test/*.test.ts"],
  bundle: true,
  format: "esm",
  platform: "node",
  target: "node22",
  outdir,
  packages: "external",
});
const tests = (await readdir(outdir))
  .filter((file) => file.endsWith(".test.js"))
  .map((file) => resolve(outdir, file));
const child = spawn(process.execPath, ["--test", ...tests], {
  stdio: "inherit",
});
const exitCode = await new Promise((resolveExit) =>
  child.once("exit", (code) => resolveExit(code ?? 1)),
);
await rm(outdir, { recursive: true, force: true });
process.exitCode = exitCode;
