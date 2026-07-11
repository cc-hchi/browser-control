import { ChunkAssembler, encodeMessage } from "./chunk-codec.js";
import {
  NATIVE_HOST,
  PROTOCOL_VERSION,
  RpcError,
  asRpcError,
  randomId,
  type JsonRpcId,
  type JsonRpcMessage,
  type JsonRpcNotification,
  type JsonRpcRequest,
} from "./shared.js";

export interface BridgeState {
  connected: boolean;
  helloComplete: boolean;
  lastError?: string;
  reconnectAttempt: number;
}

type RequestHandler = (method: string, params: unknown) => Promise<unknown>;
type StateHandler = (state: BridgeState) => void;

export class NativeBridge {
  readonly #assembler = new ChunkAssembler();
  readonly #pending = new Map<
    JsonRpcId,
    { resolve(value: unknown): void; reject(error: Error): void; timer: number }
  >();
  readonly #queuedNotifications: Array<{ method: string; params?: unknown }> =
    [];
  #port?: chrome.runtime.Port;
  #reconnectTimer?: number;
  #stopped = false;
  #instanceId = "";
  #state: BridgeState = {
    connected: false,
    helloComplete: false,
    reconnectAttempt: 0,
  };

  constructor(
    private readonly handleRequest: RequestHandler,
    private readonly handleNotification: RequestHandler,
    private readonly onState: StateHandler,
    private readonly onDisconnected: () => Promise<void>,
  ) {}

  get state(): BridgeState {
    return { ...this.#state };
  }

  async start(): Promise<void> {
    this.#stopped = false;
    const storage = await chrome.storage.local.get("browserInstanceId");
    this.#instanceId =
      typeof storage.browserInstanceId === "string"
        ? storage.browserInstanceId
        : randomId("browser");
    if (!storage.browserInstanceId)
      await chrome.storage.local.set({ browserInstanceId: this.#instanceId });
    this.#connect();
  }

  stop(): void {
    this.#stopped = true;
    if (this.#reconnectTimer) clearTimeout(this.#reconnectTimer);
    this.#port?.disconnect();
    this.#port = undefined;
    this.#rejectPending(
      new RpcError("EXTENSION_DISCONNECTED", "native bridge stopped"),
    );
  }

  async request(
    method: string,
    params?: unknown,
    timeoutMs = 10_000,
  ): Promise<unknown> {
    const id = randomId("extreq");
    const promise = new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(
          new RpcError("TIMEOUT", `bridge request timed out: ${method}`, {
            retryable: true,
          }),
        );
      }, timeoutMs) as unknown as number;
      this.#pending.set(id, { resolve, reject, timer });
    });
    void this.#send({ jsonrpc: "2.0", id, method, params }).catch(
      (error: unknown) => {
        const pending = this.#pending.get(id);
        if (!pending) return;
        clearTimeout(pending.timer);
        this.#pending.delete(id);
        pending.reject(
          error instanceof Error ? error : new Error(String(error)),
        );
      },
    );
    return promise;
  }

  notify(method: string, params?: unknown): void {
    if (!this.#port) return;
    if (!this.#state.helloComplete && method !== "bridge.goodbye") {
      if (this.#queuedNotifications.length >= 256)
        this.#queuedNotifications.shift();
      this.#queuedNotifications.push({ method, params });
      return;
    }
    void this.#send({ jsonrpc: "2.0", method, params }).catch(() => undefined);
  }

  emitEvent(type: string, payload: Record<string, unknown> = {}): void {
    const { sessionId, tabId, documentEpoch, ...eventPayload } = payload;
    this.notify("bridge.event", {
      type,
      time: new Date().toISOString(),
      sessionId,
      tabId,
      documentEpoch,
      payload: eventPayload,
    });
  }

  #connect(): void {
    if (this.#stopped || this.#port) return;
    try {
      const port = chrome.runtime.connectNative(NATIVE_HOST);
      this.#port = port;
      this.#state = {
        connected: true,
        helloComplete: false,
        reconnectAttempt: this.#state.reconnectAttempt,
      };
      this.onState(this.state);
      port.onMessage.addListener(
        (message: unknown) => void this.#onMessage(message),
      );
      port.onDisconnect.addListener(() => void this.#onDisconnect());
      void this.request("bridge.hello", {
        protocolVersion: PROTOCOL_VERSION,
        browserInstanceId: this.#instanceId,
        chromeVersion: /(?:Chrome|Chromium)\/([\d.]+)/.exec(
          navigator.userAgent,
        )?.[1],
        extensionVersion: chrome.runtime.getManifest().version,
        extensionId: chrome.runtime.id,
        capabilities: Object.fromEntries(
          [
            "tabs",
            "windows",
            "history",
            "downloads",
            "screenshots",
            "aiDom",
            "accessibility",
            "semanticLocator",
            "cdpInput",
            "oopif",
            "dialogs",
            "fileChooser",
            "clipboard",
            "secureInput",
            "contentExport",
            "pageAssets",
            "unsafe.evaluate",
            "unsafe.cdp",
          ].map((capability) => [capability, true]),
        ),
      }).then(
        () => {
          this.#state = {
            connected: true,
            helloComplete: true,
            reconnectAttempt: 0,
          };
          this.onState(this.state);
          this.#flushNotifications();
        },
        (error: unknown) => {
          this.#state = {
            ...this.#state,
            lastError: error instanceof Error ? error.message : String(error),
          };
          this.onState(this.state);
          port.disconnect();
        },
      );
    } catch (error) {
      this.#state = {
        ...this.#state,
        connected: false,
        lastError: error instanceof Error ? error.message : String(error),
      };
      this.onState(this.state);
      this.#scheduleReconnect();
    }
  }

  async #send(message: JsonRpcMessage): Promise<void> {
    if (!this.#port)
      throw new RpcError(
        "EXTENSION_DISCONNECTED",
        "native host is not connected",
        { retryable: true },
      );
    const port = this.#port;
    for (const envelope of await encodeMessage(message)) {
      if (this.#port !== port)
        throw new RpcError(
          "EXTENSION_DISCONNECTED",
          "native host disconnected while sending",
          { retryable: true },
        );
      port.postMessage(envelope);
    }
  }

  async #onMessage(raw: unknown): Promise<void> {
    let message: JsonRpcMessage | undefined;
    try {
      message = await this.#assembler.accept(raw);
    } catch (error) {
      this.emitEvent("bridge.decodeError", {
        message: error instanceof Error ? error.message : String(error),
      });
      return;
    }
    if (!message) return;
    if ("method" in message) {
      const request = message as JsonRpcRequest | JsonRpcNotification;
      if ("id" in request) {
        void this.#respond(request);
      } else {
        void this.handleNotification(request.method, request.params).catch(
          () => undefined,
        );
      }
      return;
    }
    if (!("id" in message)) return;
    const pending = this.#pending.get(message.id);
    if (!pending) return;
    clearTimeout(pending.timer);
    this.#pending.delete(message.id);
    if ("error" in message) {
      const { kind, retryable, effect, ...details } = message.error.data;
      pending.reject(
        new RpcError(message.error.data.kind, message.error.message, {
          retryable,
          effect,
          details,
        }),
      );
    } else {
      pending.resolve(message.result);
    }
  }

  async #respond(request: JsonRpcRequest): Promise<void> {
    try {
      const result = await this.handleRequest(request.method, request.params);
      await this.#send({ jsonrpc: "2.0", id: request.id, result });
    } catch (error) {
      await this.#send({
        jsonrpc: "2.0",
        id: request.id,
        error: asRpcError(error).toJSON(),
      });
    }
  }

  async #onDisconnect(): Promise<void> {
    const reason =
      chrome.runtime.lastError?.message ?? "native host disconnected";
    this.#port = undefined;
    this.#assembler.clear();
    this.#queuedNotifications.length = 0;
    this.#rejectPending(
      new RpcError("EXTENSION_DISCONNECTED", reason, { retryable: true }),
    );
    this.#state = {
      connected: false,
      helloComplete: false,
      lastError: reason,
      reconnectAttempt: this.#state.reconnectAttempt + 1,
    };
    this.onState(this.state);
    await this.onDisconnected();
    this.#scheduleReconnect();
  }

  #scheduleReconnect(): void {
    if (this.#stopped || this.#reconnectTimer) return;
    const attempt = Math.max(1, this.#state.reconnectAttempt);
    const delay =
      Math.min(30_000, 500 * 2 ** Math.min(attempt - 1, 6)) +
      Math.floor(Math.random() * 250);
    this.#reconnectTimer = setTimeout(() => {
      this.#reconnectTimer = undefined;
      this.#connect();
    }, delay) as unknown as number;
  }

  #rejectPending(error: Error): void {
    for (const pending of this.#pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.#pending.clear();
  }

  #flushNotifications(): void {
    const queued = this.#queuedNotifications.splice(0);
    for (const notification of queued)
      this.notify(notification.method, notification.params);
  }
}
