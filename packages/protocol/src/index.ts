export const protocolName = "browser-control" as const;
export const protocolVersion = "1.0" as const;

export type JsonRpcId = string | number;

export interface JsonRpcRequest<T = unknown> {
  jsonrpc: "2.0";
  id: JsonRpcId;
  method: string;
  params?: T;
}

export interface JsonRpcNotification<T = unknown> {
  jsonrpc: "2.0";
  method: string;
  params?: T;
}

export interface JsonRpcSuccess<T = unknown> {
  jsonrpc: "2.0";
  id: JsonRpcId;
  result: T;
}

export type Effect = "none" | "possible" | "confirmed";

export interface BrowserControlErrorData {
  kind: string;
  retryable: boolean;
  effect: Effect;
  operationId?: string;
  sessionId?: string;
  tabId?: string;
  expectedDocumentEpoch?: number;
  actualDocumentEpoch?: number;
  recovery?: { method?: string; reason?: string };
}

export interface JsonRpcFailure {
  jsonrpc: "2.0";
  id: JsonRpcId;
  error: {
    code: number;
    message: string;
    data: BrowserControlErrorData;
  };
}

export type JsonRpcMessage =
  | JsonRpcRequest
  | JsonRpcNotification
  | JsonRpcSuccess
  | JsonRpcFailure;

export type Capability =
  | "history.read"
  | "clipboard.read"
  | "clipboard.write"
  | "files.upload"
  | "files.download"
  | "secureInput"
  | "artifact.localPath"
  | "unsafe.evaluate"
  | "unsafe.cdp";

export type LocatorKind =
  | "role"
  | "text"
  | "label"
  | "placeholder"
  | "testId"
  | "css";

export interface TextMatcher {
  text: string;
  exact?: boolean;
}

export interface Locator {
  by: LocatorKind;
  value?: string;
  role?: string;
  name?: string | TextMatcher;
  exact?: boolean;
  framePath?: Locator[];
  scope?: Locator;
  has?: Locator;
  hasText?: string;
  and?: Locator;
  or?: Locator;
  index?: number;
  nth?: number;
  first?: boolean;
  last?: boolean;
  shadow?: "auto" | "none";
}

export type ActionTarget =
  | { locator: Locator }
  | { snapshotId: string; nodeRef: string }
  | { snapshotId: string; point: { x: number; y: number } };

export interface BrowserEvent<T = unknown> {
  seq: number;
  time: string;
  type: string;
  sessionId?: string;
  tabId?: string;
  documentEpoch?: number;
  payload?: T;
}

export function isJsonRpcMessage(value: unknown): value is JsonRpcMessage {
  if (typeof value !== "object" || value === null) return false;
  const candidate = value as Record<string, unknown>;
  if (candidate.jsonrpc !== "2.0") return false;
  if ("method" in candidate) return typeof candidate.method === "string";
  if (!("id" in candidate)) return false;
  return "result" in candidate || "error" in candidate;
}
