import assert from "node:assert/strict";
import test from "node:test";

if (typeof globalThis.btoa !== "function") {
  Object.defineProperty(globalThis, "btoa", {
    value: (value: string) => Buffer.from(value, "binary").toString("base64"),
  });
}
if (typeof globalThis.atob !== "function") {
  Object.defineProperty(globalThis, "atob", {
    value: (value: string) => Buffer.from(value, "base64").toString("binary"),
  });
}

const { ChunkAssembler, encodeMessage } = await import("../src/chunk-codec.js");

test("small JSON-RPC messages remain unwrapped", async () => {
  const request = {
    jsonrpc: "2.0" as const,
    id: "1",
    method: "tab.list",
    params: {},
  };
  assert.deepEqual(await encodeMessage(request), [request]);
});

test("large Unicode messages use the Go-compatible envelope and reassemble out of order", async () => {
  const message = {
    jsonrpc: "2.0" as const,
    id: "1",
    result: { data: "你好🌏".repeat(150_000) },
  };
  const chunks = await encodeMessage(message);
  assert.ok(chunks.length > 1);
  assert.ok("__bc_chunk" in chunks[0]!);
  const assembler = new ChunkAssembler();
  let result;
  for (const chunk of chunks.toReversed())
    result = (await assembler.accept(chunk)) ?? result;
  assert.deepEqual(result, message);
});

test("invalid chunk indices are rejected", async () => {
  const assembler = new ChunkAssembler();
  await assert.rejects(
    assembler.accept({
      __bc_chunk: {
        version: 1,
        id: "x",
        index: 2,
        total: 1,
        sha256: "0".repeat(64),
        data: "e30=",
      },
    }),
  );
});

test("checksum mismatches are rejected", async () => {
  const assembler = new ChunkAssembler();
  await assert.rejects(
    assembler.accept({
      __bc_chunk: {
        version: 1,
        id: "x",
        index: 0,
        total: 1,
        sha256: "0".repeat(64),
        data: "e30=",
      },
    }),
    /checksum/,
  );
});
