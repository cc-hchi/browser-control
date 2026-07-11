import assert from "node:assert/strict";
import test from "node:test";

import { isJsonRpcMessage, protocolName, protocolVersion } from "./index.js";

test("exports protocol identity", () => {
  assert.equal(protocolName, "browser-control");
  assert.equal(protocolVersion, "1.0");
});

test("recognizes JSON-RPC messages", () => {
  assert.equal(
    isJsonRpcMessage({ jsonrpc: "2.0", id: 1, method: "daemon.hello" }),
    true,
  );
  assert.equal(isJsonRpcMessage({ jsonrpc: "1.0", id: 1, result: {} }), false);
});
