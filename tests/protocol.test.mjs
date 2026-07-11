import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const methods = JSON.parse(
  await readFile(new URL("../protocol/methods.json", import.meta.url), "utf8"),
);
const schema = JSON.parse(
  await readFile(new URL("../protocol/browser-control.schema.json", import.meta.url), "utf8"),
);

test("protocol method registry contains required control surfaces", () => {
  const required = [
    "daemon.hello",
    "session.open",
    "tab.claim",
    "observation.capture",
    "locator.query",
    "action.perform",
    "browser.stop",
  ];
  for (const method of required) assert.ok(methods.publicMethods.includes(method), method);
  assert.equal(new Set(methods.publicMethods).size, methods.publicMethods.length);
});

test("bridge methods remain separate from public methods", () => {
  for (const method of methods.bridgeMethods) {
    assert.ok(method.startsWith("bridge."));
    assert.equal(methods.publicMethods.includes(method), false);
  }
});

test("wire schema includes action and error contracts", () => {
  assert.equal(schema.$schema, "https://json-schema.org/draft/2020-12/schema");
  assert.ok(schema.$defs.ActionRequest);
  assert.ok(schema.$defs.Error);
  assert.ok(schema.$defs.Locator);
});
