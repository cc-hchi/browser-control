import assert from "node:assert/strict";
import test from "node:test";
import {
  sanitizeAccessibilitySnapshot,
  sanitizeDomSnapshot,
} from "../src/redaction.js";

function fixtureSnapshot(): Record<string, unknown> {
  return {
    strings: [
      "#document",
      "HTML",
      "BODY",
      "INPUT",
      "",
      "type",
      "password",
      "value",
      "hunter2",
      "TEXTAREA",
      "#text",
      "otp123",
      "data-browser-control-sensitive",
      "true",
      "prefix hunter2 suffix",
      "DIV",
      "safe text",
    ],
    documents: [
      {
        nodes: {
          parentIndex: [-1, 0, 1, 2, 2, 4, 2, 6],
          nodeName: [0, 1, 2, 3, 9, 10, 15, 10],
          nodeValue: [-1, 4, 4, 4, 4, 11, 4, 14],
          attributes: [[], [], [], [5, 6, 7, 8], [], [], [12, 13], []],
          inputValue: { index: [3], value: [8] },
          textValue: { index: [4], value: [11] },
        },
        layout: {
          nodeIndex: [3, 5, 7],
          text: [8, 11, 14],
        },
      },
    ],
  };
}

test("DOM snapshot sanitizer removes live form values and marked sensitive subtrees", () => {
  const original = fixtureSnapshot();
  const sanitized = sanitizeDomSnapshot(original) as Record<string, unknown>;
  const encoded = JSON.stringify(sanitized);
  for (const secret of ["hunter2", "otp123"]) {
    assert.equal(encoded.includes(secret), false);
  }
  assert.equal(encoded.includes("safe text"), true);
  assert.equal(JSON.stringify(original).includes("hunter2"), true);
});

test("DOM snapshot sanitizer fails closed on malformed rare string data", () => {
  const malformed = fixtureSnapshot();
  const document = (malformed.documents as Array<Record<string, unknown>>)[0]!;
  (document.nodes as Record<string, unknown>).inputValue = {
    index: [3, 4],
    value: [8],
  };
  assert.throws(() => sanitizeDomSnapshot(malformed), /REDACTION_FAILED/);
});

test("accessibility sanitizer removes editable values", () => {
  const tree = {
    nodes: [
      {
        nodeId: "input",
        childIds: ["text"],
        role: { type: "role", value: "textbox" },
        name: { type: "computedString", value: "One-time code" },
        value: { type: "string", value: "otp123" },
      },
      {
        nodeId: "text",
        role: { type: "internalRole", value: "StaticText" },
        name: {
          type: "computedString",
          value: "otp123",
          sources: [
            { type: "contents", value: { type: "computedString", value: "otp123" } },
          ],
        },
      },
      {
        role: { type: "role", value: "heading" },
        name: { type: "computedString", value: "Safe heading" },
      },
    ],
  };
  const sanitized = sanitizeAccessibilitySnapshot(tree);
  const encoded = JSON.stringify(sanitized);
  assert.equal(encoded.includes("otp123"), false);
  assert.equal(encoded.includes("Safe heading"), true);
});
