import assert from "node:assert/strict";
import test from "node:test";
import { locatorFrom } from "../src/locator-input.js";

test("css locator accepts value and the conventional selector alias", () => {
  assert.equal(locatorFrom({ by: "css", value: "#title" }).value, "#title");
  const aliased = locatorFrom({ by: "css", selector: "#title" });
  assert.equal(aliased.value, "#title");
  assert.equal("selector" in aliased, false);
});

test("unknown locator fields are rejected instead of silently matching nothing", () => {
  assert.throws(
    () => locatorFrom({ by: "role", role: "button", label: "Save" }),
    /locator has unsupported field\(s\): label/,
  );
});

test("missing or invalid locator kind is rejected", () => {
  assert.throws(() => locatorFrom({ value: "#x" }), /locator\.by is required/);
  assert.throws(
    () => locatorFrom({ by: "xpath", value: "//a" }),
    /locator\.by must be one of/,
  );
});

test("value-based locators require a non-empty value", () => {
  assert.throws(
    () => locatorFrom({ by: "css" }),
    /locator\.value is required for by: "css"/,
  );
  assert.throws(
    () => locatorFrom({ by: "text", value: "" }),
    /locator\.value is required/,
  );
});

test("nested locators and frame paths are validated with a precise path", () => {
  assert.throws(
    () =>
      locatorFrom({
        by: "role",
        role: "button",
        scope: { by: "css", selectr: "#dialog" },
      }),
    /locator\.scope has unsupported field\(s\): selectr/,
  );
  assert.throws(
    () => locatorFrom({ by: "css", value: "#a", framePath: [{ by: "css" }] }),
    /locator\.framePath\[0\]\.value is required/,
  );
  const nested = locatorFrom({
    by: "role",
    role: "textbox",
    scope: { by: "css", selector: "#dialog" },
  });
  assert.equal(nested.scope?.value, "#dialog");
});
