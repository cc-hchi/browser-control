#!/usr/bin/env node

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

const methods = JSON.parse(await readFile(new URL("../protocol/methods.json", import.meta.url)));
const schema = JSON.parse(await readFile(new URL("../protocol/browser-control.schema.json", import.meta.url)));

assert.equal(methods.name, "browser-control");
assert.match(methods.protocolVersion, /^\d+\.\d+$/);
assert.equal(new Set(methods.publicMethods).size, methods.publicMethods.length);
assert.equal(new Set(methods.bridgeMethods).size, methods.bridgeMethods.length);
assert.ok(schema.$defs.JsonRpcRequest);
assert.ok(schema.$defs.ActionRequest);
process.stdout.write("protocol registry and schema are structurally valid\n");
