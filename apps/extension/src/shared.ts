export const PROTOCOL_VERSION = "1.0";
export const NATIVE_HOST = "com.browser_control.native_host";

export type JsonRpcId = string | number;
export interface JsonRpcRequest {
  jsonrpc: "2.0";
  id: JsonRpcId;
  method: string;
  params?: unknown;
}
export interface JsonRpcNotification {
  jsonrpc: "2.0";
  method: string;
  params?: unknown;
}
export interface JsonRpcSuccess {
  jsonrpc: "2.0";
  id: JsonRpcId;
  result: unknown;
}
export interface JsonRpcFailure {
  jsonrpc: "2.0";
  id: JsonRpcId;
  error: RpcErrorShape;
}
export type JsonRpcMessage =
  JsonRpcRequest | JsonRpcNotification | JsonRpcSuccess | JsonRpcFailure;

export interface RpcErrorShape {
  code: number;
  message: string;
  data: {
    kind: string;
    retryable: boolean;
    effect: "none" | "possible" | "confirmed";
    [key: string]: unknown;
  };
}

const ERROR_CODES: Record<string, number> = {
  INVALID_REQUEST: -32602,
  METHOD_NOT_FOUND: -32601,
  INTERNAL: -32603,
  TAB_NOT_FOUND: -32003,
  TAB_CLOSED: -32003,
  TAB_NOT_CLAIMED: -32005,
  LEASE_CONFLICT: -32004,
  LEASE_REQUIRED: -32005,
  STALE_REFERENCE: -32010,
  LOCATOR_NOT_FOUND: -32010,
  LOCATOR_AMBIGUOUS: -32012,
  NOT_ACTIONABLE: -32012,
  DEBUGGER_CONFLICT: -32008,
  DEBUGGER_DETACHED: -32008,
  EXTENSION_DISCONNECTED: -32008,
  UNSUPPORTED_PAGE: -32015,
  UNSUPPORTED: -32015,
  TIMEOUT: -32009,
};

export class RpcError extends Error {
  constructor(
    readonly kind: string,
    message: string,
    readonly options: {
      retryable?: boolean;
      effect?: "none" | "possible" | "confirmed";
      details?: Record<string, unknown>;
    } = {},
  ) {
    super(message);
  }

  toJSON(): RpcErrorShape {
    return {
      code: ERROR_CODES[this.kind] ?? -32603,
      message: this.message,
      data: {
        kind: this.kind,
        retryable: this.options.retryable ?? false,
        effect: this.options.effect ?? "none",
        ...this.options.details,
      },
    };
  }
}

export function asRpcError(error: unknown): RpcError {
  if (error instanceof RpcError) return error;
  const message = error instanceof Error ? error.message : String(error);
  const prefix = /^([A-Z_]+):\s*(.*)$/.exec(message);
  if (prefix?.[1]) return new RpcError(prefix[1], prefix[2] || message);
  return new RpcError("INTERNAL", message);
}

export function randomId(prefix: string): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return `${prefix}_${Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

export function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new RpcError("INVALID_REQUEST", "params must be an object");
  return value as Record<string, unknown>;
}

export function requiredString(
  params: Record<string, unknown>,
  key: string,
): string {
  const value = params[key];
  if (typeof value !== "string" || !value)
    throw new RpcError("INVALID_REQUEST", `${key} is required`);
  return value;
}
