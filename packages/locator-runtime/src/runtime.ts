import {
  accessibleName,
  collectElements,
  elementText,
  implicitRole,
  normalizeText,
  queryLocator,
} from "./locator.js";
import type {
  ActionResult,
  Actionability,
  Locator,
  LocatorRuntimeApi,
  NodeDescription,
  PageAsset,
  Rect,
  RuntimeAction,
  RuntimeSnapshot,
  RuntimeViewport,
  SnapshotOptions,
} from "./types.js";

const VERSION = "1.0.0";
const MAX_SNAPSHOTS = 8;
const SENSITIVE_ATTRIBUTE = "data-browser-control-sensitive";
const REDACTED_TEXT = "[REDACTED]";

interface SnapshotState {
  refs: Map<string, Element>;
  reverseRefs: Map<Element, string>;
}

function randomId(prefix: string): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return `${prefix}_${Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

function rectOf(element: Element): Rect {
  const rect = element.getBoundingClientRect();
  return { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
}

function topRectOf(element: Element): { rect?: Rect; translated: boolean } {
  const initial = element.getBoundingClientRect();
  const rect = {
    x: initial.x,
    y: initial.y,
    width: initial.width,
    height: initial.height,
  };
  let current: Window = element.ownerDocument.defaultView ?? window;
  try {
    while (current !== current.top) {
      const frame = current.frameElement;
      if (!frame) return { translated: false };
      const frameRect = frame.getBoundingClientRect();
      rect.x += frameRect.x;
      rect.y += frameRect.y;
      current = current.parent;
    }
    return { rect, translated: true };
  } catch {
    return { translated: false };
  }
}

export function isVisible(element: Element): boolean {
  const style = getComputedStyle(element);
  if (
    style.display === "none" ||
    style.visibility === "hidden" ||
    style.visibility === "collapse" ||
    style.pointerEvents === "none" ||
    (style.opacity !== "" && Number(style.opacity) === 0) ||
    element.getAttribute("aria-hidden") === "true" ||
    element.closest("[hidden], [inert]")
  ) {
    return false;
  }
  const rect = element.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}

function isEnabled(element: Element): boolean {
  if (element.getAttribute("aria-disabled") === "true") return false;
  return !(
    "disabled" in element && Boolean((element as HTMLButtonElement).disabled)
  );
}

function center(rect: DOMRect | Rect): { x: number; y: number } {
  return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };
}

function receivesPointerEvents(element: Element): boolean {
  const rect = element.getBoundingClientRect();
  if (!rect.width || !rect.height) return false;
  const point = center(rect);
  const root = element.getRootNode();
  const rootHitTest = (
    root as {
      elementFromPoint?: (x: number, y: number) => Element | null;
    }
  ).elementFromPoint;
  const hit = rootHitTest
    ? rootHitTest.call(root, point.x, point.y)
    : element.ownerDocument.elementFromPoint?.(point.x, point.y);
  if (!hit) return false;
  if (hit === element || element.contains(hit) || hit.contains(element))
    return true;
  return "host" in root && hit === (root as ShadowRoot).host;
}

export function actionabilityOf(
  element: Element,
  stable = false,
): Actionability {
  const attached = element.isConnected;
  const visible = attached && isVisible(element);
  const enabled = attached && isEnabled(element);
  const rect = element.getBoundingClientRect();
  const view = element.ownerDocument.defaultView ?? window;
  const inViewport =
    rect.bottom > 0 &&
    rect.right > 0 &&
    rect.top < view.innerHeight &&
    rect.left < view.innerWidth;
  const receives = visible && receivesPointerEvents(element);
  const reasons: string[] = [];
  if (!attached) reasons.push("detached");
  if (!visible) reasons.push("hidden");
  if (!enabled) reasons.push("disabled");
  if (!stable) reasons.push("stability-not-checked");
  if (!inViewport) reasons.push("outside-viewport");
  if (!receives) reasons.push("obscured");
  return {
    attached,
    visible,
    enabled,
    stable,
    inViewport,
    receivesPointerEvents: receives,
    reasons,
  };
}

export function isSensitiveElement(element: Element): boolean {
  if (element.getAttribute(SENSITIVE_ATTRIBUTE) === "true") return true;
  if (!(
    element instanceof HTMLInputElement ||
    element instanceof HTMLTextAreaElement
  ))
    return false;
  if (element instanceof HTMLInputElement && element.type === "password")
    return true;
  const autocomplete = (
    element.getAttribute("autocomplete") ?? ""
  ).toLowerCase();
  if (/password|one-time-code|cc-number|cc-csc|cc-exp/.test(autocomplete))
    return true;
  const identity = [
    element.id,
    element.getAttribute("name"),
    element.getAttribute("aria-label"),
    element.getAttribute("placeholder"),
  ]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
  return /(?:pass(?:word|wd)?|secret|token|otp|one.?time|verification.?code|cvv|cvc|card.?number)/.test(
    identity,
  );
}

export function safeValue(element: Element): string | undefined {
  if (isSensitiveElement(element)) return undefined;
  if (
    element instanceof HTMLInputElement ||
    element instanceof HTMLTextAreaElement ||
    element instanceof HTMLSelectElement
  ) {
    return element.value;
  }
  return undefined;
}

function selectedAttributes(element: Element): Record<string, string> {
  const output: Record<string, string> = {};
  for (const name of [
    "id",
    "name",
    "type",
    "href",
    "src",
    "title",
    "placeholder",
    "aria-label",
    "aria-expanded",
    "aria-checked",
    "data-testid",
  ]) {
    const value = element.getAttribute(name);
    if (value !== null)
      output[name] =
        name === "href" ? value.slice(0, 512) : value.slice(0, 256);
  }
  return output;
}

function describe(
  element: Element,
  nodeRef: string,
  stable = false,
  elementIdentity = "",
): NodeDescription {
  const translated = topRectOf(element);
  const sensitive = isSensitiveElement(element);
  return {
    nodeRef,
    elementIdentity,
    tag: element.tagName.toLowerCase(),
    role: implicitRole(element),
    name: sensitive
      ? undefined
      : accessibleName(element).slice(0, 512) || undefined,
    text: sensitive
      ? undefined
      : elementText(element).slice(0, 512) || undefined,
    value: safeValue(element),
    sensitive: sensitive || undefined,
    attributes: selectedAttributes(element),
    rect: rectOf(element),
    topRect: translated.rect,
    canTranslateToTop: translated.translated,
    actionability: actionabilityOf(element, stable),
  };
}

function escapeAi(value: string): string {
  return value
    .replaceAll("\\", "\\\\")
    .replaceAll('"', '\\"')
    .replaceAll("\n", "\\n");
}

function aiLine(node: NodeDescription): string {
  const attrs = [`ref=${node.nodeRef}`];
  if (node.role) attrs.push(`role=${node.role}`);
  for (const [key, value] of Object.entries(node.attributes))
    attrs.push(`${key}=\"${escapeAi(value)}\"`);
  if (node.value !== undefined) attrs.push(`value=\"${escapeAi(node.value)}\"`);
  const text = normalizeText(node.name || node.text).slice(0, 300);
  return `<${node.tag} ${attrs.join(" ")}>${text ? escapeAi(text) : ""}</${node.tag}>`;
}

function nativeSetter(
  prototype: object,
  property: string,
): ((this: unknown, value: unknown) => void) | undefined {
  return Object.getOwnPropertyDescriptor(prototype, property)?.set;
}

export function setEditableValue(element: Element, value: string): void {
  if (element instanceof HTMLInputElement) {
    nativeSetter(HTMLInputElement.prototype, "value")?.call(element, value);
  } else if (element instanceof HTMLTextAreaElement) {
    nativeSetter(HTMLTextAreaElement.prototype, "value")?.call(element, value);
  } else if (element instanceof HTMLElement && element.isContentEditable) {
    element.textContent = value;
  } else {
    throw new Error("target is not editable");
  }
  element.dispatchEvent(
    new InputEvent("input", {
      bubbles: true,
      inputType: "insertText",
      data: value,
    }),
  );
  element.dispatchEvent(new Event("change", { bubbles: true }));
}

function setChecked(element: Element, checked: boolean): void {
  if (
    !(element instanceof HTMLInputElement) ||
    !["checkbox", "radio"].includes(element.type)
  ) {
    throw new Error("target is not checkable");
  }
  nativeSetter(HTMLInputElement.prototype, "checked")?.call(element, checked);
  element.dispatchEvent(new Event("input", { bubbles: true }));
  element.dispatchEvent(new Event("change", { bubbles: true }));
}

function performDomAction(element: Element, action: RuntimeAction): void {
  const html = element as HTMLElement;
  element.scrollIntoView({
    block: "center",
    inline: "center",
    behavior: "instant",
  });
  switch (action.type) {
    case "click":
      html.click();
      return;
    case "doubleClick":
      html.click();
      html.click();
      html.dispatchEvent(
        new MouseEvent("dblclick", { bubbles: true, detail: 2 }),
      );
      return;
    case "hover":
      html.dispatchEvent(new PointerEvent("pointerover", { bubbles: true }));
      html.dispatchEvent(new MouseEvent("mouseover", { bubbles: true }));
      return;
    case "focus":
      html.focus();
      return;
    case "fill":
      html.focus();
      setEditableValue(element, String(action.value ?? ""));
      return;
    case "type": {
      html.focus();
      const previous =
        element instanceof HTMLInputElement ||
        element instanceof HTMLTextAreaElement
          ? element.value
          : (element.textContent ?? "");
      setEditableValue(element, previous + String(action.value ?? ""));
      return;
    }
    case "press": {
      html.focus();
      const key = String(action.value ?? "");
      html.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true }));
      html.dispatchEvent(new KeyboardEvent("keyup", { key, bubbles: true }));
      return;
    }
    case "check":
      setChecked(element, true);
      return;
    case "uncheck":
      setChecked(element, false);
      return;
    case "select": {
      if (!(element instanceof HTMLSelectElement))
        throw new Error("target is not a select");
      const values = new Set(
        Array.isArray(action.value)
          ? action.value.map(String)
          : [String(action.value ?? "")],
      );
      for (const option of Array.from(element.options))
        option.selected = values.has(option.value);
      element.dispatchEvent(new Event("input", { bubbles: true }));
      element.dispatchEvent(new Event("change", { bubbles: true }));
      return;
    }
    case "scroll": {
      const delta = action.value ?? {};
      if (element instanceof HTMLElement)
        element.scrollBy({
          left: delta.x ?? 0,
          top: delta.y ?? 0,
          behavior: "instant",
        });
      return;
    }
  }
}

export class LocatorRuntime implements LocatorRuntimeApi {
  readonly version = VERSION;
  readonly #documentId = randomId("doc");
  readonly #snapshots = new Map<string, SnapshotState>();
  readonly #elementIdentities = new WeakMap<Element, string>();
  readonly #maskedStyles = new Map<HTMLElement, string | null>();
  readonly #observer: MutationObserver;
  #revision = 1;

  constructor() {
    this.#observer = new MutationObserver(() => {
      this.#revision += 1;
    });
    this.#observer.observe(document, {
      subtree: true,
      childList: true,
      attributes: true,
      characterData: true,
    });
    addEventListener("scroll", this.#onVisualChange, true);
    addEventListener("resize", this.#onVisualChange, true);
  }

  #onVisualChange = (): void => {
    this.#revision += 1;
  };

  getRevision(): number {
    return this.#revision;
  }

  capture(options: SnapshotOptions = {}): RuntimeSnapshot {
    const maxNodes = Math.max(1, options.maxNodes ?? 2_000);
    const includeHidden = options.includeHidden ?? false;
    const elements = collectElements(document, true);
    const candidates = elements.filter((element) => {
      if (
        ["script", "style", "noscript", "template"].includes(
          element.tagName.toLowerCase(),
        )
      )
        return false;
      if (
        element.id === "__browser_control_indicator" ||
        element.closest("#__browser_control_indicator")
      )
        return false;
      if (!includeHidden && !isVisible(element)) return false;
      const role = implicitRole(element);
      const significantText =
        (options.includeText ?? true) &&
        element.children.length === 0 &&
        elementText(element).length > 0;
      return Boolean(
        role ||
        significantText ||
        element.matches("[contenteditable], [tabindex], [onclick]"),
      );
    });
    const selected = candidates.slice(0, maxNodes);
    const snapshotId = randomId("snap");
    const state: SnapshotState = { refs: new Map(), reverseRefs: new Map() };
    const nodes = selected.map((element, index) => {
      const nodeRef = `e${index + 1}`;
      state.refs.set(nodeRef, element);
      state.reverseRefs.set(element, nodeRef);
      return this.#describe(element, nodeRef);
    });
    this.#snapshots.set(snapshotId, state);
    while (this.#snapshots.size > MAX_SNAPSHOTS) {
      const oldest = this.#snapshots.keys().next().value as string | undefined;
      if (!oldest) break;
      this.#snapshots.delete(oldest);
    }
    return {
      snapshotId,
      documentId: this.#documentId,
      revision: this.#revision,
      url: location.href,
      title: document.title,
      aiDom: nodes.map(aiLine).join("\n"),
      nodes,
      truncated: candidates.length > selected.length,
      viewport: this.viewport(),
    };
  }

  query(locator: Locator, snapshotId?: string): NodeDescription[] {
    const elements = queryLocator(document, locator);
    const existing = snapshotId ? this.#snapshots.get(snapshotId) : undefined;
    return elements.map((element, index) => {
      const nodeRef = existing?.reverseRefs.get(element) ?? `q${index + 1}`;
      return this.#describe(element, nodeRef);
    });
  }

  resolve(snapshotId: string, nodeRef: string): NodeDescription {
    const snapshot = this.#snapshots.get(snapshotId);
    if (!snapshot)
      throw new Error("STALE_REFERENCE: snapshot is no longer available");
    const element = snapshot.refs.get(nodeRef);
    if (!element || !element.isConnected)
      throw new Error("STALE_REFERENCE: node is detached");
    return this.#describe(element, nodeRef);
  }

  async prepare(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
  ): Promise<NodeDescription> {
    const { element, ref } = this.#resolveTarget(target);
    return this.#prepareElement(element, ref);
  }

  async perform(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
    action: RuntimeAction,
    expectedElementIdentity?: string,
  ): Promise<ActionResult> {
    const { element, ref } = this.#resolveTarget(target);
    const prepared = await this.#prepareElement(element, ref);
    if (
      expectedElementIdentity &&
      prepared.elementIdentity !== expectedElementIdentity
    ) {
      throw new Error("STALE_REFERENCE: target element identity changed");
    }
    if (!element.isConnected)
      throw new Error("STALE_REFERENCE: target element was detached");
    performDomAction(element, action);
    return {
      performed: true,
      revision: this.#revision,
      inputMode: "dom",
      target: this.#describe(element, ref, true),
    };
  }

  markSensitive(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
    expectedElementIdentity?: string,
  ): NodeDescription {
    const { element, ref } = this.#resolveTarget(target);
    const identity = this.#identity(element);
    if (expectedElementIdentity && identity !== expectedElementIdentity)
      throw new Error("STALE_REFERENCE: target element identity changed");
    element.setAttribute(SENSITIVE_ATTRIBUTE, "true");
    return this.#describe(element, ref);
  }

  setSensitiveMask(enabled: boolean): void {
    if (!enabled) {
      for (const [element, previous] of this.#maskedStyles) {
        if (!element.isConnected) continue;
        if (previous === null) element.removeAttribute("style");
        else element.setAttribute("style", previous);
      }
      this.#maskedStyles.clear();
      this.#observer.takeRecords();
      return;
    }
    if (this.#maskedStyles.size) return;
    for (const element of collectElements(document, true)) {
      if (!isSensitiveElement(element) || !(element instanceof HTMLElement))
        continue;
      this.#maskedStyles.set(element, element.getAttribute("style"));
      element.style.setProperty("color", "transparent", "important");
      element.style.setProperty("caret-color", "transparent", "important");
      element.style.setProperty("text-shadow", "none", "important");
    }
    this.#observer.takeRecords();
  }

  #identity(element: Element): string {
    let identity = this.#elementIdentities.get(element);
    if (!identity) {
      identity = randomId("el");
      this.#elementIdentities.set(element, identity);
    }
    return identity;
  }

  #describe(
    element: Element,
    nodeRef: string,
    stable = false,
  ): NodeDescription {
    return describe(element, nodeRef, stable, this.#identity(element));
  }

  async #prepareElement(
    element: Element,
    ref: string,
  ): Promise<NodeDescription> {
    element.scrollIntoView({
      block: "center",
      inline: "center",
      behavior: "instant",
    });
    const stable = await this.#waitForStable(element);
    const actionability = actionabilityOf(element, stable);
    if (
      !actionability.visible ||
      !actionability.enabled ||
      !actionability.stable ||
      !actionability.receivesPointerEvents
    ) {
      throw new Error(`NOT_ACTIONABLE: ${actionability.reasons.join(", ")}`);
    }
    return this.#describe(element, ref, true);
  }

  #resolveTarget(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
  ): { element: Element; ref: string } {
    if ("locator" in target) {
      const matches = queryLocator(document, target.locator);
      if (matches.length === 0)
        throw new Error("LOCATOR_NOT_FOUND: no matching element");
      if (matches.length !== 1)
        throw new Error(
          `LOCATOR_AMBIGUOUS: ${matches.length} matching elements`,
        );
      const element = matches[0];
      if (!element?.isConnected)
        throw new Error("DETACHED_NODE: target is detached");
      return { element, ref: "q1" };
    }
    const snapshot = this.#snapshots.get(target.snapshotId);
    if (!snapshot)
      throw new Error("STALE_REFERENCE: snapshot is no longer available");
    const element = snapshot.refs.get(target.nodeRef);
    if (!element?.isConnected)
      throw new Error("DETACHED_NODE: target is detached");
    return { element, ref: target.nodeRef };
  }

  async #waitForStable(element: Element): Promise<boolean> {
    const frame = (): Promise<void> =>
      new Promise((resolve) => requestAnimationFrame(() => resolve()));
    await frame();
    const first = element.getBoundingClientRect();
    await frame();
    const second = element.getBoundingClientRect();
    return (
      element.isConnected &&
      Math.abs(first.x - second.x) < 0.5 &&
      Math.abs(first.y - second.y) < 0.5 &&
      Math.abs(first.width - second.width) < 0.5 &&
      Math.abs(first.height - second.height) < 0.5
    );
  }

  fullHtml(): string {
    return sanitizedClone(document.documentElement).outerHTML;
  }

  viewport(): RuntimeViewport {
    const visual = window.visualViewport;
    return {
      width: innerWidth,
      height: innerHeight,
      devicePixelRatio,
      scrollX,
      scrollY,
      visualViewport: visual
        ? {
            width: visual.width,
            height: visual.height,
            offsetLeft: visual.offsetLeft,
            offsetTop: visual.offsetTop,
            scale: visual.scale,
          }
        : undefined,
    };
  }

  exportContent(format: "html" | "text" | "markdown"): string {
    if (format === "html") return this.fullHtml();
    const clone = document.body ? sanitizedClone(document.body) : undefined;
    if (!clone) return "";
    if (format === "text")
      return normalizeText(clone.innerText || clone.textContent || "");
    clone
      .querySelectorAll(
        "script, style, noscript, template, #__browser_control_indicator",
      )
      .forEach((node) => node.remove());
    clone.querySelectorAll("a[href]").forEach((node) => {
      const element = node as HTMLAnchorElement;
      element.replaceWith(
        `${normalizeText(element.textContent)} (${safeExportUrl(element.href)})`,
      );
    });
    clone.querySelectorAll("h1,h2,h3,h4,h5,h6").forEach((node) => {
      const level = Number(node.tagName.slice(1));
      node.replaceWith(
        `${"#".repeat(level)} ${normalizeText(node.textContent)}\n\n`,
      );
    });
    clone
      .querySelectorAll("li")
      .forEach((node) =>
        node.replaceWith(`- ${normalizeText(node.textContent)}\n`),
      );
    clone.querySelectorAll("br").forEach((node) => node.replaceWith("\n"));
    clone
      .querySelectorAll(
        "p,div,section,article,header,footer,nav,main,blockquote,pre",
      )
      .forEach((node) => {
        node.append("\n\n");
      });
    return (clone.textContent ?? "")
      .replace(/\n[ \t]+/g, "\n")
      .replace(/\n{3,}/g, "\n\n")
      .trim();
  }

  pageAssets(): PageAsset[] {
    const assets: PageAsset[] = [];
    const add = (asset: PageAsset): void => {
      if (!asset.url && !asset.text) return;
      if (
        asset.url &&
        assets.some(
          (existing) =>
            existing.kind === asset.kind && existing.url === asset.url,
        )
      )
        return;
      assets.push(asset);
    };
    for (const image of Array.from(document.images)) {
      add({
        kind: "image",
        url: image.currentSrc || image.src,
        width: image.naturalWidth,
        height: image.naturalHeight,
      });
    }
    for (const media of Array.from(
      document.querySelectorAll<HTMLMediaElement>("video, audio"),
    )) {
      add({ kind: "media", url: media.currentSrc || media.src });
      media.querySelectorAll("source[src]").forEach((source) =>
        add({
          kind: "media",
          url: (source as HTMLSourceElement).src,
          mimeType: (source as HTMLSourceElement).type || undefined,
        }),
      );
    }
    document.querySelectorAll<HTMLLinkElement>("link[href]").forEach((link) => {
      const rel = link.rel.toLowerCase();
      add({
        kind: rel.includes("stylesheet")
          ? "stylesheet"
          : rel.includes("preload") && link.as === "font"
            ? "font"
            : "link",
        url: link.href,
        mimeType: link.type || undefined,
      });
    });
    document
      .querySelectorAll<HTMLScriptElement>("script[src]")
      .forEach((script) =>
        add({
          kind: "script",
          url: script.src,
          mimeType: script.type || undefined,
        }),
      );
    document.querySelectorAll<SVGElement>("svg").forEach((svg) =>
      add({
        kind: "svg",
        text: svg.outerHTML.slice(0, 1_000_000),
        mimeType: "image/svg+xml",
      }),
    );
    try {
      for (const sheet of Array.from(document.styleSheets)) {
        for (const rule of Array.from(sheet.cssRules ?? [])) {
          const matches = rule.cssText.matchAll(/url\(["']?([^"')]+)["']?\)/g);
          for (const match of matches) {
            if (!match[1] || match[1].startsWith("data:")) continue;
            add({
              kind: /font-face/i.test(rule.cssText) ? "font" : "image",
              url: new URL(match[1], sheet.href || location.href).href,
            });
          }
        }
      }
    } catch {
      // Cross-origin stylesheets intentionally hide cssRules.
    }
    return assets;
  }

  dispose(): void {
    this.setSensitiveMask(false);
    this.#observer.disconnect();
    removeEventListener("scroll", this.#onVisualChange, true);
    removeEventListener("resize", this.#onVisualChange, true);
    this.#snapshots.clear();
  }
}

function sanitizedClone<T extends Element>(element: T): T {
  const clone = element.cloneNode(true) as T;
  const elements = [clone, ...Array.from(clone.querySelectorAll("*"))];
  for (const candidate of elements) {
    if (candidate.matches("input, textarea")) {
      candidate.removeAttribute("value");
      if (candidate.tagName === "TEXTAREA") candidate.textContent = "";
    }
    if (candidate.getAttribute(SENSITIVE_ATTRIBUTE) === "true") {
      candidate.removeAttribute("value");
      if (!candidate.matches("input")) candidate.textContent = REDACTED_TEXT;
    }
    candidate.removeAttribute(SENSITIVE_ATTRIBUTE);
  }
  return clone;
}

function safeExportUrl(value: string): string {
  try {
    const url = new URL(value, location.href);
    url.search = "";
    url.hash = "";
    return url.toString();
  } catch {
    return "";
  }
}

export function createLocatorRuntime(): LocatorRuntimeApi {
  return new LocatorRuntime();
}
