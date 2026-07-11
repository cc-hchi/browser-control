import type { Locator, TextMatcher } from "./types.js";

const whitespace = /\s+/g;

export function normalizeText(value: string | null | undefined): string {
  return (value ?? "").replace(whitespace, " ").trim();
}

export function matchesText(
  value: string,
  matcher: TextMatcher,
  exact = false,
): boolean {
  const candidate = normalizeText(value);
  const expected = normalizeText(
    typeof matcher === "string" ? matcher : matcher.text,
  );
  const useExact =
    typeof matcher === "string" ? exact : (matcher.exact ?? exact);
  return useExact
    ? candidate === expected
    : candidate.toLocaleLowerCase().includes(expected.toLocaleLowerCase());
}

export function implicitRole(element: Element): string | undefined {
  const explicit = element.getAttribute("role")?.trim().split(/\s+/)[0];
  if (explicit) return explicit;

  switch (element.tagName.toLowerCase()) {
    case "a":
    case "area":
      return element.hasAttribute("href") ? "link" : undefined;
    case "button":
      return "button";
    case "details":
      return "group";
    case "dialog":
      return "dialog";
    case "form":
      return element.hasAttribute("aria-label") ||
        element.hasAttribute("aria-labelledby")
        ? "form"
        : undefined;
    case "h1":
    case "h2":
    case "h3":
    case "h4":
    case "h5":
    case "h6":
      return "heading";
    case "img":
      return element.getAttribute("alt") === "" ? undefined : "img";
    case "li":
      return "listitem";
    case "ol":
    case "ul":
      return "list";
    case "option":
      return "option";
    case "progress":
      return "progressbar";
    case "select":
      return element.hasAttribute("multiple") ? "listbox" : "combobox";
    case "summary":
      return "button";
    case "table":
      return "table";
    case "textarea":
      return "textbox";
    case "input": {
      const type = (element.getAttribute("type") ?? "text").toLowerCase();
      if (["button", "image", "reset", "submit"].includes(type))
        return "button";
      if (type === "checkbox") return "checkbox";
      if (type === "radio") return "radio";
      if (type === "range") return "slider";
      if (type === "number") return "spinbutton";
      if (["hidden", "file"].includes(type)) return undefined;
      return "textbox";
    }
    default:
      return undefined;
  }
}

function labelledByText(element: Element): string {
  const ids =
    element
      .getAttribute("aria-labelledby")
      ?.trim()
      .split(/\s+/)
      .filter(Boolean) ?? [];
  return ids
    .map((id) =>
      normalizeText(element.ownerDocument.getElementById(id)?.textContent),
    )
    .filter(Boolean)
    .join(" ");
}

export function accessibleName(element: Element): string {
  const ariaLabel = normalizeText(element.getAttribute("aria-label"));
  if (ariaLabel) return ariaLabel;
  const labelledBy = labelledByText(element);
  if (labelledBy) return labelledBy;

  if (
    element instanceof HTMLInputElement ||
    element instanceof HTMLTextAreaElement ||
    element instanceof HTMLSelectElement
  ) {
    const labels = Array.from(element.labels ?? [])
      .map((label) => normalizeText(label.textContent))
      .filter(Boolean);
    if (labels.length) return labels.join(" ");
    const placeholder = normalizeText(element.getAttribute("placeholder"));
    if (placeholder) return placeholder;
  }

  const alt = normalizeText(element.getAttribute("alt"));
  if (alt) return alt;
  const title = normalizeText(element.getAttribute("title"));
  if (title) return title;
  if (
    element instanceof HTMLInputElement &&
    ["button", "reset", "submit"].includes(element.type)
  ) {
    return normalizeText(element.value);
  }
  return normalizeText(
    (element as HTMLElement).innerText || element.textContent,
  );
}

export function elementText(element: Element): string {
  return normalizeText(
    (element as HTMLElement).innerText || element.textContent,
  );
}

export function collectElements(
  root: Document | Element | ShadowRoot,
  includeShadow = true,
): Element[] {
  const output: Element[] = [];
  const visit = (node: Document | Element | ShadowRoot): void => {
    for (const child of Array.from(node.children)) {
      output.push(child);
      if (includeShadow && child.shadowRoot) visit(child.shadowRoot);
      visit(child);
    }
  };
  visit(root);
  return output;
}

function baseMatches(element: Element, locator: Locator): boolean {
  const value = locator.value ?? "";
  switch (locator.by) {
    case "css":
      try {
        return element.matches(value);
      } catch {
        return false;
      }
    case "role": {
      const expectedRole = locator.role ?? value;
      if (implicitRole(element) !== expectedRole) return false;
      return (
        locator.name === undefined ||
        matchesText(accessibleName(element), locator.name, locator.exact)
      );
    }
    case "text":
      return matchesText(elementText(element), value, locator.exact);
    case "label":
      if (
        !element.hasAttribute("aria-label") &&
        !element.hasAttribute("aria-labelledby") &&
        !("labels" in element && (element as HTMLInputElement).labels?.length)
      ) {
        return false;
      }
      return matchesText(accessibleName(element), value, locator.exact);
    case "placeholder":
      return matchesText(
        element.getAttribute("placeholder") ?? "",
        value,
        locator.exact,
      );
    case "testId":
      return ["data-testid", "data-test-id", "data-test"].some((name) =>
        matchesText(
          element.getAttribute(name) ?? "",
          value,
          locator.exact ?? true,
        ),
      );
  }
}

export function queryLocator(
  root: Document | Element | ShadowRoot,
  locator: Locator,
): Element[] {
  const includeShadow = locator.shadow !== "none";
  let universe = collectElements(root, includeShadow);

  if (locator.scope) {
    const scopes = queryLocator(root, locator.scope);
    const scoped = new Set<Element>();
    for (const scope of scopes) {
      for (const descendant of collectElements(scope, includeShadow))
        scoped.add(descendant);
    }
    universe = universe.filter((element) => scoped.has(element));
  }

  let matches = universe.filter((element) => baseMatches(element, locator));
  if (locator.by === "text") {
    // Match the smallest element carrying the text, mirroring getByText-style
    // semantics instead of returning every ancestor whose textContent contains it.
    matches = matches.filter(
      (element) =>
        !collectElements(element, includeShadow).some((descendant) =>
          baseMatches(descendant, locator),
        ),
    );
  }
  if (locator.hasText !== undefined) {
    matches = matches.filter((element) =>
      matchesText(elementText(element), locator.hasText!),
    );
  }
  if (locator.has) {
    matches = matches.filter(
      (element) => queryLocator(element, locator.has!).length > 0,
    );
  }
  if (locator.and) {
    const intersection = new Set(queryLocator(root, locator.and));
    matches = matches.filter((element) => intersection.has(element));
  }
  if (locator.or) {
    const union = queryLocator(root, locator.or);
    matches = Array.from(new Set([...matches, ...union]));
  }
  const selectedIndex =
    locator.nth ??
    locator.index ??
    (locator.first ? 0 : locator.last ? -1 : undefined);
  if (selectedIndex !== undefined) {
    const selected = matches.at(selectedIndex);
    matches = selected ? [selected] : [];
  }
  return matches;
}
