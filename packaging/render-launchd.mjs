#!/usr/bin/env node

import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { delimiter, dirname, isAbsolute } from "node:path";

const required = [
  "template",
  "output",
  "label",
  "browserd",
  "state-dir",
  "socket",
  "bridge-socket",
  "bridge-token-file",
  "artifact-dir",
  "upload-roots",
  "home",
  "stdout-log",
  "stderr-log",
];

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(2);
}

const values = new Map();
const argv = process.argv.slice(2);
for (let index = 0; index < argv.length; index += 2) {
  const option = argv[index];
  const value = argv[index + 1];
  if (!option?.startsWith("--") || value === undefined) fail("invalid launchd renderer arguments");
  const name = option.slice(2);
  if (!required.includes(name)) fail(`unknown option: ${option}`);
  if (values.has(name)) fail(`duplicate option: ${option}`);
  values.set(name, value);
}
for (const name of required) {
  if (!values.has(name)) fail(`missing --${name}`);
}
if (!values.get("label")) fail("--label must not be empty");
for (const name of ["template", "output", "browserd", "state-dir", "socket", "bridge-socket", "bridge-token-file", "artifact-dir", "home", "stdout-log", "stderr-log"]) {
  if (!isAbsolute(values.get(name))) fail(`--${name} must be absolute`);
}

const uploadRoots = values.get("upload-roots").split(delimiter);
if (!uploadRoots.length || uploadRoots.some((root) => !root || !isAbsolute(root))) {
  fail(`--upload-roots must be a ${JSON.stringify(delimiter)}-separated list of absolute paths`);
}

function escapeXML(value) {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");
}

const replacements = {
  LABEL: values.get("label"),
  BROWSERD: values.get("browserd"),
  STATE_DIR: values.get("state-dir"),
  SOCKET: values.get("socket"),
  BRIDGE_SOCKET: values.get("bridge-socket"),
  BRIDGE_TOKEN_FILE: values.get("bridge-token-file"),
  ARTIFACT_DIR: values.get("artifact-dir"),
  HOME: values.get("home"),
  STDOUT_LOG: values.get("stdout-log"),
  STDERR_LOG: values.get("stderr-log"),
};

let rendered = await readFile(values.get("template"), "utf8");
for (const [token, value] of Object.entries(replacements)) {
  rendered = rendered.replaceAll(`@@${token}@@`, escapeXML(value));
}
const uploadRootArguments = uploadRoots
  .map((root) => `    <string>--upload-root</string>\n    <string>${escapeXML(root)}</string>`)
  .join("\n");
rendered = rendered.replaceAll("@@UPLOAD_ROOT_ARGUMENTS@@", uploadRootArguments);
const unresolved = rendered.match(/@@[A-Z_]+@@/);
if (unresolved) fail(`unresolved template token: ${unresolved[0]}`);

const output = values.get("output");
await mkdir(dirname(output), { recursive: true, mode: 0o700 });
const temporary = `${output}.tmp-${process.pid}`;
await writeFile(temporary, rendered, { encoding: "utf8", mode: 0o600 });
await rename(temporary, output);
