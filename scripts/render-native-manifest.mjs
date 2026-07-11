#!/usr/bin/env node

import { createHash } from "node:crypto";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, resolve } from "node:path";

const HOST_NAME = "com.browser_control.native_host";
const DESCRIPTION = "browser-control native messaging bridge";

function usage() {
  process.stderr.write(
    "usage: render-native-manifest.mjs --extension-manifest PATH --host-path PATH [--output PATH]\n" +
      "       render-native-manifest.mjs --check-owned MANIFEST --install-root PATH\n",
  );
}

function fail(message, exitCode = 1) {
  process.stderr.write(`${message}\n`);
  process.exit(exitCode);
}

function parseArgs(argv) {
  const values = new Map();
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (!argument.startsWith("--")) fail(`unexpected argument: ${argument}`, 2);
    const value = argv[index + 1];
    if (!value || value.startsWith("--")) fail(`missing value for ${argument}`, 2);
    if (values.has(argument)) fail(`duplicate argument: ${argument}`, 2);
    values.set(argument, value);
    index += 1;
  }
  return values;
}

function extensionId(publicKey) {
  let keyBytes;
  try {
    keyBytes = Buffer.from(publicKey, "base64");
  } catch {
    fail("extension manifest key is not valid base64");
  }
  if (keyBytes.length === 0 || keyBytes.toString("base64").replace(/=+$/, "") !== publicKey.replace(/\s+/g, "").replace(/=+$/, "")) {
    fail("extension manifest key is not valid base64");
  }
  const digest = createHash("sha256").update(keyBytes).digest();
  const alphabet = "abcdefghijklmnop";
  let id = "";
  for (const byte of digest.subarray(0, 16)) {
    id += alphabet[byte >> 4];
    id += alphabet[byte & 0x0f];
  }
  return id;
}

async function readJSON(path, description) {
  let text;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    fail(`cannot read ${description} at ${path}: ${error.message}`);
  }
  try {
    return JSON.parse(text);
  } catch (error) {
    fail(`${description} is not valid JSON: ${error.message}`);
  }
}

async function checkOwned(args) {
  const manifestPath = args.get("--check-owned");
  const installRoot = args.get("--install-root");
  if (!manifestPath || !installRoot || args.size !== 2) {
    usage();
    process.exit(2);
  }
  if (!isAbsolute(manifestPath)) fail("--check-owned must be absolute", 2);
  if (!isAbsolute(installRoot)) fail("--install-root must be absolute", 2);
  const manifest = await readJSON(manifestPath, "native messaging manifest");
  const expectedHostPath = resolve(installRoot, "bin", "browser-native-host");
  const owned =
    manifest.name === HOST_NAME &&
    manifest.description === DESCRIPTION &&
    manifest.type === "stdio" &&
    typeof manifest.path === "string" &&
    isAbsolute(manifest.path) &&
    resolve(manifest.path) === expectedHostPath &&
    Array.isArray(manifest.allowed_origins) &&
    manifest.allowed_origins.length === 1 &&
    manifest.allowed_origins[0] === "chrome-extension://bfnlcmlokggpcgncalophomjeencbbli/";
  process.exit(owned ? 0 : 1);
}

async function render(args) {
  const extensionManifestPath = args.get("--extension-manifest");
  const hostPath = args.get("--host-path");
  const outputPath = args.get("--output");
  const expectedSize = outputPath ? 3 : 2;
  if (!extensionManifestPath || !hostPath || args.size !== expectedSize) {
    usage();
    process.exit(2);
  }
  if (!isAbsolute(hostPath)) fail("--host-path must be absolute", 2);

  const extensionManifest = await readJSON(extensionManifestPath, "extension manifest");
  if (typeof extensionManifest.key !== "string" || extensionManifest.key.trim() === "") {
    fail("extension manifest does not contain a stable public key");
  }
  const id = extensionId(extensionManifest.key.trim());
  const nativeManifest = {
    name: HOST_NAME,
    description: DESCRIPTION,
    path: resolve(hostPath),
    type: "stdio",
    allowed_origins: [`chrome-extension://${id}/`],
  };
  const rendered = `${JSON.stringify(nativeManifest, null, 2)}\n`;

  if (!outputPath) {
    process.stdout.write(rendered);
    return;
  }
  if (!isAbsolute(outputPath)) fail("--output must be absolute", 2);
  await mkdir(dirname(outputPath), { recursive: true, mode: 0o700 });
  const temporaryPath = `${outputPath}.tmp-${process.pid}`;
  await writeFile(temporaryPath, rendered, { encoding: "utf8", mode: 0o600 });
  await rename(temporaryPath, outputPath);
}

const args = parseArgs(process.argv.slice(2));
if (args.has("--check-owned")) await checkOwned(args);
await render(args);
