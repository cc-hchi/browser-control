import assert from "node:assert/strict";
import test from "node:test";
import { Window } from "happy-dom";

interface InstalledDom {
  window: Window;
  document: Document;
}

function installDom(markup = ""): InstalledDom {
  const window = new Window({ url: "https://example.test/form" });
  window.document.body.innerHTML = markup;

  Object.assign(globalThis, {
    window,
    document: window.document,
    location: window.location,
    Element: window.Element,
    HTMLElement: window.HTMLElement,
    HTMLInputElement: window.HTMLInputElement,
    HTMLTextAreaElement: window.HTMLTextAreaElement,
    HTMLSelectElement: window.HTMLSelectElement,
    HTMLButtonElement: window.HTMLButtonElement,
    MutationObserver: window.MutationObserver,
    InputEvent: window.InputEvent,
    Event: window.Event,
    MouseEvent: window.MouseEvent,
    PointerEvent: window.PointerEvent,
    KeyboardEvent: window.KeyboardEvent,
    getComputedStyle: window.getComputedStyle.bind(window),
    addEventListener: window.addEventListener.bind(window),
    removeEventListener: window.removeEventListener.bind(window),
    requestAnimationFrame: window.requestAnimationFrame.bind(window),
    cancelAnimationFrame: window.cancelAnimationFrame.bind(window),
    innerWidth: 1_024,
    innerHeight: 768,
    devicePixelRatio: 1,
    scrollX: 0,
    scrollY: 0,
  });

  return { window, document: window.document };
}

function setRect(
  element: Element,
  x = 10,
  y = 10,
  width = 120,
  height = 30,
): void {
  Object.defineProperty(element, "getBoundingClientRect", {
    configurable: true,
    value: () => ({
      x,
      y,
      width,
      height,
      top: y,
      right: x + width,
      bottom: y + height,
      left: x,
      toJSON: () => ({}),
    }),
  });
}

function setHitTarget(document: Document, target: Element | null): void {
  Object.defineProperty(document, "elementFromPoint", {
    configurable: true,
    value: () => target,
  });
}

test("capture redacts password, credit-card, and OTP values", async () => {
  installDom(`
    <input id="password" type="password" value="hunter2">
    <input id="card" autocomplete="cc-number" value="4111111111111111">
    <input id="otp" name="verification-code" autocomplete="one-time-code" value="123456">
    <input id="email" type="email" value="person@example.test">
  `);
  const { LocatorRuntime } = await import("../src/runtime.js");
  const runtime = new LocatorRuntime();

  const snapshot = runtime.capture({ includeHidden: true });
  const nodes = new Map(
    snapshot.nodes.map((node) => [node.attributes.id, node]),
  );

  for (const id of ["password", "card", "otp"]) {
    const node = nodes.get(id);
    assert.ok(node, `expected ${id} in snapshot`);
    assert.equal(node.sensitive, true);
    assert.equal(node.value, undefined);
  }
  assert.equal(nodes.get("email")?.sensitive, undefined);
  assert.equal(nodes.get("email")?.value, "person@example.test");
  for (const secret of ["hunter2", "4111111111111111", "123456"]) {
    assert.equal(snapshot.aiDom.includes(secret), false);
  }

  runtime.dispose();
});

test("snapshot refs reject detached nodes and evicted snapshots", async () => {
  const { document } = installDom('<button id="save">Save</button>');
  const { LocatorRuntime } = await import("../src/runtime.js");
  const runtime = new LocatorRuntime();

  const detachedSnapshot = runtime.capture({ includeHidden: true });
  const detachedRef = detachedSnapshot.nodes.find(
    (node) => node.attributes.id === "save",
  )?.nodeRef;
  assert.ok(detachedRef);
  document.querySelector("#save")?.remove();
  assert.throws(
    () => runtime.resolve(detachedSnapshot.snapshotId, detachedRef),
    /STALE_REFERENCE: node is detached/,
  );

  document.body.innerHTML = '<button id="new-save">Save</button>';
  const snapshots = Array.from({ length: 9 }, () =>
    runtime.capture({ includeHidden: true }),
  );
  const firstRef = snapshots[0]?.nodes[0]?.nodeRef;
  assert.ok(firstRef);
  assert.throws(
    () => runtime.resolve(snapshots[0]!.snapshotId, firstRef),
    /STALE_REFERENCE: snapshot is no longer available/,
  );
  assert.doesNotThrow(() =>
    runtime.resolve(
      snapshots.at(-1)!.snapshotId,
      snapshots.at(-1)!.nodes[0]!.nodeRef,
    ),
  );

  runtime.dispose();
});

test("actionability reports disabled, hidden, detached, and obscured targets", async () => {
  const { document } = installDom(`
    <button id="disabled" disabled>Disabled</button>
    <button id="hidden" style="display:none">Hidden</button>
    <button id="obscured">Obscured</button>
    <div id="overlay"></div>
  `);
  const { actionabilityOf } = await import("../src/runtime.js");
  const disabled = document.querySelector("#disabled")!;
  const hidden = document.querySelector("#hidden")!;
  const obscured = document.querySelector("#obscured")!;
  const overlay = document.querySelector("#overlay")!;
  [disabled, hidden, obscured, overlay].forEach((element) => setRect(element));

  setHitTarget(document, disabled);
  const disabledResult = actionabilityOf(disabled, true);
  assert.equal(disabledResult.enabled, false);
  assert.ok(disabledResult.reasons.includes("disabled"));

  setHitTarget(document, hidden);
  const hiddenResult = actionabilityOf(hidden, true);
  assert.equal(hiddenResult.visible, false);
  assert.ok(hiddenResult.reasons.includes("hidden"));

  setHitTarget(document, overlay);
  const obscuredResult = actionabilityOf(obscured, true);
  assert.equal(obscuredResult.visible, true);
  assert.equal(obscuredResult.receivesPointerEvents, false);
  assert.ok(obscuredResult.reasons.includes("obscured"));

  obscured.remove();
  const detachedResult = actionabilityOf(obscured, true);
  assert.equal(detachedResult.attached, false);
  assert.ok(detachedResult.reasons.includes("detached"));
});

test("editable values use native input and textarea setters and emit input then change", async () => {
  const { document } = installDom(
    '<form><input id="input"><textarea id="textarea"></textarea></form>',
  );
  const { setEditableValue } = await import("../src/runtime.js");
  const input = document.querySelector<HTMLInputElement>("#input")!;
  const textarea = document.querySelector<HTMLTextAreaElement>("#textarea")!;
  const events: string[] = [];
  document
    .querySelector("form")!
    .addEventListener("input", (event) =>
      events.push(`${(event.target as Element).id}:input`),
    );
  document
    .querySelector("form")!
    .addEventListener("change", (event) =>
      events.push(`${(event.target as Element).id}:change`),
    );

  setEditableValue(input, "new input");
  setEditableValue(textarea, "new textarea");

  assert.equal(input.value, "new input");
  assert.equal(textarea.value, "new textarea");
  assert.deepEqual(events, [
    "input:input",
    "input:change",
    "textarea:input",
    "textarea:change",
  ]);
});

test("native setter bypasses a React-style controlled-input instance setter", async () => {
  const { document, window } = installDom('<input id="controlled">');
  const { setEditableValue } = await import("../src/runtime.js");
  const input = document.querySelector<HTMLInputElement>("#controlled")!;
  const descriptor = Object.getOwnPropertyDescriptor(
    window.HTMLInputElement.prototype,
    "value",
  )!;
  let frameworkSetterCalls = 0;
  Object.defineProperty(input, "value", {
    configurable: true,
    get: () => descriptor.get!.call(input) as string,
    set: (value: string) => {
      frameworkSetterCalls += 1;
      descriptor.set!.call(input, value);
    },
  });

  setEditableValue(input, "updated by automation");

  assert.equal(input.value, "updated by automation");
  assert.equal(frameworkSetterCalls, 0);
});

test("contenteditable fill updates text and emits bubbling input/change events", async () => {
  const { document } = installDom(
    '<section><div id="editor" contenteditable="true">old</div></section>',
  );
  const { setEditableValue } = await import("../src/runtime.js");
  const editor = document.querySelector<HTMLElement>("#editor")!;
  const events: string[] = [];
  document
    .querySelector("section")!
    .addEventListener("input", () => events.push("input"));
  document
    .querySelector("section")!
    .addEventListener("change", () => events.push("change"));

  setEditableValue(editor, "new content");

  assert.equal(editor.textContent, "new content");
  assert.deepEqual(events, ["input", "change"]);
});

test("runtime traverses open shadow DOM and rejects ambiguous strict locators", async () => {
  const { document } = installDom(
    '<div id="host"></div><button>Duplicate</button><button>Duplicate</button>',
  );
  const { LocatorRuntime } = await import("../src/runtime.js");
  const host = document.querySelector("#host")!;
  const shadowRoot = host.attachShadow({ mode: "open" });
  shadowRoot.innerHTML = '<button aria-label="Shadow save">Save</button>';
  const runtime = new LocatorRuntime();

  const shadowMatches = runtime.query({
    by: "role",
    role: "button",
    name: { text: "Shadow save", exact: true },
  });
  assert.equal(shadowMatches.length, 1);
  await assert.rejects(
    runtime.prepare({
      locator: {
        by: "role",
        role: "button",
        name: { text: "Duplicate", exact: true },
      },
    }),
    /LOCATOR_AMBIGUOUS: 2 matching elements/,
  );

  runtime.dispose();
});

test("element identity is stable for one node and rejects an equivalent replacement", async () => {
  const { document } = installDom('<button id="save">Save</button>');
  const { LocatorRuntime } = await import("../src/runtime.js");
  const runtime = new LocatorRuntime();
  const locator = { by: "css" as const, value: "#save" };
  const original = document.querySelector("#save")!;
  setRect(original);
  setHitTarget(document, original);

  const first = runtime.query(locator)[0]!;
  const second = runtime.query(locator)[0]!;
  assert.equal(first.elementIdentity, second.elementIdentity);

  const replacement = original.cloneNode(true) as Element;
  original.replaceWith(replacement);
  setRect(replacement);
  setHitTarget(document, replacement);
  const replaced = runtime.query(locator)[0]!;
  assert.notEqual(replaced.elementIdentity, first.elementIdentity);
  await assert.rejects(
    runtime.perform({ locator }, { type: "click" }, first.elementIdentity),
    /STALE_REFERENCE: target element identity changed/,
  );

  runtime.dispose();
});

test("marked secure input is redacted from snapshots and content exports", async () => {
  const { document } = installDom(
    '<input id="plain" value="plain-secret"><div id="editor" contenteditable="true">editor-secret</div>',
  );
  const { LocatorRuntime } = await import("../src/runtime.js");
  const runtime = new LocatorRuntime();
  const input = document.querySelector("#plain")!;
  const editor = document.querySelector("#editor")!;

  const inputDescription = runtime.query({ by: "css", value: "#plain" })[0]!;
  const editorDescription = runtime.query({ by: "css", value: "#editor" })[0]!;
  runtime.markSensitive(
    { locator: { by: "css", value: "#plain" } },
    inputDescription.elementIdentity,
  );
  runtime.markSensitive(
    { locator: { by: "css", value: "#editor" } },
    editorDescription.elementIdentity,
  );

  const snapshot = runtime.capture({ includeHidden: true });
  assert.equal(JSON.stringify(snapshot).includes("plain-secret"), false);
  assert.equal(JSON.stringify(snapshot).includes("editor-secret"), false);
  for (const format of ["html", "text", "markdown"] as const) {
    const exported = runtime.exportContent(format);
    assert.equal(exported.includes("plain-secret"), false);
    assert.equal(exported.includes("editor-secret"), false);
    assert.equal(exported.includes("data-browser-control-sensitive"), false);
  }

  runtime.setSensitiveMask(true);
  assert.equal((input as HTMLElement).style.color, "transparent");
  assert.equal((editor as HTMLElement).style.color, "transparent");
  runtime.setSensitiveMask(false);
  assert.equal(input.hasAttribute("style"), false);
  assert.equal(editor.hasAttribute("style"), false);
  runtime.dispose();
});
