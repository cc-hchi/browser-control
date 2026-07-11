import { RpcError } from "./shared.js";
import {
  sanitizeAccessibilitySnapshot,
  sanitizeDomSnapshot,
} from "./redaction.js";

type EventSink = (type: string, payload: Record<string, unknown>) => void;

type SessionDebuggee = chrome.debugger.Debuggee & { sessionId?: string };

interface CaptureClip {
  x: number;
  y: number;
  width: number;
  height: number;
  scale: number;
}

function sessionDebuggee(
  tabId: number,
  sessionId?: string,
): chrome.debugger.Debuggee {
  return {
    tabId,
    ...(sessionId ? { sessionId } : {}),
  } as chrome.debugger.Debuggee;
}

interface TargetSession {
  sessionId: string;
  targetId: string;
  type: string;
  url: string;
  parentSessionId?: string;
}

export interface DialogState {
  type: string;
  message: string;
  defaultPrompt?: string;
  url?: string;
  openedAt: string;
}

export interface FileChooserState {
  backendNodeId: number;
  mode: string;
  frameId?: string;
  sessionId?: string;
  openedAt: string;
}

interface DebuggerState {
  chromeTabId: number;
  sessions: Map<string, TargetSession>;
  frames: Map<string, Record<string, unknown>>;
  contexts: Map<string, Record<string, unknown>>;
  dialog?: DialogState;
  fileChooser?: FileChooserState;
  console: Array<Record<string, unknown>>;
}

export class DebuggerManager {
  readonly #states = new Map<number, DebuggerState>();

  constructor(private readonly emit: EventSink) {
    chrome.debugger.onEvent.addListener(
      (source, method, params) =>
        void this.#onEvent(source, method, params ?? {}),
    );
    chrome.debugger.onDetach.addListener((source, reason) =>
      this.#onDetach(source, reason),
    );
  }

  isAttached(chromeTabId: number): boolean {
    return this.#states.has(chromeTabId);
  }

  async attach(chromeTabId: number): Promise<void> {
    if (this.#states.has(chromeTabId)) return;
    const state: DebuggerState = {
      chromeTabId,
      sessions: new Map(),
      frames: new Map(),
      contexts: new Map(),
      console: [],
    };
    try {
      await chrome.debugger.attach({ tabId: chromeTabId }, "1.3");
    } catch (error) {
      throw new RpcError(
        "DEBUGGER_CONFLICT",
        error instanceof Error ? error.message : String(error),
        { retryable: true },
      );
    }
    this.#states.set(chromeTabId, state);
    try {
      await this.#enableTarget({ tabId: chromeTabId });
      await this.send(chromeTabId, "Target.setAutoAttach", {
        autoAttach: true,
        waitForDebuggerOnStart: false,
        flatten: true,
        filter: [
          { type: "iframe", exclude: false },
          { type: "page", exclude: true },
          { type: "worker", exclude: true },
        ],
      });
      const frameTree = (await this.send(chromeTabId, "Page.getFrameTree")) as {
        frameTree?: { frame?: Record<string, unknown> };
      };
      if (frameTree.frameTree?.frame?.id)
        state.frames.set(
          String(frameTree.frameTree.frame.id),
          frameTree.frameTree.frame,
        );
    } catch (error) {
      await this.detach(chromeTabId);
      throw error;
    }
  }

  async detach(chromeTabId: number): Promise<void> {
    this.#states.delete(chromeTabId);
    try {
      await chrome.debugger.detach({ tabId: chromeTabId });
    } catch {
      // Idempotent detach.
    }
  }

  async detachAll(): Promise<void> {
    await Promise.allSettled(
      Array.from(this.#states.keys(), (tabId) => this.detach(tabId)),
    );
  }

  async send(
    chromeTabId: number,
    method: string,
    params: Record<string, unknown> = {},
    sessionId?: string,
  ): Promise<unknown> {
    if (!this.#states.has(chromeTabId))
      throw new RpcError("DEBUGGER_DETACHED", "debugger is not attached", {
        retryable: true,
      });
    try {
      return await chrome.debugger.sendCommand(
        sessionDebuggee(chromeTabId, sessionId),
        method,
        params,
      );
    } catch (error) {
      throw new RpcError(
        "DEBUGGER_DETACHED",
        `${method}: ${error instanceof Error ? error.message : String(error)}`,
        {
          retryable: true,
        },
      );
    }
  }

  async screenshot(
    chromeTabId: number,
    options: {
      fullPage?: boolean;
      format?: "png" | "jpeg" | "webp";
      quality?: number;
      clip?: {
        x: number;
        y: number;
        width: number;
        height: number;
        scale?: number;
      };
    } = {},
  ): Promise<{
    data?: string;
    tiles?: Array<{
      data: string;
      clip: {
        x: number;
        y: number;
        width: number;
        height: number;
        scale: number;
      };
      pixelWidth?: number;
      pixelHeight?: number;
    }>;
    mimeType: string;
    width?: number;
    height?: number;
    pixelWidth?: number;
    pixelHeight?: number;
  }> {
    const format = options.format ?? "png";
    let viewportSize: { width: number; height: number } | undefined;
    let clip: CaptureClip | undefined;
    if (options.clip) {
      const { x = 0, y = 0, width, height, scale = 1 } = options.clip;
      if (
        typeof width !== "number" ||
        typeof height !== "number" ||
        ![x, y, width, height, scale].every(Number.isFinite) ||
        width <= 0 ||
        height <= 0 ||
        scale <= 0
      ) {
        throw new RpcError(
          "INVALID_REQUEST",
          "screenshot clip must have finite positive width, height, and scale",
        );
      }
      clip = { x, y, width, height, scale };
    }
    if (!clip) {
      const metrics = (await this.send(
        chromeTabId,
        "Page.getLayoutMetrics",
      )) as {
        cssContentSize?: {
          x: number;
          y: number;
          width: number;
          height: number;
        };
        cssVisualViewport?: { clientWidth: number; clientHeight: number };
        cssLayoutViewport?: { clientWidth: number; clientHeight: number };
      };
      const viewport = metrics.cssVisualViewport ?? metrics.cssLayoutViewport;
      if (viewport)
        viewportSize = {
          width: viewport.clientWidth,
          height: viewport.clientHeight,
        };
      if (options.fullPage && metrics.cssContentSize)
        clip = { ...metrics.cssContentSize, scale: 1 };
    }
    const maxTileDimension = 8_000;
    if (
      clip &&
      (clip.height > maxTileDimension || clip.width > maxTileDimension)
    ) {
      const tiles: Array<{
        data: string;
        clip: {
          x: number;
          y: number;
          width: number;
          height: number;
          scale: number;
        };
        pixelWidth?: number;
        pixelHeight?: number;
      }> = [];
      for (let y = clip.y; y < clip.y + clip.height; y += maxTileDimension) {
        for (let x = clip.x; x < clip.x + clip.width; x += maxTileDimension) {
          const tileClip = {
            x,
            y,
            width: Math.min(maxTileDimension, clip.x + clip.width - x),
            height: Math.min(maxTileDimension, clip.y + clip.height - y),
            scale: clip.scale,
          };
          const tile = (await this.send(chromeTabId, "Page.captureScreenshot", {
            format,
            quality: format === "png" ? undefined : (options.quality ?? 90),
            fromSurface: true,
            captureBeyondViewport: true,
            clip: tileClip,
          })) as { data: string };
          const pixels =
            format === "png" ? pngDimensions(tile.data) : undefined;
          tiles.push({
            data: tile.data,
            clip: tileClip,
            pixelWidth: pixels?.width,
            pixelHeight: pixels?.height,
          });
        }
      }
      return {
        tiles,
        mimeType: `image/${format}`,
        width: clip.width,
        height: clip.height,
      };
    }
    const result = (await this.send(chromeTabId, "Page.captureScreenshot", {
      format,
      quality: format === "png" ? undefined : (options.quality ?? 90),
      fromSurface: true,
      captureBeyondViewport: Boolean(options.fullPage || clip),
      clip,
    })) as { data: string };
    const pixels = format === "png" ? pngDimensions(result.data) : undefined;
    return {
      data: result.data,
      mimeType: `image/${format}`,
      width: clip?.width ?? viewportSize?.width,
      height: clip?.height ?? viewportSize?.height,
      pixelWidth: pixels?.width,
      pixelHeight: pixels?.height,
    };
  }

  async accessibilitySnapshot(chromeTabId: number): Promise<unknown[]> {
    const state = this.#state(chromeTabId);
    const sources: Array<string | undefined> = [
      undefined,
      ...state.sessions.keys(),
    ];
    const trees: unknown[] = [];
    for (const sessionId of sources) {
      try {
        const tree = await this.send(
          chromeTabId,
          "Accessibility.getFullAXTree",
          {},
          sessionId,
        );
        trees.push({ sessionId, tree: sanitizeAccessibilitySnapshot(tree) });
      } catch {
        // Some target types do not implement Accessibility.
      }
    }
    return trees;
  }

  async domSnapshot(chromeTabId: number): Promise<unknown[]> {
    const state = this.#state(chromeTabId);
    const sources: Array<string | undefined> = [
      undefined,
      ...state.sessions.keys(),
    ];
    const snapshots: unknown[] = [];
    for (const sessionId of sources) {
      let snapshot: unknown;
      try {
        snapshot = await this.send(
          chromeTabId,
          "DOMSnapshot.captureSnapshot",
          {
            computedStyles: [],
            includeDOMRects: true,
            includePaintOrder: true,
            includeBlendedBackgroundColors: false,
          },
          sessionId,
        );
      } catch {
        // Ignore targets without a DOM domain.
        continue;
      }
      try {
        snapshots.push({ sessionId, snapshot: sanitizeDomSnapshot(snapshot) });
      } catch (error) {
        throw new RpcError(
          "REDACTION_FAILED",
          `DOM snapshot did not match the safe redaction schema: ${
            error instanceof Error ? error.message : String(error)
          }`,
        );
      }
    }
    return snapshots;
  }

  targetGraph(chromeTabId: number): Record<string, unknown> {
    const state = this.#state(chromeTabId);
    return {
      targets: Array.from(state.sessions.values()),
      frames: Array.from(state.frames.values()),
      contexts: Array.from(state.contexts.values()),
    };
  }

  getDialog(chromeTabId: number): DialogState | undefined {
    return this.#state(chromeTabId).dialog;
  }

  async respondDialog(
    chromeTabId: number,
    accept: boolean,
    promptText?: string,
  ): Promise<void> {
    await this.send(chromeTabId, "Page.handleJavaScriptDialog", {
      accept,
      promptText,
    });
    this.#state(chromeTabId).dialog = undefined;
  }

  getFileChooser(chromeTabId: number): FileChooserState | undefined {
    return this.#state(chromeTabId).fileChooser;
  }

  async setFileChooserFiles(
    chromeTabId: number,
    files: string[],
  ): Promise<void> {
    const chooser = this.#state(chromeTabId).fileChooser;
    if (!chooser)
      throw new RpcError("INVALID_REQUEST", "no file chooser is waiting");
    await this.send(
      chromeTabId,
      "DOM.setFileInputFiles",
      { files, backendNodeId: chooser.backendNodeId },
      chooser.sessionId,
    );
    this.#state(chromeTabId).fileChooser = undefined;
  }

  consoleEntries(
    chromeTabId: number,
    cursor = 0,
  ): { cursor: number; entries: Array<Record<string, unknown>> } {
    const entries = this.#state(chromeTabId).console;
    return {
      cursor: entries.length,
      entries: entries.slice(Math.max(0, cursor)),
    };
  }

  async dispatchClick(
    chromeTabId: number,
    x: number,
    y: number,
    count = 1,
  ): Promise<void> {
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseMoved",
      x,
      y,
    });
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mousePressed",
      x,
      y,
      button: "left",
      clickCount: count,
    });
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseReleased",
      x,
      y,
      button: "left",
      clickCount: count,
    });
  }

  async dispatchHover(
    chromeTabId: number,
    x: number,
    y: number,
  ): Promise<void> {
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseMoved",
      x,
      y,
    });
  }

  async dispatchMove(
    chromeTabId: number,
    x: number,
    y: number,
    buttons = 0,
  ): Promise<void> {
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseMoved",
      x,
      y,
      buttons,
    });
  }

  async dispatchScroll(
    chromeTabId: number,
    x: number,
    y: number,
    deltaX: number,
    deltaY: number,
  ): Promise<void> {
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseWheel",
      x,
      y,
      deltaX,
      deltaY,
    });
  }

  async dispatchDrag(
    chromeTabId: number,
    from: { x: number; y: number },
    to: { x: number; y: number },
    steps = 10,
  ): Promise<void> {
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseMoved",
      ...from,
    });
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mousePressed",
      ...from,
      button: "left",
      clickCount: 1,
    });
    for (let index = 1; index <= steps; index += 1) {
      const t = index / steps;
      await this.send(chromeTabId, "Input.dispatchMouseEvent", {
        type: "mouseMoved",
        x: from.x + (to.x - from.x) * t,
        y: from.y + (to.y - from.y) * t,
        button: "left",
        buttons: 1,
      });
    }
    await this.send(chromeTabId, "Input.dispatchMouseEvent", {
      type: "mouseReleased",
      ...to,
      button: "left",
      clickCount: 1,
    });
  }

  async dispatchKey(chromeTabId: number, key: string): Promise<void> {
    const modifiers = { Alt: 1, Control: 2, Meta: 4, Shift: 8 } as const;
    const aliases: Record<string, keyof typeof modifiers> = {
      Alt: "Alt",
      Option: "Alt",
      Control: "Control",
      Ctrl: "Control",
      Meta: "Meta",
      Cmd: "Meta",
      Command: "Meta",
      Shift: "Shift",
    };
    const definitions: Record<
      string,
      { key: string; code: string; virtualKeyCode: number }
    > = {
      Enter: { key: "Enter", code: "Enter", virtualKeyCode: 13 },
      Tab: { key: "Tab", code: "Tab", virtualKeyCode: 9 },
      Escape: { key: "Escape", code: "Escape", virtualKeyCode: 27 },
      Esc: { key: "Escape", code: "Escape", virtualKeyCode: 27 },
      Backspace: { key: "Backspace", code: "Backspace", virtualKeyCode: 8 },
      Delete: { key: "Delete", code: "Delete", virtualKeyCode: 46 },
      ArrowLeft: { key: "ArrowLeft", code: "ArrowLeft", virtualKeyCode: 37 },
      ArrowUp: { key: "ArrowUp", code: "ArrowUp", virtualKeyCode: 38 },
      ArrowRight: { key: "ArrowRight", code: "ArrowRight", virtualKeyCode: 39 },
      ArrowDown: { key: "ArrowDown", code: "ArrowDown", virtualKeyCode: 40 },
      Home: { key: "Home", code: "Home", virtualKeyCode: 36 },
      End: { key: "End", code: "End", virtualKeyCode: 35 },
      PageUp: { key: "PageUp", code: "PageUp", virtualKeyCode: 33 },
      PageDown: { key: "PageDown", code: "PageDown", virtualKeyCode: 34 },
      Space: { key: " ", code: "Space", virtualKeyCode: 32 },
    };
    const parts = key
      .split("+")
      .map((part) => part.trim())
      .filter(Boolean);
    const primaryText = parts.pop() ?? "";
    if (!primaryText)
      throw new RpcError("INVALID_REQUEST", "press requires a non-empty key");
    let mask = 0;
    for (const modifier of parts) {
      const normalized = aliases[modifier];
      if (!normalized)
        throw new RpcError(
          "INVALID_REQUEST",
          `unknown key modifier: ${modifier}`,
        );
      mask |= modifiers[normalized];
    }
    const character = primaryText.length === 1 ? primaryText : undefined;
    const definition = definitions[primaryText] ?? {
      key: primaryText,
      code:
        character && /[a-z]/i.test(character)
          ? `Key${character.toUpperCase()}`
          : character && /\d/.test(character)
            ? `Digit${character}`
            : primaryText,
      virtualKeyCode: character ? character.toUpperCase().charCodeAt(0) : 0,
    };
    const common = {
      key: definition.key,
      code: definition.code,
      modifiers: mask,
      windowsVirtualKeyCode: definition.virtualKeyCode,
      nativeVirtualKeyCode: definition.virtualKeyCode,
    };
    await this.send(chromeTabId, "Input.dispatchKeyEvent", {
      type: character && mask === 0 ? "keyDown" : "rawKeyDown",
      ...common,
      text: character && mask === 0 ? character : undefined,
      unmodifiedText: character,
    });
    await this.send(chromeTabId, "Input.dispatchKeyEvent", {
      type: "keyUp",
      ...common,
    });
  }

  async dispatchText(chromeTabId: number, text: string): Promise<void> {
    await this.send(chromeTabId, "Input.insertText", { text });
  }

  #state(chromeTabId: number): DebuggerState {
    const state = this.#states.get(chromeTabId);
    if (!state)
      throw new RpcError("DEBUGGER_DETACHED", "debugger is not attached", {
        retryable: true,
      });
    return state;
  }

  async #enableTarget(debuggee: chrome.debugger.Debuggee): Promise<void> {
    const send = async (
      method: string,
      params: Record<string, unknown> = {},
    ): Promise<void> => {
      try {
        await chrome.debugger.sendCommand(debuggee, method, params);
      } catch {
        // A target can disappear while domains are being enabled.
      }
    };
    await Promise.all([
      send("Page.enable"),
      send("Runtime.enable"),
      send("DOM.enable"),
      send("Accessibility.enable"),
      send("Log.enable"),
      send("Page.setLifecycleEventsEnabled", { enabled: true }),
      send("Page.setInterceptFileChooserDialog", { enabled: true }),
    ]);
  }

  async #onEvent(
    source: chrome.debugger.Debuggee,
    method: string,
    params: object,
  ): Promise<void> {
    const sessionSource = source as SessionDebuggee;
    const tabId = source.tabId;
    if (tabId === undefined) return;
    const state = this.#states.get(tabId);
    if (!state) return;
    const payload = params as Record<string, any>;
    switch (method) {
      case "Target.attachedToTarget": {
        const sessionId = String(payload.sessionId);
        const target = payload.targetInfo as Record<string, unknown>;
        state.sessions.set(sessionId, {
          sessionId,
          targetId: String(target.targetId ?? ""),
          type: String(target.type ?? "unknown"),
          url: String(target.url ?? ""),
          parentSessionId: sessionSource.sessionId,
        });
        await this.#enableTarget(sessionDebuggee(tabId, sessionId));
        await chrome.debugger
          .sendCommand(
            sessionDebuggee(tabId, sessionId),
            "Target.setAutoAttach",
            {
              autoAttach: true,
              waitForDebuggerOnStart: false,
              flatten: true,
              filter: [
                { type: "iframe", exclude: false },
                { type: "page", exclude: true },
                { type: "worker", exclude: true },
              ],
            },
          )
          .catch(() => undefined);
        break;
      }
      case "Target.detachedFromTarget":
        state.sessions.delete(String(payload.sessionId));
        for (const key of Array.from(state.contexts.keys())) {
          if (key.startsWith(`${String(payload.sessionId)}:`))
            state.contexts.delete(key);
        }
        break;
      case "Runtime.executionContextCreated": {
        const context = payload.context as Record<string, unknown>;
        state.contexts.set(
          `${sessionSource.sessionId ?? "root"}:${String(context.id)}`,
          { ...context, sessionId: sessionSource.sessionId },
        );
        break;
      }
      case "Runtime.executionContextDestroyed":
        state.contexts.delete(
          `${sessionSource.sessionId ?? "root"}:${String(payload.executionContextId)}`,
        );
        break;
      case "Page.frameNavigated": {
        const frame = payload.frame as Record<string, unknown>;
        state.frames.set(String(frame.id), {
          ...frame,
          sessionId: sessionSource.sessionId,
        });
        break;
      }
      case "Page.frameDetached":
        state.frames.delete(String(payload.frameId));
        break;
      case "Page.frameStartedLoading":
        this.emit("navigation.started", {
          chromeTabId: tabId,
          frameId: payload.frameId,
          sessionId: sessionSource.sessionId,
        });
        break;
      case "Page.lifecycleEvent":
        if (payload.name === "load" || payload.name === "networkIdle") {
          this.emit("navigation.lifecycle", {
            chromeTabId: tabId,
            frameId: payload.frameId,
            lifecycle: payload.name,
            loaderId: payload.loaderId,
            sessionId: sessionSource.sessionId,
          });
        }
        break;
      case "Page.javascriptDialogOpening":
        state.dialog = {
          type: String(payload.type),
          message: String(payload.message),
          defaultPrompt: payload.defaultPrompt
            ? String(payload.defaultPrompt)
            : undefined,
          url: payload.url ? String(payload.url) : undefined,
          openedAt: new Date().toISOString(),
        };
        this.emit("dialog.opened", {
          chromeTabId: tabId,
          dialog: state.dialog,
        });
        break;
      case "Page.javascriptDialogClosed":
        state.dialog = undefined;
        this.emit("dialog.closed", {
          chromeTabId: tabId,
          result: payload.result,
        });
        break;
      case "Page.fileChooserOpened":
        state.fileChooser = {
          backendNodeId: Number(payload.backendNodeId),
          mode: String(payload.mode),
          frameId: payload.frameId ? String(payload.frameId) : undefined,
          sessionId: sessionSource.sessionId,
          openedAt: new Date().toISOString(),
        };
        this.emit("fileChooser.opened", {
          chromeTabId: tabId,
          fileChooser: state.fileChooser,
        });
        break;
      case "Runtime.consoleAPICalled": {
        const entry = {
          level: payload.type,
          timestamp: payload.timestamp,
          args: Array.isArray(payload.args)
            ? payload.args.map(
                (arg: Record<string, unknown>) =>
                  arg.value ?? arg.description ?? arg.type,
              )
            : [],
          sessionId: sessionSource.sessionId,
        };
        state.console.push(entry);
        if (state.console.length > 1_000)
          state.console.splice(0, state.console.length - 1_000);
        this.emit("console.entry", { chromeTabId: tabId, entry });
        break;
      }
      case "Log.entryAdded": {
        const raw = payload.entry as Record<string, unknown>;
        const entry = {
          level: raw.level,
          source: raw.source,
          timestamp: raw.timestamp,
          text:
            typeof raw.text === "string" ? raw.text.slice(0, 4_096) : undefined,
          sessionId: sessionSource.sessionId,
        };
        state.console.push(entry);
        if (state.console.length > 1_000)
          state.console.splice(0, state.console.length - 1_000);
        this.emit("console.entry", { chromeTabId: tabId, entry });
        break;
      }
    }
  }

  #onDetach(source: chrome.debugger.Debuggee, reason: string): void {
    if (source.tabId === undefined) return;
    const existed = this.#states.delete(source.tabId);
    if (existed)
      this.emit("debugger.detached", { chromeTabId: source.tabId, reason });
  }
}

function pngDimensions(
  base64: string,
): { width: number; height: number } | undefined {
  try {
    const binary = atob(base64.slice(0, 40));
    if (binary.length < 24 || binary.slice(1, 4) !== "PNG") return undefined;
    const read = (offset: number): number =>
      ((binary.charCodeAt(offset) << 24) >>> 0) |
      (binary.charCodeAt(offset + 1) << 16) |
      (binary.charCodeAt(offset + 2) << 8) |
      binary.charCodeAt(offset + 3);
    return { width: read(16) >>> 0, height: read(20) >>> 0 };
  } catch {
    return undefined;
  }
}
