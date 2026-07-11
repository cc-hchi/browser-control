import { cp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const outdir = resolve(root, "dist");
await rm(outdir, { recursive: true, force: true });
await mkdir(outdir, { recursive: true });

await build({
  absWorkingDir: root,
  entryPoints: {
    background: "src/background.ts",
    popup: "src/popup.ts",
    offscreen: "src/offscreen.ts",
    "secure-input": "src/secure-input.ts",
    locator: "src/locator-entry.ts",
  },
  bundle: true,
  format: "esm",
  target: "chrome125",
  outdir,
  sourcemap: true,
  legalComments: "none",
});

for (const file of [
  "popup.html",
  "popup.css",
  "offscreen.html",
  "secure-input.html",
  "secure-input.css",
]) {
  await cp(resolve(root, "static", file), resolve(outdir, file));
}

const manifest = JSON.parse(
  await readFile(resolve(root, "manifest.json"), "utf8"),
);
const overrideKey = process.env.BROWSER_CONTROL_EXTENSION_PUBLIC_KEY?.trim();
if (overrideKey) manifest.key = overrideKey;
await writeFile(
  resolve(outdir, "manifest.json"),
  `${JSON.stringify(manifest, null, 2)}\n`,
);
