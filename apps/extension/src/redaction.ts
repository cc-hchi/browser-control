const REDACTED = "[REDACTED]";

type Values = Record<string, unknown>;

function record(value: unknown): Values {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("REDACTION_FAILED: unexpected CDP snapshot shape");
  return value as Values;
}

function numbers(value: unknown): number[] {
  if (!Array.isArray(value) || value.some((item) => !Number.isInteger(item)))
    throw new Error("REDACTION_FAILED: expected an integer array");
  return value as number[];
}

function optionalRareStrings(value: unknown): number[] {
  if (value === undefined) return [];
  const rare = record(value);
  const indexes = numbers(rare.index);
  const values = numbers(rare.value);
  if (indexes.length !== values.length)
    throw new Error("REDACTION_FAILED: malformed rare string data");
  return values;
}

function stringAt(strings: string[], index: number): string {
  if (index === -1) return "";
  if (index < 0 || index >= strings.length)
    throw new Error("REDACTION_FAILED: string index is out of range");
  return strings[index]!;
}

export function sanitizeDomSnapshot(snapshot: unknown): unknown {
  const cloned = structuredClone(snapshot);
  const root = record(cloned);
  if (
    !Array.isArray(root.strings) ||
    root.strings.some((item) => typeof item !== "string")
  )
    throw new Error("REDACTION_FAILED: snapshot strings are missing");
  if (!Array.isArray(root.documents))
    throw new Error("REDACTION_FAILED: snapshot documents are missing");
  const strings = root.strings as string[];
  const taintedIndexes = new Set<number>();

  const taint = (index: number): void => {
    if (stringAt(strings, index) !== "") taintedIndexes.add(index);
  };

  for (const rawDocument of root.documents) {
    const document = record(rawDocument);
    const nodes = record(document.nodes);
    const nodeNames = numbers(nodes.nodeName);
    const nodeValues = numbers(nodes.nodeValue);
    const parentIndexes = numbers(nodes.parentIndex);
    if (
      nodeNames.length !== nodeValues.length ||
      nodeNames.length !== parentIndexes.length ||
      !Array.isArray(nodes.attributes) ||
      nodes.attributes.length !== nodeNames.length
    ) {
      throw new Error("REDACTION_FAILED: inconsistent DOM node arrays");
    }
    const attributes = nodes.attributes as unknown[];
    const sensitiveNodes = new Set<number>();

    for (let nodeIndex = 0; nodeIndex < nodeNames.length; nodeIndex += 1) {
      const tag = stringAt(strings, nodeNames[nodeIndex]!).toUpperCase();
      const flatAttributes = numbers(attributes[nodeIndex]);
      if (flatAttributes.length % 2 !== 0)
        throw new Error("REDACTION_FAILED: malformed DOM attributes");
      let markedSensitive = false;
      for (let index = 0; index < flatAttributes.length; index += 2) {
        const name = stringAt(strings, flatAttributes[index]!).toLowerCase();
        const valueIndex = flatAttributes[index + 1]!;
        stringAt(strings, valueIndex);
        if (name === "data-browser-control-sensitive") markedSensitive = true;
        if (name === "value" && (tag === "INPUT" || tag === "TEXTAREA"))
          taint(valueIndex);
      }
      if (tag === "INPUT" || tag === "TEXTAREA" || markedSensitive)
        sensitiveNodes.add(nodeIndex);
    }

    for (const valueIndex of optionalRareStrings(nodes.inputValue))
      taint(valueIndex);
    for (const valueIndex of optionalRareStrings(nodes.textValue))
      taint(valueIndex);

    const belongsToSensitiveSubtree = (nodeIndex: number): boolean => {
      const visited = new Set<number>();
      let current = nodeIndex;
      while (current >= 0) {
        if (sensitiveNodes.has(current)) return true;
        if (visited.has(current) || current >= parentIndexes.length)
          throw new Error("REDACTION_FAILED: invalid DOM parent graph");
        visited.add(current);
        current = parentIndexes[current]!;
      }
      return false;
    };

    for (let nodeIndex = 0; nodeIndex < nodeValues.length; nodeIndex += 1) {
      if (belongsToSensitiveSubtree(nodeIndex)) taint(nodeValues[nodeIndex]!);
    }

    if (document.layout !== undefined) {
      const layout = record(document.layout);
      const layoutNodes = numbers(layout.nodeIndex);
      const layoutText = numbers(layout.text);
      if (layoutNodes.length !== layoutText.length)
        throw new Error("REDACTION_FAILED: inconsistent layout arrays");
      for (let index = 0; index < layoutNodes.length; index += 1) {
        if (belongsToSensitiveSubtree(layoutNodes[index]!))
          taint(layoutText[index]!);
      }
    }
  }

  const taintedValues = Array.from(
    taintedIndexes,
    (index) => strings[index]!,
  ).filter(Boolean);
  for (let index = 0; index < strings.length; index += 1) {
    const value = strings[index]!;
    if (
      taintedIndexes.has(index) ||
      taintedValues.some((secret) => value.includes(secret))
    ) {
      strings[index] = REDACTED;
    }
  }
  return cloned;
}

export function sanitizeAccessibilitySnapshot(snapshot: unknown): unknown {
  const cloned = structuredClone(snapshot);
  const redactValueTree = (value: unknown): void => {
    if (Array.isArray(value)) {
      value.forEach(redactValueTree);
      return;
    }
    if (!value || typeof value !== "object") return;
    const item = value as Values;
    if (typeof item.value === "string") item.value = REDACTED;
    Object.values(item).forEach(redactValueTree);
  };
  const redactEditableTree = (value: Values): void => {
    if (!Array.isArray(value.nodes)) return;
    const nodes = value.nodes.filter(
      (node): node is Values => Boolean(node && typeof node === "object"),
    );
    const byID = new Map(nodes.map((node) => [String(node.nodeId ?? ""), node]));
    const sensitive = new Set<string>();
    for (const node of nodes) {
      const role = node.role as Values | undefined;
      const roleValue = typeof role?.value === "string" ? role.value.toLowerCase() : "";
      const editable = Array.isArray(node.properties) && node.properties.some((property) => {
        if (!property || typeof property !== "object") return false;
        return String((property as Values).name).toLowerCase() === "editable";
      });
      if (["textbox", "searchbox", "combobox", "spinbutton"].includes(roleValue) || editable)
        sensitive.add(String(node.nodeId ?? ""));
    }
    const queue = Array.from(sensitive);
    while (queue.length) {
      const id = queue.shift()!;
      const node = byID.get(id);
      if (!node || !Array.isArray(node.childIds)) continue;
      for (const child of node.childIds) {
        const childID = String(child);
        if (sensitive.has(childID)) continue;
        sensitive.add(childID);
        queue.push(childID);
      }
    }
    for (const id of sensitive) {
      const node = byID.get(id);
      if (!node) continue;
      redactValueTree(node.name);
      redactValueTree(node.value);
      redactValueTree(node.description);
    }
  };
  const visit = (value: unknown): void => {
    if (Array.isArray(value)) {
      value.forEach(visit);
      return;
    }
    if (!value || typeof value !== "object") return;
    const item = value as Values;
    redactEditableTree(item);
    const role = item.role as Values | undefined;
    const roleValue =
      typeof role?.value === "string" ? role.value.toLowerCase() : "";
    if (
      ["textbox", "searchbox", "combobox", "spinbutton"].includes(roleValue) &&
      item.value &&
      typeof item.value === "object"
    ) {
      (item.value as Values).value = REDACTED;
    }
    if (Array.isArray(item.properties)) {
      for (const property of item.properties) {
        if (!property || typeof property !== "object") continue;
        const entry = property as Values;
        if (
          ["value", "valuetext"].includes(String(entry.name).toLowerCase()) &&
          entry.value &&
          typeof entry.value === "object"
        ) {
          (entry.value as Values).value = REDACTED;
        }
      }
    }
    Object.values(item).forEach(visit);
  };
  visit(cloned);
  return cloned;
}
