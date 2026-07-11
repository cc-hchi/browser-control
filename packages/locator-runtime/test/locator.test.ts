import assert from "node:assert/strict";
import test from "node:test";
import { Window } from "happy-dom";

test("semantic locators traverse open shadow roots", async () => {
  const window = new Window({ url: "https://example.test" });
  Object.assign(globalThis, {
    Element: window.Element,
    HTMLInputElement: window.HTMLInputElement,
    HTMLTextAreaElement: window.HTMLTextAreaElement,
    HTMLSelectElement: window.HTMLSelectElement,
  });
  const { queryLocator } = await import("../src/locator.js");
  window.document.body.innerHTML =
    '<button aria-label="Save changes">icon</button><div id="host"></div>';
  const root = window.document
    .querySelector("#host")!
    .attachShadow({ mode: "open" });
  root.innerHTML = '<input data-testid="email" placeholder="Email address">';

  assert.equal(
    queryLocator(window.document, { by: "role", role: "button", name: "Save" })
      .length,
    1,
  );
  assert.equal(
    queryLocator(window.document, { by: "testId", value: "email" }).length,
    1,
  );
  assert.equal(
    queryLocator(window.document, { by: "placeholder", value: "email" }).length,
    1,
  );
});

test("locator combinators scope, intersect, union, and select by index", async () => {
  const window = new Window();
  Object.assign(globalThis, { Element: window.Element });
  const { queryLocator } = await import("../src/locator.js");
  window.document.body.innerHTML = `
    <section data-testid="main"><button class="primary">Send now</button><button>Cancel</button></section>
    <section><button class="primary">Other</button></section>`;

  const result = queryLocator(window.document, {
    by: "role",
    role: "button",
    scope: { by: "testId", value: "main" },
    and: { by: "css", value: ".primary" },
    hasText: "send",
  });
  assert.equal(result.length, 1);
  assert.equal(result[0]?.textContent, "Send now");

  const union = queryLocator(window.document, {
    by: "text",
    value: "Cancel",
    or: { by: "text", value: "Other" },
    index: 1,
  });
  assert.equal(union.length, 1);
  assert.equal(union[0]?.textContent, "Other");
});

test("label locator returns the labelled control, not its label element", async () => {
  const window = new Window();
  Object.assign(globalThis, {
    Element: window.Element,
    HTMLInputElement: window.HTMLInputElement,
    HTMLTextAreaElement: window.HTMLTextAreaElement,
    HTMLSelectElement: window.HTMLSelectElement,
  });
  const { queryLocator } = await import("../src/locator.js");
  window.document.body.innerHTML = '<label>Email <input name="email"></label>';

  const result = queryLocator(window.document, {
    by: "label",
    value: "Email",
    exact: true,
  });
  assert.equal(result.length, 1);
  assert.equal(result[0]?.tagName, "INPUT");
});
