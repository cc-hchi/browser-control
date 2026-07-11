#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";

const manifestPath = process.argv[2];
if (!manifestPath) {
  process.stderr.write("usage: extension-id.mjs /absolute/path/to/manifest.json\n");
  process.exit(2);
}

const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
if (typeof manifest.key !== "string" || manifest.key.length === 0) {
  process.stderr.write("manifest does not contain a stable public key\n");
  process.exit(1);
}

const digest = createHash("sha256").update(Buffer.from(manifest.key, "base64")).digest();
const alphabet = "abcdefghijklmnop";
let id = "";
for (const byte of digest.subarray(0, 16)) {
  id += alphabet[byte >> 4];
  id += alphabet[byte & 0x0f];
}
process.stdout.write(`${id}\n`);
