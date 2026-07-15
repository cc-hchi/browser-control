import type {
  Locator,
  NodeDescription,
  RuntimeAction,
  RuntimeSnapshot,
  SnapshotOptions,
} from "../../../packages/locator-runtime/src/types.js";
import { DebuggerManager } from "./debugger-manager.js";
import type { BridgeState, NativeBridge } from "./native-bridge.js";
import { RpcError, record, requiredString, randomId } from "./shared.js";
import { TabRegistry, type TabState } from "./tab-registry.js";

interface FrameSnapshot {
  frameId: number;
  documentId?: string;
  localSnapshotId: string;
  snapshot: RuntimeSnapshot;
}

interface SnapshotState {
  snapshotId: string;
  tabId: string;
  documentEpoch: number;
  createdAt: number;
  frames: Map<number, FrameSnapshot>;
}

interface Confirmation {
  confirmationId: string;
  title?: string;
  summary?: string;
  origin?: string;
  expiresAt?: string;
  sessionName?: string;
  clientId?: string;
  [key: string]: unknown;
}

interface SecureInputRequest {
  requestId: string;
  tabId: string;
  sessionId: string;
  leaseId: string;
  documentEpoch: number;
  frameId: number;
  elementIdentity: string;
  target: { locator: Locator } | { snapshotId: string; nodeRef: string };
  label?: string;
  origin: string;
  secret: boolean;
  autocomplete?: string;
  createdAt: number;
}

interface ServiceEvent {
  type: string;
  time: string;
  [key: string]: unknown;
}

interface EventWaiter {
  type: string;
  tabId?: string;
  predicate: (event: ServiceEvent) => boolean;
  resolve: (event: ServiceEvent) => void;
  reject: (error: Error) => void;
  timer: number;
}

interface DownloadWaiter {
  tabId: string;
  sessionId?: string;
  downloadId?: number;
  resolve: (event: ServiceEvent) => void;
  reject: (error: Error) => void;
  timer: number;
}

interface FrameResult<T> {
  frameId: number;
  documentId?: string;
  result?: T;
  error?: string;
}

const MAX_SNAPSHOTS = 16;
const MAX_INLINE_ARTIFACT_BYTES = 64 * 1024 * 1024;

function publicDownloadId(id: number): string {
  return `download_${id}`;
}

function chromeDownloadId(id: unknown): number {
  if (typeof id === "number") return id;
  const match = /^download_(\d+)$/.exec(String(id));
  if (!match?.[1]) throw new RpcError("INVALID_REQUEST", "invalid downloadId");
  return Number(match[1]);
}

function downloadToPublic(
  item: chrome.downloads.DownloadItem,
): Record<string, unknown> {
  return {
    downloadId: publicDownloadId(item.id),
    url: redactUrl(item.url),
    finalUrl: redactUrl(item.finalUrl),
    fileName: item.filename,
    mimeType: item.mime,
    state: item.state,
    danger: item.danger,
    error: item.error,
    paused: item.paused,
    bytesReceived: item.bytesReceived,
    totalBytes: item.totalBytes,
    startTime: item.startTime,
    endTime: item.endTime,
  };
}

function locatorFrom(value: unknown): Locator {
  const locator = record(value) as unknown as Locator;
  if (!locator.by)
    throw new RpcError("INVALID_REQUEST", "locator.by is required");
  return locator;
}

export class ExtensionService {
  readonly #registry = new TabRegistry();
  readonly #debugger = new DebuggerManager((type, payload) =>
    this.#onDebuggerEvent(type, payload),
  );
  readonly #snapshots = new Map<string, SnapshotState>();
  readonly #waiters = new Set<EventWaiter>();
  readonly #downloadWaiters = new Set<DownloadWaiter>();
  readonly #downloadOwners = new Map<
    number,
    { tabId: string; sessionId?: string }
  >();
  readonly #confirmations = new Map<string, Confirmation>();
  readonly #confirmationTimers = new Map<string, number>();
  readonly #secureInputs = new Map<string, SecureInputRequest>();
  #bridge?: NativeBridge;
  #bridgeState: BridgeState = {
    connected: false,
    helloComplete: false,
    reconnectAttempt: 0,
  };

  async initialize(): Promise<void> {
    await this.#registry.initialize();
    this.#registerChromeEvents();
    chrome.runtime.onMessage.addListener(
      (message: unknown, sender, sendResponse) => {
        if (sender.id !== chrome.runtime.id) return false;
        const envelope = message as Record<string, unknown>;
        if (envelope.target === "browser-control-offscreen") return false;
        void this.#handleRuntimeMessage(envelope, sender).then(
          sendResponse,
          (error: unknown) => {
            sendResponse({
              error: error instanceof Error ? error.message : String(error),
            });
          },
        );
        return true;
      },
    );
    await this.#updateBadge();
  }

  attachBridge(bridge: NativeBridge): void {
    this.#bridge = bridge;
  }

  updateBridgeState(state: BridgeState): void {
    this.#bridgeState = state;
    void chrome.action.setTitle({
      title: state.helloComplete
        ? "Local Chrome Control — connected"
        : "Local Chrome Control — disconnected",
    });
  }

  async failClosed(): Promise<void> {
    await this.stopAll("bridge-disconnected", false);
  }

  handleNotification = async (
    method: string,
    params: unknown,
  ): Promise<unknown> => {
    if (method === "bridge.event") {
      const event = record(params);
      if (event.type === "confirmation.requested") {
        const raw = record(event.confirmation ?? event.payload ?? event);
        const confirmationId = requiredString(raw, "confirmationId");
        this.#confirmations.set(confirmationId, { ...raw, confirmationId });
        this.#scheduleConfirmationExpiry(
          confirmationId,
          typeof raw.expiresAt === "string" ? raw.expiresAt : undefined,
        );
      }
      if (event.type === "confirmation.resolved") {
        const id = String(
          record(event.confirmation ?? event.payload ?? event).confirmationId ??
            "",
        );
        this.#removeConfirmation(id);
      }
      await this.#updateBadge();
      return { accepted: true };
    }
    throw new RpcError(
      "METHOD_NOT_FOUND",
      `unknown bridge notification: ${method}`,
    );
  };

  handleRequest = async (method: string, params: unknown): Promise<unknown> => {
    const input = params === undefined ? {} : record(params);
    switch (method) {
      case "browser.list":
        return this.#browserList();
      case "browser.history.query":
        return this.#historyQuery(input);
      case "browser.stop":
        return typeof input.sessionId === "string"
          ? this.stopSession(
              input.sessionId,
              typeof input.reason === "string" ? input.reason : "remote-stop",
            )
          : this.stopAll("remote-stop", false);
      case "tab.list":
        return this.#tabList(input);
      case "tab.get":
        return this.#tabGet(input);
      case "tab.open":
        return this.#tabOpen(input);
      case "tab.claim":
        return this.#tabClaim(input);
      case "tab.activate":
        return this.#tabActivate(input);
      case "tab.release":
        return this.#tabRelease(input);
      case "tab.close":
        return this.#tabClose(input);
      case "tab.navigate":
        return this.#tabNavigate(input);
      case "tab.back":
        return this.#tabHistoryAction(input, "back");
      case "tab.forward":
        return this.#tabHistoryAction(input, "forward");
      case "tab.reload":
        return this.#tabHistoryAction(input, "reload");
      case "observation.capture":
        return this.#capture(input);
      case "observation.diff":
        return this.#observationDiff(input);
      case "locator.query":
        return this.#locatorQuery(input);
      case "action.preflight":
        return this.#actionPreflight(input);
      case "action.perform":
        return this.#actionPerform(input);
      case "condition.wait":
        return this.#conditionWait(input);
      case "dialog.get":
        return this.#dialogGet(input);
      case "dialog.respond":
        return this.#dialogRespond(input);
      case "clipboard.read":
        return this.#clipboard("read", input);
      case "clipboard.write":
        return this.#clipboard("write", input);
      case "fileChooser.setFiles":
        return this.#setFiles(input);
      case "download.get":
        return this.#downloadGet(input);
      case "download.wait":
        return this.#downloadWait(input);
      case "content.export":
        return this.#contentExport(input);
      case "pageAssets.list":
        return this.#pageAssets(input, false);
      case "pageAssets.export":
        return this.#pageAssets(input, true);
      case "secureInput.request":
        return this.#secureInputRequest(input);
      case "unsafe.evaluate":
        return this.#unsafeEvaluate(input);
      case "unsafe.cdp.send":
        return this.#unsafeCdp(input);
      default:
        throw new RpcError(
          "METHOD_NOT_FOUND",
          `extension does not implement ${method}`,
        );
    }
  };

  async stopAll(
    reason: string,
    notifyDaemon = true,
  ): Promise<Record<string, unknown>> {
    const claimed = await this.#registry.releaseAll();
    await this.#debugger.detachAll();
    await Promise.allSettled(
      claimed.map((tab) => this.#removeRuntime(tab.chromeTabId)),
    );
    this.#snapshots.clear();
    this.#secureInputs.clear();
    for (const confirmationId of Array.from(this.#confirmations.keys()))
      this.#removeConfirmation(confirmationId);
    for (const waiter of this.#waiters) {
      clearTimeout(waiter.timer);
      waiter.reject(new RpcError("CANCELLED", `control stopped: ${reason}`));
    }
    this.#waiters.clear();
    for (const waiter of this.#downloadWaiters) {
      clearTimeout(waiter.timer);
      waiter.reject(new RpcError("CANCELLED", `control stopped: ${reason}`));
    }
    this.#downloadWaiters.clear();
    this.#downloadOwners.clear();
    await this.#updateBadge();
    this.#emit("emergency.stop", {
      reason,
      releasedTabs: claimed.map((tab) => tab.tabId),
    });
    if (notifyDaemon)
      void this.#bridge
        ?.request("bridge.admin.stopAll", { reason })
        .catch(() => undefined);
    return { stopped: true, releasedTabs: claimed.map((tab) => tab.tabId) };
  }

  async stopSession(
    sessionId: string,
    reason: string,
  ): Promise<Record<string, unknown>> {
    const claimed = this.#registry
      .all()
      .filter((state) => state.claim?.sessionId === sessionId);
    for (const state of claimed)
      this.#cancelExpectationWaiters(
        state.tabId,
        new RpcError("CANCELLED", `session stopped: ${reason}`),
      );
    await Promise.allSettled(claimed.map((state) => this.#releaseLocal(state)));
    for (const [snapshotId, snapshot] of this.#snapshots) {
      if (claimed.some((state) => state.tabId === snapshot.tabId))
        this.#snapshots.delete(snapshotId);
    }
    for (const [confirmationId, confirmation] of this.#confirmations) {
      if (confirmation.sessionId === sessionId)
        this.#removeConfirmation(confirmationId);
    }
    for (const [requestId, request] of this.#secureInputs) {
      if (claimed.some((state) => state.tabId === request.tabId))
        this.#secureInputs.delete(requestId);
    }
    this.#emit("session.stopped", {
      sessionId,
      reason,
      releasedTabs: claimed.map((tab) => tab.tabId),
    });
    return {
      stopped: true,
      sessionId,
      releasedTabs: claimed.map((tab) => tab.tabId),
    };
  }

  async #browserList(): Promise<Record<string, unknown>> {
    const [windows, groups] = await Promise.all([
      chrome.windows.getAll({
        populate: true,
        windowTypes: ["normal", "popup"],
      }),
      chrome.tabGroups.query({}),
    ]);
    const stored = await chrome.storage.local.get("browserInstanceId");
    return {
      browsers: [
        {
          browserInstanceId: stored.browserInstanceId,
          extensionId: chrome.runtime.id,
          extensionVersion: chrome.runtime.getManifest().version,
          connected: this.#bridgeState.helloComplete,
          tabGroups: groups.map((group) => ({
            groupId: group.id,
            windowId: group.windowId,
            title: group.title,
            color: group.color,
            collapsed: group.collapsed,
          })),
          windows: windows.map((window) => ({
            windowId: window.id,
            focused: window.focused,
            incognito: window.incognito,
            state: window.state,
            type: window.type,
            tabs: (window.tabs ?? [])
              .map((tab) => {
                if (tab.id === undefined) return undefined;
                return this.#registry.toPublic(
                  this.#registry.ensure(tab.id),
                  tab,
                );
              })
              .filter(Boolean),
          })),
        },
      ],
    };
  }

  async #historyQuery(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const query: chrome.history.HistoryQuery = {
      text: typeof params.text === "string" ? params.text : "",
      startTime:
        typeof params.startTime === "number" ? params.startTime : undefined,
      endTime: typeof params.endTime === "number" ? params.endTime : undefined,
      maxResults:
        typeof params.maxResults === "number"
          ? Math.min(params.maxResults, 10_000)
          : 100,
    };
    return { items: await chrome.history.search(query) };
  }

  async #tabList(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const query: chrome.tabs.QueryInfo = {};
    if (typeof params.active === "boolean") query.active = params.active;
    if (typeof params.currentWindow === "boolean")
      query.currentWindow = params.currentWindow;
    if (typeof params.windowId === "number") query.windowId = params.windowId;
    const tabs = await chrome.tabs.query(query);
    return {
      tabs: tabs.flatMap((tab) => {
        if (tab.id === undefined) return [];
        return [this.#registry.toPublic(this.#registry.ensure(tab.id), tab)];
      }),
    };
  }

  async #tabGet(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#registry.fromHandle(requiredString(params, "tabId"));
    try {
      const tab = await chrome.tabs.get(state.chromeTabId);
      return this.#registry.toPublic(state, tab);
    } catch {
      throw new RpcError("TAB_CLOSED", "tab no longer exists");
    }
  }

  async #tabOpen(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const tab = await chrome.tabs.create({
      url: typeof params.url === "string" ? params.url : "about:blank",
      active: params.active !== false,
      windowId:
        typeof params.windowId === "number" ? params.windowId : undefined,
      index: typeof params.index === "number" ? params.index : undefined,
      openerTabId:
        typeof params.openerTabId === "number" ? params.openerTabId : undefined,
    });
    if (tab.id === undefined)
      throw new RpcError("INTERNAL", "Chrome did not return a tab id");
    const state = this.#registry.ensure(tab.id, true);
    return this.#registry.toPublic(state, tab);
  }

  async #tabClaim(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const tabId = requiredString(params, "tabId");
    const session =
      params.session && typeof params.session === "object"
        ? record(params.session)
        : {};
    const state = await this.#registry.claim(
      tabId,
      requiredString(params, "sessionId"),
      requiredString(params, "leaseId"),
      {
        sessionName:
          typeof session.name === "string"
            ? session.name.slice(0, 120)
            : undefined,
        clientId:
          typeof session.clientId === "string"
            ? session.clientId.slice(0, 120)
            : undefined,
      },
    );
    try {
      await this.#debugger.attach(state.chromeTabId);
      await this.#ensureRuntime(state.chromeTabId);
    } catch (error) {
      await this.#registry.release(tabId);
      await this.#debugger.detach(state.chromeTabId);
      throw error;
    }
    await this.#updateBadge();
    this.#emit("tab.claimed", {
      tabId,
      sessionId: state.claim?.sessionId,
      documentEpoch: state.documentEpoch,
    });
    return this.#tabGet({ tabId });
  }

  async #tabActivate(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#registry.fromHandle(requiredString(params, "tabId"));
    const tab = await chrome.tabs.update(state.chromeTabId, { active: true });
    if (!tab)
      throw new RpcError(
        "TAB_NOT_FOUND",
        "Chrome did not return the activated tab",
        { retryable: true },
      );
    if (tab.windowId !== undefined)
      await chrome.windows.update(tab.windowId, { focused: true });
    return this.#registry.toPublic(state, tab);
  }

  async #tabRelease(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const tabId = requiredString(params, "tabId");
    const state = this.#registry.assertClaimed(
      tabId,
      typeof params.sessionId === "string" ? params.sessionId : undefined,
      typeof params.leaseId === "string" ? params.leaseId : undefined,
    );
    await this.#debugger.detach(state.chromeTabId);
    await this.#removeRuntime(state.chromeTabId);
    await this.#registry.release(tabId);
    this.#dropTabSnapshots(tabId);
    await this.#updateBadge();
    this.#emit("tab.released", { tabId });
    return { released: true, tabId };
  }

  async #tabClose(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#registry.fromHandle(requiredString(params, "tabId"));
    if (!state.owned)
      throw new RpcError(
        "PERMISSION_DENIED",
        "only session-owned tabs may be closed automatically",
      );
    await chrome.tabs.remove(state.chromeTabId);
    return { closed: true, tabId: state.tabId };
  }

  async #tabNavigate(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const url = requiredString(params, "url");
    const tab = await chrome.tabs.update(state.chromeTabId, { url });
    return { ...this.#registry.toPublic(state, tab), navigationStarted: true };
  }

  async #tabHistoryAction(
    params: Record<string, unknown>,
    action: "back" | "forward" | "reload",
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    if (action === "back") await chrome.tabs.goBack(state.chromeTabId);
    else if (action === "forward")
      await chrome.tabs.goForward(state.chromeTabId);
    else
      await chrome.tabs.reload(state.chromeTabId, {
        bypassCache: params.bypassCache === true,
      });
    return { started: true, tabId: state.tabId, action };
  }

  async #capture(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const options = (
      params.options && typeof params.options === "object"
        ? params.options
        : params
    ) as Record<string, unknown>;
    const runtimeOptions: SnapshotOptions = {
      maxNodes:
        typeof options.maxNodes === "number" ? options.maxNodes : undefined,
      includeHidden: options.includeHidden === true,
      includeText: options.includeText !== false,
    };
    const include = (
      options.include && typeof options.include === "object"
        ? options.include
        : options
    ) as Record<string, unknown>;
    const includeFrameAiDom = include.frameAiDom === true;
    const includeNodeDetails = include.nodeDetails === true;
    const results = await this.#invokeAll<RuntimeSnapshot>(
      state.chromeTabId,
      "capture",
      [runtimeOptions],
    );
    const snapshotId = randomId("snap");
    const frames = new Map<number, FrameSnapshot>();
    const publicFrames: Array<Record<string, unknown>> = [];
    const domParts: string[] = [];
    for (const result of results) {
      if (!result.result) continue;
      const local = result.result;
      const prefix = `f${result.frameId}_`;
      const nodes = local.nodes.map((node) => ({
        ...node,
        nodeRef: `${prefix}${node.nodeRef}`,
      }));
      const aiDom = local.aiDom.replace(
        /\bref=([a-z]\d+)\b/g,
        (_match, ref: string) => `ref=${prefix}${ref}`,
      );
      domParts.push(
        `[frame ${result.frameId} ${String(redactUrl(local.url) ?? "")}]\n${aiDom}`,
      );
      frames.set(result.frameId, {
        frameId: result.frameId,
        documentId: result.documentId,
        localSnapshotId: local.snapshotId,
        snapshot: local,
      });
      publicFrames.push({
        frameId: result.frameId,
        documentId: result.documentId,
        url: redactUrl(local.url),
        title: local.title,
        revision: local.revision,
        viewport: local.viewport,
        ...(includeFrameAiDom ? { aiDom } : {}),
        ...(includeNodeDetails ? { nodes } : {}),
        truncated: local.truncated,
      });
    }
    if (!frames.size)
      throw new RpcError(
        "UNSUPPORTED_PAGE",
        "locator runtime could not be injected into this page",
      );
    this.#storeSnapshot({
      snapshotId,
      tabId: state.tabId,
      documentEpoch: state.documentEpoch,
      createdAt: Date.now(),
      frames,
    });

    const response: Record<string, unknown> = {
      snapshotId,
      tab: await this.#tabGet({ tabId: state.tabId }),
      documentEpoch: state.documentEpoch,
      frames: publicFrames,
      dom: {
        format: "ai-dom",
        text: domParts.join("\n"),
        truncated: publicFrames.some((frame) => frame.truncated === true),
      },
    };
    if (include.screenshot !== false) {
      const rawScreenshot =
        options.screenshot && typeof options.screenshot === "object"
          ? record(options.screenshot)
          : {};
      const screenshotOptions = { ...rawScreenshot } as NonNullable<
        Parameters<DebuggerManager["screenshot"]>[1]
      >;
      if (rawScreenshot.target && typeof rawScreenshot.target === "object") {
        const prepared = await this.#prepareTarget(
          state,
          record(rawScreenshot.target),
        );
        if (!prepared.node.canTranslateToTop)
          throw new RpcError(
            "FRAME_UNAVAILABLE",
            "cannot capture an element whose frame coordinates cannot be translated",
          );
        screenshotOptions.clip = prepared.node.topRect ?? prepared.node.rect;
      }
      await this.#invokeAll(state.chromeTabId, "setSensitiveMask", [true]);
      try {
        response.screenshot = await this.#debugger.screenshot(
          state.chromeTabId,
          screenshotOptions,
        );
      } finally {
        await this.#invokeAll(state.chromeTabId, "setSensitiveMask", [false]);
      }
    }
    if (include.accessibility === true || include.ax === true)
      response.accessibility = await this.#debugger.accessibilitySnapshot(
        state.chromeTabId,
      );
    if (include.fullDom === true)
      response.fullDom = await this.#debugger.domSnapshot(state.chromeTabId);
    if (include.frameGraph === true)
      response.frameGraph = this.#debugger.targetGraph(state.chromeTabId);
    if (include.logs === true)
      response.logs = this.#debugger.consoleEntries(
        state.chromeTabId,
        Number(options.logCursor ?? 0),
      );
    return response;
  }

  async #observationDiff(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const baseSnapshotId = requiredString(params, "snapshotId");
    const base = this.#snapshots.get(baseSnapshotId);
    if (!base)
      throw new RpcError(
        "STALE_REFERENCE",
        "base snapshot is no longer available",
      );
    const current = await this.#capture({ ...params, tabId: base.tabId });
    return {
      baseSnapshotId,
      ...current,
      diff: { mode: "replacement", reason: "frame-safe immutable snapshot" },
    };
  }

  async #locatorQuery(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const locator = locatorFrom(params.locator);
    const snapshotId =
      typeof params.snapshotId === "string" ? params.snapshotId : undefined;
    const nodes = await this.#query(state, locator, snapshotId);
    return { count: nodes.length, nodes };
  }

  async #query(
    state: TabState,
    locator: Locator,
    snapshotId?: string,
  ): Promise<Array<NodeDescription & { frameId: number }>> {
    const frameIds = locator.framePath?.length
      ? await this.#resolveFramePath(state.chromeTabId, locator.framePath)
      : undefined;
    const targetLocator = { ...locator, framePath: undefined };
    if (snapshotId) {
      const snapshot = this.#snapshot(snapshotId, state);
      const output: Array<NodeDescription & { frameId: number }> = [];
      for (const frame of snapshot.frames.values()) {
        if (frameIds && !frameIds.has(frame.frameId)) continue;
        const results = await this.#invokeFrame<NodeDescription[]>(
          state.chromeTabId,
          frame.frameId,
          "query",
          [targetLocator, frame.localSnapshotId],
        );
        for (const node of results ?? [])
          output.push({
            ...node,
            nodeRef: `f${frame.frameId}_${node.nodeRef}`,
            frameId: frame.frameId,
          });
      }
      return output;
    }
    const results = frameIds
      ? await Promise.all(
          Array.from(
            frameIds,
            async (frameId): Promise<FrameResult<NodeDescription[]>> => ({
              frameId,
              result: await this.#invokeFrame<NodeDescription[]>(
                state.chromeTabId,
                frameId,
                "query",
                [targetLocator],
              ),
            }),
          ),
        )
      : await this.#invokeAll<NodeDescription[]>(state.chromeTabId, "query", [
          targetLocator,
        ]);
    return results.flatMap((result) =>
      (result.result ?? []).map((node) => ({
        ...node,
        nodeRef: `f${result.frameId}_${node.nodeRef}`,
        frameId: result.frameId,
      })),
    );
  }

  async #resolveFramePath(
    chromeTabId: number,
    path: Locator[],
  ): Promise<Set<number>> {
    const frames = await chrome.webNavigation.getAllFrames({
      tabId: chromeTabId,
    });
    if (!frames)
      throw new RpcError(
        "FRAME_UNAVAILABLE",
        "Chrome did not return the tab frame tree",
        { retryable: true },
      );
    let parents = new Set<number>([0]);
    for (const component of path) {
      const next = new Set<number>();
      for (const parentFrameId of parents) {
        const matchesBeforeParent = next.size;
        const parent = frames.find((frame) => frame.frameId === parentFrameId);
        const children = frames.filter(
          (frame) => frame.parentFrameId === parentFrameId,
        );
        const locator = { ...component, framePath: undefined };
        const nodes = await this.#invokeFrame<NodeDescription[]>(
          chromeTabId,
          parentFrameId,
          "query",
          [locator],
        );
        for (const node of nodes ?? []) {
          const src = node.attributes.src;
          let candidates = children;
          if (src) {
            try {
              const expected = new URL(src, parent?.url).href;
              candidates = children.filter((frame) => frame.url === expected);
            } catch {
              candidates = [];
            }
          }
          if (candidates.length === 1) next.add(candidates[0]!.frameId);
        }
        if (
          (nodes?.length ?? 0) === 1 &&
          children.length === 1 &&
          next.size === matchesBeforeParent
        )
          next.add(children[0]!.frameId);
      }
      if (!next.size)
        throw new RpcError(
          "FRAME_UNAVAILABLE",
          "framePath did not resolve to an available child frame",
          { retryable: true },
        );
      parents = next;
    }
    return parents;
  }

  async #actionPerform(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const action = record(params.action);
    const type = requiredString(action, "type");
    const preflight =
      params.__preflight && typeof params.__preflight === "object"
        ? record(params.__preflight)
        : await this.#actionPreflight(params);
    if (preflight.confirmationRequired === true) {
      const approvedHash =
        typeof params.__approvedRequestHash === "string"
          ? params.__approvedRequestHash
          : "";
      if (!approvedHash) {
        throw new RpcError(
          "CONFIRMATION_REQUIRED",
          "this action requires approval in the trusted extension UI",
          {
            details: { requestHash: preflight.requestHash },
          },
        );
      }
      if (approvedHash !== preflight.requestHash) {
        throw new RpcError(
          "STALE_REFERENCE",
          "the approved action or resolved target changed before execution",
          {
            retryable: true,
            details: {
              approvedRequestHash: approvedHash,
              actualRequestHash: preflight.requestHash,
            },
          },
        );
      }
    }
    const expectations = Array.isArray(params.expect)
      ? params.expect.map(record)
      : [];
    const waiting = expectations.map((expectation) =>
      this.#waitForExpectation(state, expectation),
    );
    let actionResult: Record<string, unknown>;

    try {
      switch (type) {
        case "navigate":
          actionResult = await this.#tabNavigate({
            ...params,
            url: action.url ?? action.value,
          });
          break;
        case "back":
        case "forward":
        case "reload":
          actionResult = await this.#tabHistoryAction(params, type);
          break;
        case "dialogAccept":
        case "dialogDismiss":
        case "dialogPrompt": {
          const dialog = this.#debugger.getDialog(state.chromeTabId);
          if (!dialog)
            throw new RpcError(
              "INVALID_REQUEST",
              "no JavaScript dialog is open",
            );
          await this.#debugger.respondDialog(
            state.chromeTabId,
            type !== "dialogDismiss",
            type === "dialogPrompt" ? String(action.value ?? "") : undefined,
          );
          actionResult = {
            handled: true,
            accepted: type !== "dialogDismiss",
            dialogType: dialog.type,
          };
          break;
        }
        case "drag": {
          const from = await this.#dragPoint(
            state,
            action.from ?? record(action.target ?? {}).from,
            action.snapshotId,
          );
          const to = await this.#dragPoint(
            state,
            action.to ?? record(action.target ?? {}).to,
            action.snapshotId,
          );
          await this.#debugger.dispatchDrag(
            state.chromeTabId,
            from,
            to,
            typeof action.steps === "number" ? action.steps : 12,
          );
          actionResult = { performed: true, inputMode: "cdp", from, to };
          break;
        }
        case "scroll": {
          const point = action.target
            ? await this.#actionPoint(state, record(action.target))
            : { x: 1, y: 1 };
          const delta = record(action.delta ?? action.value ?? {});
          await this.#debugger.dispatchScroll(
            state.chromeTabId,
            point.x,
            point.y,
            Number(delta.x ?? delta.deltaX ?? 0),
            Number(delta.y ?? delta.deltaY ?? 0),
          );
          actionResult = { performed: true, inputMode: "cdp", point };
          break;
        }
        case "press": {
          if (action.target && typeof action.target === "object") {
            actionResult = await this.#performTargetAction(
              state,
              action,
              type,
              typeof preflight.targetElementIdentity === "string"
                ? preflight.targetElementIdentity
                : undefined,
              typeof preflight.targetFrameId === "number"
                ? preflight.targetFrameId
                : undefined,
            );
          } else {
            await this.#debugger.dispatchKey(
              state.chromeTabId,
              String(action.value ?? action.key ?? ""),
            );
            actionResult = { performed: true, inputMode: "cdp" };
          }
          break;
        }
        case "downloadMedia": {
          const url = requiredString(action, "url");
          const downloadId = await chrome.downloads.download({
            url,
            saveAs: action.saveAs === true,
            filename:
              typeof action.fileName === "string" ? action.fileName : undefined,
          });
          this.#associateDownload(downloadId, state);
          actionResult = {
            performed: true,
            downloadId: publicDownloadId(downloadId),
          };
          break;
        }
        default:
          actionResult = await this.#performTargetAction(
            state,
            action,
            type,
            typeof preflight.targetElementIdentity === "string"
              ? preflight.targetElementIdentity
              : undefined,
            typeof preflight.targetFrameId === "number"
              ? preflight.targetFrameId
              : undefined,
          );
      }
    } catch (error) {
      this.#cancelExpectationWaiters(
        state.tabId,
        error instanceof Error ? error : new Error(String(error)),
      );
      await Promise.allSettled(waiting);
      throw error;
    }

    const expected = waiting.length ? await Promise.all(waiting) : [];
    const response: Record<string, unknown> = {
      performed: true,
      action: actionResult,
      expected,
      documentEpoch: state.documentEpoch,
    };
    if (params.observeAfter && typeof params.observeAfter === "object") {
      response.observation = await this.#capture({
        ...params,
        options: params.observeAfter,
      });
    }
    return response;
  }

  async #actionPreflight(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const action = record(params.action);
    const type = requiredString(action, "type");
    const confirmation =
      params.confirmation && typeof params.confirmation === "object"
        ? record(params.confirmation)
        : {};
    let confirmationRequired = confirmation.required === true;
    let reason =
      typeof confirmation.reason === "string"
        ? confirmation.reason.slice(0, 120)
        : "";
    let targetIdentity: Record<string, unknown> | undefined;
    let targetElementIdentity: string | undefined;
    let targetFrameId: number | undefined;
    let targetLabel = "";
    const key = String(action.value ?? action.key ?? "");
    if (type === "press" && !key) {
      throw new RpcError("INVALID_REQUEST", "press requires value or key");
    }

    if (
      action.target &&
      typeof action.target === "object" &&
      !record(action.target).point
    ) {
      const prepared = await this.#prepareTarget(state, record(action.target));
      const node = prepared.node;
      targetElementIdentity = node.elementIdentity;
      targetFrameId = prepared.frameId;
      targetLabel = String(
        node.name || node.text || node.attributes["aria-label"] || node.tag,
      ).slice(0, 160);
      targetIdentity = {
        frameId: prepared.frameId,
        elementIdentity: node.elementIdentity,
        nodeRef: node.nodeRef,
        tag: node.tag,
        role: node.role,
        name: node.name,
        text: node.text,
        attributes: node.attributes,
      };
      if (["fill", "type"].includes(type) && node.sensitive) {
        throw new RpcError(
          "PERMISSION_DENIED",
          "sensitive fields must be filled through secureInput.request",
        );
      }
      const semanticText = [
        node.name,
        node.text,
        node.attributes.type,
        node.attributes["aria-label"],
        node.attributes.id,
        node.attributes.name,
      ]
        .filter(Boolean)
        .join(" ");
      if (
        ["click", "doubleClick"].includes(type) &&
        (node.attributes.type?.toLowerCase() === "submit" ||
          CONSEQUENTIAL_LABEL.test(semanticText))
      ) {
        confirmationRequired = true;
        reason ||= "submit or consequential click";
      }
    } else if (
      ["click", "doubleClick"].includes(type) &&
      action.target &&
      record(action.target).point
    ) {
      await this.#validateCoordinateTarget(state, record(action.target));
      confirmationRequired = true;
      reason ||= "coordinate click";
      targetLabel = "the selected screenshot coordinate";
      targetIdentity = { coordinateTarget: action.target };
    }

    const dialog = type.startsWith("dialog")
      ? this.#debugger.getDialog(state.chromeTabId)
      : undefined;
    if (
      type === "dialogPrompt" ||
      (type === "dialogAccept" && dialog?.type !== "alert") ||
      (type === "press" && key.toLowerCase() === "enter")
    ) {
      confirmationRequired = true;
      reason ||= type.startsWith("dialog")
        ? "respond to browser confirmation"
        : "submit with Enter";
    }

    const tab = await chrome.tabs.get(state.chromeTabId);
    const origin = safeOrigin(tab.url);
    const canonicalParams = { ...params };
    delete canonicalParams.confirmationId;
    delete canonicalParams.__approvedRequestHash;
    delete canonicalParams.__preflight;
    const requestHash = await hashCanonical({
      method: "action.perform",
      params: canonicalParams,
      documentEpoch: state.documentEpoch,
      origin,
      targetIdentity,
    });
    return {
      confirmationRequired,
      requestHash,
      title: confirmationRequired ? "Confirm browser action" : undefined,
      summary: confirmationRequired
        ? `${type}${targetLabel ? ` on “${targetLabel}”` : ""}${reason ? ` — ${reason}` : ""}`.slice(
            0,
            500,
          )
        : undefined,
      origin,
      documentEpoch: state.documentEpoch,
      targetElementIdentity,
      targetFrameId,
    };
  }

  async #performTargetAction(
    state: TabState,
    action: Record<string, unknown>,
    type: string,
    expectedElementIdentity?: string,
    expectedFrameId?: number,
  ): Promise<Record<string, unknown>> {
    const rawTarget = record(action.target);
    if ("point" in rawTarget) {
      await this.#validateCoordinateTarget(state, rawTarget);
      const point = this.#point(rawTarget.point);
      if (type === "click" || type === "doubleClick") {
        await this.#debugger.dispatchClick(
          state.chromeTabId,
          point.x,
          point.y,
          type === "doubleClick" ? 2 : 1,
        );
      } else if (type === "hover" || type === "move") {
        await this.#debugger.dispatchMove(state.chromeTabId, point.x, point.y);
      } else {
        throw new RpcError(
          "INVALID_REQUEST",
          `${type} does not support a coordinate target`,
        );
      }
      return { performed: true, inputMode: "cdp", point };
    }

    const prepared = await this.#prepareTarget(state, rawTarget);
    if (
      (expectedElementIdentity &&
        prepared.node.elementIdentity !== expectedElementIdentity) ||
      (expectedFrameId !== undefined && prepared.frameId !== expectedFrameId)
    ) {
      throw new RpcError(
        "STALE_REFERENCE",
        "the resolved target element changed before execution",
        { retryable: true },
      );
    }
    const localAction = {
      type,
      value: action.value ?? action.key,
    } as RuntimeAction;
    if (["fill", "select", "check", "uncheck", "focus"].includes(type)) {
      const result = await this.#invokeFrame<Record<string, unknown>>(
        state.chromeTabId,
        prepared.frameId,
        "perform",
        [prepared.localTarget, localAction, prepared.node.elementIdentity],
      );
      return { ...result, frameId: prepared.frameId, inputMode: "dom-native" };
    }
    if (type === "type") {
      await this.#invokeFrame(state.chromeTabId, prepared.frameId, "perform", [
        prepared.localTarget,
        { type: "focus" },
        prepared.node.elementIdentity,
      ]);
      await this.#debugger.dispatchText(
        state.chromeTabId,
        String(action.value ?? ""),
      );
      return { performed: true, frameId: prepared.frameId, inputMode: "cdp" };
    }
    if (type === "press") {
      await this.#invokeFrame(state.chromeTabId, prepared.frameId, "perform", [
        prepared.localTarget,
        { type: "focus" },
        prepared.node.elementIdentity,
      ]);
      await this.#debugger.dispatchKey(
        state.chromeTabId,
        String(action.value ?? action.key ?? ""),
      );
      return { performed: true, frameId: prepared.frameId, inputMode: "cdp" };
    }
    if (!["click", "doubleClick", "hover", "move"].includes(type))
      throw new RpcError("INVALID_REQUEST", `unsupported action type: ${type}`);

    const bounds = prepared.node.topRect ?? prepared.node.rect;
    if (!prepared.node.canTranslateToTop) {
      const fallbackType = type === "move" ? "hover" : type;
      const result = await this.#invokeFrame<Record<string, unknown>>(
        state.chromeTabId,
        prepared.frameId,
        "perform",
        [
          prepared.localTarget,
          { type: fallbackType },
          prepared.node.elementIdentity,
        ],
      );
      return {
        ...result,
        frameId: prepared.frameId,
        inputMode: "dom-oopif-fallback",
      };
    }
    const point = {
      x: bounds.x + bounds.width / 2,
      y: bounds.y + bounds.height / 2,
    };
    if (type === "click" || type === "doubleClick") {
      await this.#debugger.dispatchClick(
        state.chromeTabId,
        point.x,
        point.y,
        type === "doubleClick" ? 2 : 1,
      );
    } else {
      await this.#debugger.dispatchMove(state.chromeTabId, point.x, point.y);
    }
    return {
      performed: true,
      frameId: prepared.frameId,
      inputMode: "cdp",
      point,
      target: prepared.node,
    };
  }

  async #prepareTarget(
    state: TabState,
    target: Record<string, unknown>,
  ): Promise<{
    frameId: number;
    localTarget: Record<string, unknown>;
    node: NodeDescription;
  }> {
    if (target.locator) {
      const locator = locatorFrom(target.locator);
      const matches = await this.#query(
        state,
        locator,
        typeof target.snapshotId === "string" ? target.snapshotId : undefined,
      );
      if (!matches.length)
        throw new RpcError("LOCATOR_NOT_FOUND", "no matching element");
      if (matches.length !== 1)
        throw new RpcError(
          "LOCATOR_AMBIGUOUS",
          `${matches.length} matching elements`,
        );
      const match = matches[0]!;
      const localTarget = { locator: { ...locator, framePath: undefined } };
      const node = await this.#invokeFrame<NodeDescription>(
        state.chromeTabId,
        match.frameId,
        "prepare",
        [localTarget],
      );
      if (!node)
        throw new RpcError(
          "FRAME_UNAVAILABLE",
          "locator frame is unavailable",
          { retryable: true },
        );
      return { frameId: match.frameId, localTarget, node };
    }
    const snapshotId = requiredString(target, "snapshotId");
    const nodeRef = requiredString(target, "nodeRef");
    const snapshot = this.#snapshot(snapshotId, state);
    const parsed = /^f(\d+)_(.+)$/.exec(nodeRef);
    if (!parsed?.[1] || !parsed[2])
      throw new RpcError(
        "STALE_REFERENCE",
        "node reference is not bound to a frame",
      );
    const frameId = Number(parsed[1]);
    const frame = snapshot.frames.get(frameId);
    if (!frame)
      throw new RpcError("FRAME_UNAVAILABLE", "snapshot frame is unavailable", {
        retryable: true,
      });
    const localTarget = {
      snapshotId: frame.localSnapshotId,
      nodeRef: parsed[2],
    };
    const node = await this.#invokeFrame<NodeDescription>(
      state.chromeTabId,
      frameId,
      "prepare",
      [localTarget],
    );
    if (!node)
      throw new RpcError("STALE_REFERENCE", "node reference no longer exists", {
        retryable: true,
      });
    return { frameId, localTarget, node };
  }

  async #actionPoint(
    state: TabState,
    target: Record<string, unknown>,
  ): Promise<{ x: number; y: number }> {
    if (target.point) {
      await this.#validateCoordinateTarget(state, target);
      return this.#point(target.point);
    }
    const prepared = await this.#prepareTarget(state, target);
    if (!prepared.node.canTranslateToTop)
      throw new RpcError(
        "FRAME_UNAVAILABLE",
        "cannot translate this frame to top-level coordinates",
      );
    const rect = prepared.node.topRect ?? prepared.node.rect;
    return { x: rect.x + rect.width / 2, y: rect.y + rect.height / 2 };
  }

  #point(value: unknown): { x: number; y: number } {
    const point = record(value);
    const x = Number(point.x);
    const y = Number(point.y);
    if (!Number.isFinite(x) || !Number.isFinite(y))
      throw new RpcError("INVALID_REQUEST", "point requires finite x and y");
    return { x, y };
  }

  async #dragPoint(
    state: TabState,
    value: unknown,
    sharedSnapshotId: unknown,
  ): Promise<{ x: number; y: number }> {
    const target = record(value);
    if (
      "locator" in target ||
      "point" in target ||
      ("snapshotId" in target && "nodeRef" in target)
    ) {
      return this.#actionPoint(state, target);
    }
    if ("x" in target && "y" in target) {
      if (typeof sharedSnapshotId !== "string")
        throw new RpcError(
          "INVALID_REQUEST",
          "coordinate drag points require action.snapshotId",
        );
      const point = this.#point(target);
      await this.#validateCoordinateTarget(state, {
        snapshotId: sharedSnapshotId,
        point,
      });
      return point;
    }
    throw new RpcError(
      "INVALID_REQUEST",
      "drag endpoints require a locator, snapshot node, or snapshot-bound point",
    );
  }

  async #validateCoordinateTarget(
    state: TabState,
    target: Record<string, unknown>,
  ): Promise<void> {
    const snapshot = this.#snapshot(
      requiredString(target, "snapshotId"),
      state,
    );
    const root =
      snapshot.frames.get(0) ?? snapshot.frames.values().next().value;
    if (!root)
      throw new RpcError("STALE_REFERENCE", "snapshot has no active document");
    const viewport = await this.#invokeFrame<Record<string, unknown>>(
      state.chromeTabId,
      root.frameId,
      "viewport",
      [],
    );
    if (JSON.stringify(viewport) !== JSON.stringify(root.snapshot.viewport)) {
      throw new RpcError(
        "STALE_REFERENCE",
        "viewport changed after the screenshot was captured",
        { retryable: true },
      );
    }
    const revision = await this.#invokeFrame<number>(
      state.chromeTabId,
      root.frameId,
      "getRevision",
      [],
    );
    if (revision !== root.snapshot.revision) {
      throw new RpcError(
        "STALE_REFERENCE",
        "page content changed after the screenshot was captured",
        { retryable: true },
      );
    }
  }

  async #conditionWait(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const condition =
      params.condition && typeof params.condition === "object"
        ? record(params.condition)
        : params;
    const timeoutMs = Math.min(
      Math.max(Number(condition.timeoutMs ?? params.timeoutMs ?? 30_000), 1),
      600_000,
    );
    if (
      typeof condition.type === "string" &&
      ["navigation", "popup", "download", "fileChooser", "dialog"].includes(
        condition.type,
      )
    ) {
      return { event: await this.#waitForExpectation(state, condition) };
    }
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      this.#registry.assertClaimed(
        state.tabId,
        typeof params.sessionId === "string" ? params.sessionId : undefined,
        typeof params.leaseId === "string" ? params.leaseId : undefined,
      );
      if (condition.url !== undefined || condition.urlIncludes !== undefined) {
        const tab = await chrome.tabs.get(state.chromeTabId);
        const current = tab.url ?? "";
        const expected = String(condition.url ?? condition.urlIncludes ?? "");
        const matches =
          condition.exact === true
            ? current === expected
            : current.includes(expected);
        if (matches)
          return {
            matched: true,
            url: redactUrl(current),
            documentEpoch: state.documentEpoch,
          };
      } else if (condition.locator) {
        const nodes = await this.#query(state, locatorFrom(condition.locator));
        const desired = String(condition.state ?? "visible");
        const matched =
          desired === "detached" || desired === "hidden"
            ? !nodes.length ||
              nodes.every((node) => !node.actionability.visible)
            : desired === "enabled"
              ? nodes.some((node) => node.actionability.enabled)
              : desired === "count"
                ? nodes.length === Number(condition.count)
                : nodes.some((node) => node.actionability.visible);
        if (matched) return { matched: true, count: nodes.length, nodes };
      } else {
        throw new RpcError(
          "INVALID_REQUEST",
          "condition requires url, locator, or an event type",
        );
      }
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new RpcError("TIMEOUT", "condition was not met before timeout", {
      retryable: true,
    });
  }

  async #dialogGet(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const dialog = this.#debugger.getDialog(state.chromeTabId);
    return {
      dialog: dialog ? { ...dialog, url: redactUrl(dialog.url) } : null,
    };
  }

  async #dialogRespond(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const dialog = this.#debugger.getDialog(state.chromeTabId);
    if (!dialog)
      throw new RpcError("INVALID_REQUEST", "no JavaScript dialog is open");
    if (params.accept !== false && dialog.type !== "alert") {
      throw new RpcError(
        "PERMISSION_DENIED",
        "accepting confirm, prompt, or beforeunload dialogs requires action.perform trusted confirmation",
      );
    }
    await this.#debugger.respondDialog(
      state.chromeTabId,
      params.accept !== false,
      typeof params.promptText === "string" ? params.promptText : undefined,
    );
    return { handled: true, accepted: params.accept !== false };
  }

  async #clipboard(
    action: "read" | "write",
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    await this.#ensureOffscreen();
    const response = (await chrome.runtime.sendMessage({
      target: "browser-control-offscreen",
      action,
      text: params.text,
      html: params.html,
      items: params.items,
    })) as Record<string, unknown>;
    if (typeof response?.error === "string")
      throw new RpcError("INTERNAL", response.error);
    return response;
  }

  async #setFiles(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const rawFiles = params.files ?? params.paths;
    if (!Array.isArray(rawFiles) || !rawFiles.length || rawFiles.length > 100)
      throw new RpcError(
        "INVALID_REQUEST",
        "files must be a non-empty array of at most 100 paths",
      );
    const files = rawFiles.map(String);
    for (const file of files) {
      if (!file.startsWith("/") || file.includes("\0"))
        throw new RpcError(
          "FILE_NOT_ALLOWED",
          "uploads require absolute, NUL-free macOS paths",
        );
    }
    await this.#debugger.setFileChooserFiles(state.chromeTabId, files);
    this.#emit("fileChooser.completed", {
      tabId: state.tabId,
      sessionId: state.claim?.sessionId,
      fileCount: files.length,
    });
    return { set: true, fileCount: files.length };
  }

  async #downloadGet(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const id = chromeDownloadId(params.downloadId);
    const [item] = await chrome.downloads.search({ id });
    if (!item) throw new RpcError("DOWNLOAD_FAILED", "download does not exist");
    return downloadToPublic(item);
  }

  async #downloadWait(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const timeoutMs = Math.min(
      Math.max(Number(params.timeoutMs ?? 120_000), 1),
      600_000,
    );
    if (params.downloadId !== undefined) {
      const id = chromeDownloadId(params.downloadId);
      const deadline = Date.now() + timeoutMs;
      while (Date.now() < deadline) {
        if (typeof params.tabId === "string")
          this.#registry.assertClaimed(
            params.tabId,
            typeof params.sessionId === "string" ? params.sessionId : undefined,
            typeof params.leaseId === "string" ? params.leaseId : undefined,
          );
        const [item] = await chrome.downloads.search({ id });
        if (!item)
          throw new RpcError("DOWNLOAD_FAILED", "download does not exist");
        if (item.state === "complete") return downloadToPublic(item);
        if (item.state === "interrupted")
          throw new RpcError(
            "DOWNLOAD_FAILED",
            item.error ?? "download was interrupted",
          );
        await new Promise((resolve) => setTimeout(resolve, 200));
      }
      throw new RpcError("TIMEOUT", "download did not finish before timeout", {
        retryable: true,
        effect: "possible",
      });
    }
    const state = this.#claimed(params);
    const event = await this.#waitForDownload(state, timeoutMs);
    return record(event.download);
  }

  async #contentExport(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const format = String(params.format ?? "html");
    const coverage = {
      source: "materialized-dom",
      completeness: "unknown",
      virtualizedContentMayBeOmitted: true,
      documentEpoch: state.documentEpoch,
    };
    if (format === "dom") {
      const content = JSON.stringify(
        {
          format: "cdp-dom-snapshot",
          documentEpoch: state.documentEpoch,
          frames: await this.#debugger.domSnapshot(state.chromeTabId),
        },
        null,
        2,
      );
      const artifact = await this.#createArtifact(
        params,
        state,
        "page-dom.json",
        "application/json",
        content,
        "dom-export",
      );
      return { ...artifact, coverage };
    }
    if (format === "googleWorkspace") {
      const tab = await chrome.tabs.get(state.chromeTabId);
      let host = "";
      try {
        host = new URL(tab.url ?? "").hostname;
      } catch {
        // Rejected below.
      }
      if (host !== "docs.google.com")
        throw new RpcError(
          "UNSUPPORTED",
          "googleWorkspace export requires a docs.google.com document, sheet, or presentation",
        );
      const visibleFrames = await this.#invokeAll<string>(
        state.chromeTabId,
        "exportContent",
        ["text"],
      );
      const content = JSON.stringify(
        {
          format: "google-workspace-accessibility",
          url: redactUrl(tab.url),
          title: tab.title,
          documentEpoch: state.documentEpoch,
          visibleText: visibleFrames.map((frame) => ({
            frameId: frame.frameId,
            text: frame.result,
          })),
          accessibility: await this.#debugger.accessibilitySnapshot(
            state.chromeTabId,
          ),
        },
        null,
        2,
      );
      const artifact = await this.#createArtifact(
        params,
        state,
        "google-workspace.json",
        "application/json",
        content,
        "google-workspace-export",
      );
      return {
        ...artifact,
        coverage: {
          ...coverage,
          source: "visible-text-and-accessibility",
        },
      };
    }
    if (!(["html", "text", "markdown"] as string[]).includes(format))
      throw new RpcError("UNSUPPORTED", `unsupported export format: ${format}`);
    const frames = await this.#invokeAll<string>(
      state.chromeTabId,
      "exportContent",
      [format],
    );
    const content = frames
      .filter((frame) => frame.result !== undefined)
      .map((frame) => `<!-- frame:${frame.frameId} -->\n${frame.result}`)
      .join("\n\n");
    const mimeType =
      format === "html"
        ? "text/html"
        : format === "markdown"
          ? "text/markdown"
          : "text/plain";
    const artifact = await this.#createArtifact(
      params,
      state,
      `page.${format === "markdown" ? "md" : format === "text" ? "txt" : "html"}`,
      mimeType,
      content,
      "page-export",
    );
    return { ...artifact, coverage };
  }

  async #pageAssets(
    params: Record<string, unknown>,
    exportManifest: boolean,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const frames = await this.#invokeAll<unknown[]>(
      state.chromeTabId,
      "pageAssets",
      [],
    );
    const assets = frames.flatMap((frame) =>
      (frame.result ?? []).map((asset) => {
        const item = record(asset);
        return {
          frameId: frame.frameId,
          ...item,
          url: typeof item.url === "string" ? redactUrl(item.url) : undefined,
        };
      }),
    );
    if (!exportManifest) return { assets, count: assets.length };
    return this.#createArtifact(
      params,
      state,
      "page-assets.json",
      "application/json",
      JSON.stringify({ assets }, null, 2),
      "page-assets",
    );
  }

  async #secureInputRequest(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const tab = await chrome.tabs.get(state.chromeTabId);
    const actualOrigin = safeOrigin(tab.url);
    const requestedOrigin = safeOrigin(requiredString(params, "origin"));
    if (!actualOrigin || actualOrigin !== requestedOrigin)
      throw new RpcError(
        "PERMISSION_DENIED",
        "secure input origin does not match the claimed tab",
      );
    const target = record(params.target);
    const normalizedTarget = target.locator
      ? { locator: locatorFrom(target.locator) }
      : {
          snapshotId: requiredString(target, "snapshotId"),
          nodeRef: requiredString(target, "nodeRef"),
        };
    const prepared = await this.#prepareTarget(
      state,
      normalizedTarget as unknown as Record<string, unknown>,
    );
    if (!state.claim)
      throw new RpcError(
        "LEASE_REQUIRED",
        "secure input requires an active lease",
      );
    const requestId = randomId("secure");
    this.#secureInputs.set(requestId, {
      requestId,
      tabId: state.tabId,
      sessionId: state.claim.sessionId,
      leaseId: state.claim.leaseId,
      documentEpoch: state.documentEpoch,
      frameId: prepared.frameId,
      elementIdentity: prepared.node.elementIdentity,
      target: normalizedTarget as SecureInputRequest["target"],
      label:
        typeof params.label === "string"
          ? params.label.slice(0, 120)
          : undefined,
      origin: requestedOrigin,
      secret: params.secret !== false,
      autocomplete:
        typeof params.autocomplete === "string"
          ? params.autocomplete.slice(0, 80)
          : undefined,
      createdAt: Date.now(),
    });
    await chrome.windows.create({
      url: chrome.runtime.getURL(
        `secure-input.html?requestId=${encodeURIComponent(requestId)}`,
      ),
      type: "popup",
      width: 430,
      height: 330,
      focused: true,
    });
    return { requestId, status: "waiting-for-user", origin: requestedOrigin };
  }

  async #unsafeEvaluate(
    params: Record<string, unknown>,
  ): Promise<Record<string, unknown>> {
    const state = this.#claimed(params);
    const expression = requiredString(params, "expression");
    const result = await this.#debugger.send(
      state.chromeTabId,
      "Runtime.evaluate",
      {
        expression,
        awaitPromise: params.awaitPromise !== false,
        returnByValue: params.returnByValue !== false,
        userGesture: params.userGesture === true,
      },
      typeof params.targetSessionId === "string"
        ? params.targetSessionId
        : undefined,
    );
    return record(result);
  }

  async #unsafeCdp(params: Record<string, unknown>): Promise<unknown> {
    const state = this.#claimed(params);
    return this.#debugger.send(
      state.chromeTabId,
      requiredString(params, "method"),
      params.params && typeof params.params === "object"
        ? record(params.params)
        : {},
      typeof params.targetSessionId === "string"
        ? params.targetSessionId
        : undefined,
    );
  }

  #claimed(params: Record<string, unknown>): TabState {
    const state = this.#registry.assertClaimed(
      requiredString(params, "tabId"),
      typeof params.sessionId === "string" ? params.sessionId : undefined,
      typeof params.leaseId === "string" ? params.leaseId : undefined,
    );
    if (
      typeof params.expectedDocumentEpoch === "number" &&
      params.expectedDocumentEpoch !== state.documentEpoch
    ) {
      throw new RpcError(
        "STALE_REFERENCE",
        "document changed after the caller observed it",
        {
          retryable: true,
          details: {
            expectedDocumentEpoch: params.expectedDocumentEpoch,
            actualDocumentEpoch: state.documentEpoch,
          },
        },
      );
    }
    return state;
  }

  #snapshot(snapshotId: string, state: TabState): SnapshotState {
    const snapshot = this.#snapshots.get(snapshotId);
    if (
      !snapshot ||
      snapshot.tabId !== state.tabId ||
      snapshot.documentEpoch !== state.documentEpoch
    ) {
      throw new RpcError(
        "STALE_REFERENCE",
        "snapshot does not belong to the current document",
        { retryable: true },
      );
    }
    return snapshot;
  }

  #storeSnapshot(snapshot: SnapshotState): void {
    this.#snapshots.set(snapshot.snapshotId, snapshot);
    while (this.#snapshots.size > MAX_SNAPSHOTS) {
      const oldest = this.#snapshots.keys().next().value as string | undefined;
      if (!oldest) break;
      this.#snapshots.delete(oldest);
    }
  }

  #dropTabSnapshots(tabId: string): void {
    for (const [snapshotId, snapshot] of this.#snapshots) {
      if (snapshot.tabId === tabId) this.#snapshots.delete(snapshotId);
    }
  }

  async #ensureRuntime(chromeTabId: number): Promise<void> {
    try {
      await chrome.scripting.executeScript({
        target: { tabId: chromeTabId, allFrames: true },
        files: ["locator.js"],
        injectImmediately: true,
      });
    } catch (error) {
      throw new RpcError(
        "UNSUPPORTED_PAGE",
        `cannot inject locator runtime: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  async #removeRuntime(chromeTabId: number): Promise<void> {
    await chrome.scripting
      .executeScript({
        target: { tabId: chromeTabId, allFrames: true },
        func: () => {
          window.__browserControlLocatorRuntime?.dispose();
          delete window.__browserControlLocatorRuntime;
          document.getElementById("__browser_control_indicator")?.remove();
        },
      })
      .catch(() => undefined);
  }

  async #invokeAll<T>(
    chromeTabId: number,
    method: string,
    args: unknown[],
  ): Promise<Array<FrameResult<T>>> {
    let results: chrome.scripting.InjectionResult<unknown>[];
    try {
      results = await chrome.scripting.executeScript({
        target: { tabId: chromeTabId, allFrames: true },
        func: async (runtimeMethod: string, runtimeArgs: unknown[]) => {
          const runtime = window.__browserControlLocatorRuntime as unknown as
            Record<string, (...values: unknown[]) => unknown> | undefined;
          if (!runtime)
            throw new Error(
              "FRAME_UNAVAILABLE: locator runtime is not installed",
            );
          const fn = runtime[runtimeMethod];
          if (typeof fn !== "function")
            throw new Error(
              `METHOD_NOT_FOUND: locator runtime method ${runtimeMethod} does not exist`,
            );
          return await fn.apply(runtime, runtimeArgs);
        },
        args: [method, args],
      });
    } catch (error) {
      throw this.#runtimeError(error);
    }
    return results.map((result) => {
      const execution = result as typeof result & { error?: unknown };
      if (execution.error) throw this.#runtimeError(execution.error);
      return {
        frameId: result.frameId,
        documentId: result.documentId,
        result: result.result as T,
      };
    });
  }

  async #invokeFrame<T>(
    chromeTabId: number,
    frameId: number,
    method: string,
    args: unknown[],
  ): Promise<T | undefined> {
    let results: chrome.scripting.InjectionResult<unknown>[];
    try {
      results = await chrome.scripting.executeScript({
        target: { tabId: chromeTabId, frameIds: [frameId] },
        func: async (runtimeMethod: string, runtimeArgs: unknown[]) => {
          const runtime = window.__browserControlLocatorRuntime as unknown as
            Record<string, (...values: unknown[]) => unknown> | undefined;
          if (!runtime)
            throw new Error(
              "FRAME_UNAVAILABLE: locator runtime is not installed",
            );
          const fn = runtime[runtimeMethod];
          if (typeof fn !== "function")
            throw new Error(
              `METHOD_NOT_FOUND: locator runtime method ${runtimeMethod} does not exist`,
            );
          return await fn.apply(runtime, runtimeArgs);
        },
        args: [method, args],
      });
    } catch (error) {
      throw this.#runtimeError(error);
    }
    const first = results[0] as
      | (chrome.scripting.InjectionResult<unknown> & { error?: unknown })
      | undefined;
    if (first?.error) throw this.#runtimeError(first.error);
    return first?.result as T | undefined;
  }

  #runtimeError(error: unknown): RpcError {
    const message =
      error instanceof Error
        ? error.message
        : error && typeof error === "object" && "message" in error
          ? String((error as { message?: unknown }).message)
          : String(error);
    const match =
      /(STALE_REFERENCE|DETACHED_NODE|FRAME_UNAVAILABLE|LOCATOR_NOT_FOUND|LOCATOR_AMBIGUOUS|NOT_ACTIONABLE|METHOD_NOT_FOUND):\s*([^\n]*)/.exec(
        message,
      );
    return match?.[1]
      ? new RpcError(match[1], match[2] || message, {
          retryable: match[1] !== "METHOD_NOT_FOUND",
        })
      : new RpcError("FRAME_UNAVAILABLE", message, { retryable: true });
  }

  async #ensureOffscreen(): Promise<void> {
    const offscreenUrl = chrome.runtime.getURL("offscreen.html");
    const contexts = await chrome.runtime.getContexts({
      contextTypes: ["OFFSCREEN_DOCUMENT"],
      documentUrls: [offscreenUrl],
    });
    if (contexts.length) return;
    await chrome.offscreen.createDocument({
      url: "offscreen.html",
      reasons: [chrome.offscreen.Reason.CLIPBOARD],
      justification: "Read and write the user-approved clipboard payload",
    });
  }

  async #createArtifact(
    params: Record<string, unknown>,
    state: TabState,
    fileName: string,
    mimeType: string,
    content: string,
    kind: string,
  ): Promise<Record<string, unknown>> {
    if (!this.#bridgeState.helloComplete || !this.#bridge)
      throw new RpcError(
        "EXTENSION_DISCONNECTED",
        "browserd is required to store artifacts",
        { retryable: true },
      );
    const bytes = new TextEncoder().encode(content);
    if (bytes.byteLength > MAX_INLINE_ARTIFACT_BYTES)
      throw new RpcError(
        "UNSUPPORTED",
        "export exceeds the 64 MiB local artifact transport limit",
        {
          details: { size: bytes.byteLength, limit: MAX_INLINE_ARTIFACT_BYTES },
        },
      );
    let binary = "";
    for (let offset = 0; offset < bytes.length; offset += 0x8000)
      binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
    const result = await this.#bridge.request(
      "bridge.event",
      {
        type: "artifact.created",
        sessionId: params.sessionId,
        tabId: state.tabId,
        documentEpoch: state.documentEpoch,
        payload: { kind, mimeType, fileName, dataBase64: btoa(binary) },
      },
      60_000,
    );
    const response = result && typeof result === "object" ? record(result) : {};
    const artifact =
      response.artifact && typeof response.artifact === "object"
        ? record(response.artifact)
        : {};
    return { created: true, ...artifact, eventSeq: response.seq };
  }

  #waitForExpectation(
    state: TabState,
    expectation: Record<string, unknown>,
  ): Promise<ServiceEvent> {
    const typeMap: Record<string, string> = {
      navigation: "navigation.completed",
      popup: "popup.opened",
      download: "download.completed",
      fileChooser: "fileChooser.opened",
      dialog: "dialog.opened",
    };
    const requested = requiredString(expectation, "type");
    const eventType = typeMap[requested] ?? requested;
    const timeoutMs = Math.min(
      Math.max(Number(expectation.timeoutMs ?? 30_000), 1),
      600_000,
    );
    if (requested === "download")
      return this.#waitForDownload(state, timeoutMs);
    return this.#waitForEvent(
      eventType,
      (event) =>
        event.tabId === state.tabId ||
        (requested === "popup" && event.openerTabId === state.tabId),
      timeoutMs,
      state.tabId,
    );
  }

  #waitForDownload(state: TabState, timeoutMs: number): Promise<ServiceEvent> {
    return new Promise((resolve, reject) => {
      const waiter: DownloadWaiter = {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        resolve,
        reject,
        timer: setTimeout(() => {
          this.#downloadWaiters.delete(waiter);
          reject(
            new RpcError(
              "TIMEOUT",
              "timed out waiting for a download associated with this tab",
              { retryable: true, effect: "possible" },
            ),
          );
        }, timeoutMs) as unknown as number,
      };
      this.#downloadWaiters.add(waiter);
    });
  }

  #associateDownload(downloadId: number, state: TabState): void {
    const candidates = Array.from(this.#downloadWaiters).filter(
      (waiter) =>
        waiter.downloadId === undefined && waiter.tabId === state.tabId,
    );
    if (candidates.length === 1) candidates[0]!.downloadId = downloadId;
    this.#downloadOwners.set(downloadId, {
      tabId: state.tabId,
      sessionId: state.claim?.sessionId,
    });
  }

  #noteDownloadCreated(item: chrome.downloads.DownloadItem): void {
    const candidates = Array.from(this.#downloadWaiters).filter(
      (waiter) => waiter.downloadId === undefined,
    );
    if (candidates.length === 1) {
      const waiter = candidates[0]!;
      waiter.downloadId = item.id;
      this.#downloadOwners.set(item.id, {
        tabId: waiter.tabId,
        sessionId: waiter.sessionId,
      });
      return;
    }
    if (candidates.length > 1) {
      const error = new RpcError(
        "DOWNLOAD_AMBIGUOUS",
        "multiple tabs were waiting when a download started",
        {
          retryable: false,
          effect: "possible",
          details: {
            downloadId: publicDownloadId(item.id),
            candidateTabIds: candidates.map((waiter) => waiter.tabId),
          },
        },
      );
      for (const waiter of candidates) {
        clearTimeout(waiter.timer);
        this.#downloadWaiters.delete(waiter);
        waiter.reject(error);
      }
    }
  }

  #completeDownloadWaiter(
    item: chrome.downloads.DownloadItem,
    event: ServiceEvent,
  ): void {
    const waiter = Array.from(this.#downloadWaiters).find(
      (candidate) => candidate.downloadId === item.id,
    );
    if (!waiter) return;
    clearTimeout(waiter.timer);
    this.#downloadWaiters.delete(waiter);
    if (item.state === "complete") waiter.resolve(event);
    else
      waiter.reject(
        new RpcError(
          "DOWNLOAD_FAILED",
          item.error ?? "download was interrupted",
          { effect: "possible" },
        ),
      );
  }

  #waitForEvent(
    type: string,
    predicate: (event: ServiceEvent) => boolean,
    timeoutMs: number,
    tabId?: string,
  ): Promise<ServiceEvent> {
    return new Promise((resolve, reject) => {
      const waiter: EventWaiter = {
        type,
        tabId,
        predicate,
        resolve,
        reject,
        timer: setTimeout(() => {
          this.#waiters.delete(waiter);
          reject(
            new RpcError("TIMEOUT", `timed out waiting for ${type}`, {
              retryable: true,
              effect: "possible",
            }),
          );
        }, timeoutMs) as unknown as number,
      };
      this.#waiters.add(waiter);
    });
  }

  #cancelExpectationWaiters(tabId: string, error: Error): void {
    for (const waiter of Array.from(this.#waiters)) {
      if (waiter.tabId !== tabId) continue;
      clearTimeout(waiter.timer);
      this.#waiters.delete(waiter);
      waiter.reject(error);
    }
    for (const waiter of Array.from(this.#downloadWaiters)) {
      if (waiter.tabId !== tabId) continue;
      clearTimeout(waiter.timer);
      this.#downloadWaiters.delete(waiter);
      waiter.reject(error);
    }
  }

  #emit(type: string, payload: Record<string, unknown> = {}): void {
    const event: ServiceEvent = {
      type,
      time: new Date().toISOString(),
      ...payload,
    };
    for (const waiter of Array.from(this.#waiters)) {
      if (waiter.type !== type || !waiter.predicate(event)) continue;
      clearTimeout(waiter.timer);
      this.#waiters.delete(waiter);
      waiter.resolve(event);
    }
    this.#bridge?.emitEvent(type, payload);
  }

  #registerChromeEvents(): void {
    chrome.tabs.onCreated.addListener((tab) => {
      if (tab.id === undefined) return;
      const opener =
        tab.openerTabId === undefined
          ? undefined
          : this.#registry.fromChromeId(tab.openerTabId);
      const state = this.#registry.ensure(tab.id, Boolean(opener?.owned));
      this.#emit(
        tab.openerTabId === undefined ? "tab.created" : "popup.opened",
        {
          tabId: state.tabId,
          openerTabId: opener?.tabId,
          sessionId: opener?.claim?.sessionId,
          tab: this.#registry.toPublic(state, {
            ...tab,
            url: redactUrl(tab.url),
          }),
        },
      );
    });
    chrome.tabs.onUpdated.addListener((chromeTabId, changeInfo, tab) => {
      const state = this.#registry.ensure(chromeTabId);
      this.#emit("tab.updated", {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        change: {
          ...changeInfo,
          url: changeInfo.url ? redactUrl(changeInfo.url) : undefined,
        },
        tab: this.#registry.toPublic(state, {
          ...tab,
          url: redactUrl(tab.url),
        }),
      });
    });
    chrome.tabs.onActivated.addListener(({ tabId, windowId }) => {
      const state = this.#registry.ensure(tabId);
      this.#emit("tab.activated", {
        tabId: state.tabId,
        windowId,
        sessionId: state.claim?.sessionId,
      });
    });
    chrome.tabs.onRemoved.addListener((chromeTabId, removeInfo) => {
      const state = this.#registry.fromChromeId(chromeTabId);
      if (!state) return;
      void this.#debugger.detach(chromeTabId);
      this.#dropTabSnapshots(state.tabId);
      void this.#registry.remove(chromeTabId);
      this.#emit("tab.closed", {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        windowId: removeInfo.windowId,
      });
    });
    chrome.webNavigation.onCommitted.addListener((details) => {
      if (details.frameId !== 0) return;
      const state = this.#registry.noteNavigation(details.tabId);
      if (!state) return;
      this.#dropTabSnapshots(state.tabId);
      this.#emit("navigation.committed", {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        documentEpoch: state.documentEpoch,
        url: redactUrl(details.url),
        transitionType: details.transitionType,
      });
    });
    chrome.webNavigation.onCompleted.addListener((details) => {
      if (details.frameId !== 0) return;
      const state = this.#registry.fromChromeId(details.tabId);
      if (!state) return;
      if (state.claim)
        void this.#ensureRuntime(details.tabId).catch(() => undefined);
      this.#emit("navigation.completed", {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        documentEpoch: state.documentEpoch,
        url: redactUrl(details.url),
      });
    });
    const sameDocumentNavigation = (
      details: chrome.webNavigation.WebNavigationTransitionCallbackDetails,
      kind: "history" | "fragment",
    ): void => {
      if (details.frameId !== 0) return;
      const state = this.#registry.fromChromeId(details.tabId);
      if (!state) return;
      const payload = {
        tabId: state.tabId,
        sessionId: state.claim?.sessionId,
        documentEpoch: state.documentEpoch,
        url: redactUrl(details.url),
        sameDocument: true,
        sameDocumentKind: kind,
        transitionType: details.transitionType,
      };
      this.#emit("navigation.committed", payload);
      this.#emit("navigation.completed", payload);
    };
    chrome.webNavigation.onHistoryStateUpdated.addListener((details) =>
      sameDocumentNavigation(details, "history"),
    );
    chrome.webNavigation.onReferenceFragmentUpdated.addListener((details) =>
      sameDocumentNavigation(details, "fragment"),
    );
    chrome.downloads.onCreated.addListener((item) => {
      this.#noteDownloadCreated(item);
      const owner = this.#downloadOwners.get(item.id);
      this.#emit("download.started", {
        ...owner,
        download: downloadToPublic(item),
      });
    });
    chrome.downloads.onChanged.addListener((delta) => {
      void chrome.downloads.search({ id: delta.id }).then(([item]) => {
        if (!item) return;
        const type =
          item.state === "complete"
            ? "download.completed"
            : item.state === "interrupted"
              ? "download.failed"
              : "download.progress";
        const owner = this.#downloadOwners.get(item.id);
        const event: ServiceEvent = {
          type,
          time: new Date().toISOString(),
          ...owner,
          download: downloadToPublic(item),
        };
        this.#emit(type, { ...owner, download: downloadToPublic(item) });
        if (item.state === "complete" || item.state === "interrupted") {
          this.#completeDownloadWaiter(item, event);
          this.#downloadOwners.delete(item.id);
        }
      });
    });
  }

  #onDebuggerEvent(type: string, payload: Record<string, unknown>): void {
    const chromeTabId = Number(payload.chromeTabId);
    const state = Number.isFinite(chromeTabId)
      ? this.#registry.fromChromeId(chromeTabId)
      : undefined;
    const translated = {
      ...payload,
      chromeTabId: undefined,
      tabId: state?.tabId,
      sessionId: state?.claim?.sessionId,
    };
    this.#emit(
      type === "navigation.lifecycle" && payload.lifecycle === "load"
        ? "navigation.completed"
        : type,
      translated,
    );
    if (type === "debugger.detached" && state?.claim) {
      void this.#registry.release(state.tabId).then(() => this.#updateBadge());
      this.#dropTabSnapshots(state.tabId);
    }
  }

  async #handleRuntimeMessage(
    message: Record<string, unknown>,
    sender: chrome.runtime.MessageSender,
  ): Promise<unknown> {
    if (message.source === "popup") {
      if (sender.url !== chrome.runtime.getURL("popup.html"))
        throw new RpcError(
          "PERMISSION_DENIED",
          "message did not originate from the trusted popup",
        );
      if (message.type === "status") return this.#popupStatus();
      if (message.type === "stopAll") return this.stopAll("user-stop", true);
      if (message.type === "release") {
        const tabId = requiredString(message, "tabId");
        const state = this.#registry.fromHandle(tabId);
        await this.#bridge?.request("bridge.admin.lease.revoke", {
          tabId,
          reason: "released by user",
        });
        if (state.claim) await this.#releaseLocal(state);
        return { released: true };
      }
      if (message.type === "confirmation") {
        const confirmationId = requiredString(message, "confirmationId");
        if (!this.#confirmations.has(confirmationId))
          throw new RpcError(
            "INVALID_REQUEST",
            "confirmation is no longer pending",
          );
        await this.#bridge?.request("bridge.admin.confirmation.respond", {
          confirmationId,
          decision: message.approved === true ? "approve" : "deny",
        });
        this.#removeConfirmation(confirmationId);
        await this.#updateBadge();
        return { resolved: true };
      }
    }
    if (message.source === "secure-input") {
      const requestId = requiredString(message, "requestId");
      let senderURL: URL;
      try {
        senderURL = new URL(sender.url ?? "");
      } catch {
        throw new RpcError("PERMISSION_DENIED", "invalid secure-input sender");
      }
      if (
        senderURL.origin !== new URL(chrome.runtime.getURL("/")).origin ||
        senderURL.pathname !== "/secure-input.html" ||
        senderURL.searchParams.get("requestId") !== requestId
      ) {
        throw new RpcError(
          "PERMISSION_DENIED",
          "message did not originate from the trusted secure-input window",
        );
      }
      const request = this.#secureInputs.get(requestId);
      if (message.type === "get") {
        if (!request || Date.now() - request.createdAt > 5 * 60_000)
          return null;
        return {
          requestId,
          label: request.label,
          origin: request.origin,
          secret: request.secret,
          autocomplete: request.autocomplete,
        };
      }
      if (!request)
        throw new RpcError(
          "INVALID_REQUEST",
          "secure input request is no longer valid",
        );
      if (message.type === "cancel") {
        this.#secureInputs.delete(requestId);
        this.#emit("secureInput.cancelled", {
          tabId: request.tabId,
          requestId,
        });
        return { cancelled: true };
      }
      if (message.type === "submit") {
        const value = requiredString(message, "value");
        this.#secureInputs.delete(requestId);
        const state = this.#registry.assertClaimed(
          request.tabId,
          request.sessionId,
          request.leaseId,
        );
        if (state.documentEpoch !== request.documentEpoch)
          throw new RpcError(
            "STALE_REFERENCE",
            "secure-input document changed before submission",
            { retryable: false },
          );
        const tab = await chrome.tabs.get(state.chromeTabId);
        if (safeOrigin(tab.url) !== request.origin)
          throw new RpcError(
            "PERMISSION_DENIED",
            "secure-input origin changed before submission",
          );
        const prepared = await this.#prepareTarget(
          state,
          request.target as unknown as Record<string, unknown>,
        );
        if (
          prepared.frameId !== request.frameId ||
          prepared.node.elementIdentity !== request.elementIdentity
        ) {
          throw new RpcError(
            "STALE_REFERENCE",
            "secure-input target changed before submission",
            { retryable: false },
          );
        }
        await this.#invokeFrame(
          state.chromeTabId,
          prepared.frameId,
          "markSensitive",
          [prepared.localTarget, request.elementIdentity],
        );
        await this.#invokeFrame(
          state.chromeTabId,
          prepared.frameId,
          "perform",
          [
            prepared.localTarget,
            { type: "fill", value },
            request.elementIdentity,
          ],
        );
        this.#emit("secureInput.completed", {
          tabId: request.tabId,
          sessionId: state.claim?.sessionId,
          requestId,
        });
        return { completed: true };
      }
    }
    throw new RpcError("METHOD_NOT_FOUND", "unsupported extension UI message");
  }

  async #popupStatus(): Promise<Record<string, unknown>> {
    this.#pruneExpiredConfirmations();
    const claimedTabs: Array<Record<string, unknown>> = [];
    for (const state of this.#registry
      .all()
      .filter((candidate) => candidate.claim)) {
      try {
        const tab = await chrome.tabs.get(state.chromeTabId);
        claimedTabs.push({
          tabId: state.tabId,
          title: tab.title?.slice(0, 200),
          url: redactUrl(tab.url),
          sessionId: state.claim?.sessionId,
          sessionName: state.claim?.sessionName,
          clientId: state.claim?.clientId,
        });
      } catch {
        // Closed tabs are removed asynchronously.
      }
    }
    return {
      bridge: this.#bridgeState,
      claimedTabs,
      confirmations: Array.from(this.#confirmations.values()),
    };
  }

  async #releaseLocal(state: TabState): Promise<void> {
    await this.#debugger.detach(state.chromeTabId);
    await this.#removeRuntime(state.chromeTabId);
    await this.#registry.release(state.tabId);
    this.#dropTabSnapshots(state.tabId);
    await this.#updateBadge();
  }

  async #updateBadge(): Promise<void> {
    this.#pruneExpiredConfirmations();
    const claimed = this.#registry.all().filter((state) => state.claim).length;
    const pending = this.#confirmations.size;
    await chrome.action.setBadgeBackgroundColor({
      color: pending ? "#d97706" : "#2563eb",
    });
    await chrome.action.setBadgeText({
      text: pending ? "!" : claimed ? String(claimed) : "",
    });
  }

  #scheduleConfirmationExpiry(
    confirmationId: string,
    expiresAt?: string,
  ): void {
    const previous = this.#confirmationTimers.get(confirmationId);
    if (previous !== undefined) clearTimeout(previous);
    const expires = expiresAt ? Date.parse(expiresAt) : Number.NaN;
    if (!Number.isFinite(expires)) return;
    const timer = setTimeout(
      () => {
        this.#confirmationTimers.delete(confirmationId);
        this.#confirmations.delete(confirmationId);
        void this.#updateBadge();
      },
      Math.max(0, expires - Date.now()),
    ) as unknown as number;
    this.#confirmationTimers.set(confirmationId, timer);
  }

  #removeConfirmation(confirmationId: string): void {
    this.#confirmations.delete(confirmationId);
    const timer = this.#confirmationTimers.get(confirmationId);
    if (timer !== undefined) clearTimeout(timer);
    this.#confirmationTimers.delete(confirmationId);
  }

  #pruneExpiredConfirmations(): void {
    const now = Date.now();
    for (const [confirmationId, confirmation] of this.#confirmations) {
      const expires = confirmation.expiresAt
        ? Date.parse(confirmation.expiresAt)
        : Number.NaN;
      if (Number.isFinite(expires) && expires <= now)
        this.#removeConfirmation(confirmationId);
    }
  }
}

const CONSEQUENTIAL_LABEL =
  /\b(?:send|submit|publish|post|save|purchase|buy|pay|order|delete|remove|destroy|confirm|approve|grant|allow|invite|share|transfer|withdraw|sign\s*up|create\s+account)\b|发送|提交|发布|保存|购买|支付|下单|删除|移除|确认|批准|授权|允许|邀请|分享|转账|提现|注册/i;

async function hashCanonical(value: unknown): Promise<string> {
  const bytes = new TextEncoder().encode(JSON.stringify(canonicalize(value)));
  const digest = await crypto.subtle.digest(
    "SHA-256",
    bytes as Uint8Array<ArrayBuffer>,
  );
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

function canonicalize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (!value || typeof value !== "object") return value;
  return Object.fromEntries(
    Object.entries(value as Record<string, unknown>)
      .filter(([, item]) => item !== undefined)
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([key, item]) => [key, canonicalize(item)]),
  );
}

export function redactUrl(value: string | undefined): string {
  if (!value) return "";
  try {
    const url = new URL(value);
    if (!["http:", "https:"].includes(url.protocol)) return `${url.protocol}//`;
    return `${url.origin}${url.pathname}`;
  } catch {
    return "";
  }
}

function safeOrigin(value: string | undefined): string {
  if (!value) return "";
  try {
    const url = new URL(value);
    return ["http:", "https:"].includes(url.protocol) ? url.origin : "";
  } catch {
    return "";
  }
}
