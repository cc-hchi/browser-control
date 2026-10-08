import type { Locator } from "../../../packages/locator-runtime/src/types.js";
import { RpcError, record } from "./shared.js";

const LOCATOR_KINDS = new Set([
  "role",
  "text",
  "label",
  "placeholder",
  "testId",
  "css",
]);
const LOCATOR_FIELDS = new Set([
  "by",
  "value",
  "role",
  "name",
  "exact",
  "framePath",
  "scope",
  "has",
  "hasText",
  "and",
  "or",
  "index",
  "nth",
  "first",
  "last",
  "shadow",
]);
const NESTED_LOCATOR_FIELDS = ["scope", "has", "and", "or"] as const;

// Validates a caller-supplied locator. Unknown fields used to be ignored, so a
// misspelled field (for example `selector` instead of `value`) silently matched
// nothing and surfaced as LOCATOR_NOT_FOUND. Reject them with a precise error.
export function locatorFrom(value: unknown, path = "locator"): Locator {
  const input = { ...record(value) };
  // `selector` is the conventional name for a CSS query; accept it as an alias.
  if (input.by === "css" && input.value === undefined && "selector" in input) {
    input.value = input.selector;
  }
  delete input.selector;
  if (typeof input.by !== "string" || !input.by)
    throw new RpcError("INVALID_REQUEST", `${path}.by is required`);
  if (!LOCATOR_KINDS.has(input.by))
    throw new RpcError(
      "INVALID_REQUEST",
      `${path}.by must be one of ${Array.from(LOCATOR_KINDS).join(", ")}`,
    );
  const unknown = Object.keys(input).filter((key) => !LOCATOR_FIELDS.has(key));
  if (unknown.length)
    throw new RpcError(
      "INVALID_REQUEST",
      `${path} has unsupported field(s): ${unknown.join(", ")}`,
    );
  if (["css", "text", "label", "placeholder", "testId"].includes(input.by)) {
    if (typeof input.value !== "string" || !input.value)
      throw new RpcError(
        "INVALID_REQUEST",
        `${path}.value is required for by: "${input.by}"`,
      );
  }
  for (const key of NESTED_LOCATOR_FIELDS) {
    if (input[key] !== undefined)
      input[key] = locatorFrom(input[key], `${path}.${key}`);
  }
  if (input.framePath !== undefined) {
    if (!Array.isArray(input.framePath))
      throw new RpcError(
        "INVALID_REQUEST",
        `${path}.framePath must be an array`,
      );
    input.framePath = input.framePath.map((entry, index) =>
      locatorFrom(entry, `${path}.framePath[${index}]`),
    );
  }
  return input as unknown as Locator;
}
