import { RpcError, randomId } from "./shared.js";

export interface Claim {
  sessionId: string;
  leaseId: string;
  claimedAt: string;
  sessionName?: string;
  clientId?: string;
}

export interface TabState {
  tabId: string;
  chromeTabId: number;
  documentEpoch: number;
  owned: boolean;
  claim?: Claim;
}

interface StoredState {
  handles?: Record<string, string>;
  owned?: number[];
  claimedChromeTabIds?: number[];
}

export class TabRegistry {
  readonly #byHandle = new Map<string, TabState>();
  readonly #byChromeId = new Map<number, TabState>();

  async initialize(): Promise<void> {
    const stored = (await chrome.storage.session.get([
      "tabHandles",
      "ownedTabs",
      "claimedChromeTabIds",
    ])) as StoredState & {
      tabHandles?: Record<string, string>;
      ownedTabs?: number[];
    };
    const handles = stored.tabHandles ?? {};
    const owned = new Set(stored.ownedTabs ?? []);
    const tabs = await chrome.tabs.query({});
    for (const tab of tabs) {
      if (tab.id === undefined) continue;
      const handle = handles[String(tab.id)] ?? randomId("tab");
      this.#register(tab.id, handle, owned.has(tab.id));
    }
    for (const tabId of stored.claimedChromeTabIds ?? []) {
      try {
        await chrome.debugger.detach({ tabId });
      } catch {
        // A service-worker restart may already have detached the target.
      }
      await chrome.scripting
        .executeScript({
          target: { tabId, allFrames: true },
          func: () => {
            window.__browserControlLocatorRuntime?.dispose();
            delete window.__browserControlLocatorRuntime;
            document.getElementById("__browser_control_indicator")?.remove();
          },
        })
        .catch(() => undefined);
    }
    await this.#persist();
  }

  ensure(chromeTabId: number, owned = false): TabState {
    const existing = this.#byChromeId.get(chromeTabId);
    if (existing) {
      if (owned) existing.owned = true;
      return existing;
    }
    const state = this.#register(chromeTabId, randomId("tab"), owned);
    void this.#persist();
    return state;
  }

  fromHandle(tabId: string): TabState {
    const state = this.#byHandle.get(tabId);
    if (!state) throw new RpcError("TAB_NOT_FOUND", `unknown tab: ${tabId}`);
    return state;
  }

  fromChromeId(chromeTabId: number): TabState | undefined {
    return this.#byChromeId.get(chromeTabId);
  }

  all(): TabState[] {
    return Array.from(this.#byHandle.values());
  }

  async claim(
    tabId: string,
    sessionId: string,
    leaseId: string,
    metadata: { sessionName?: string; clientId?: string } = {},
  ): Promise<TabState> {
    const state = this.fromHandle(tabId);
    if (
      state.claim &&
      (state.claim.sessionId !== sessionId || state.claim.leaseId !== leaseId)
    ) {
      throw new RpcError(
        "LEASE_CONFLICT",
        `tab ${tabId} is already controlled`,
        { details: { tabId } },
      );
    }
    state.claim = {
      sessionId,
      leaseId,
      claimedAt: new Date().toISOString(),
      ...metadata,
    };
    await this.#persist();
    return state;
  }

  assertClaimed(tabId: string, sessionId?: string, leaseId?: string): TabState {
    const state = this.fromHandle(tabId);
    if (!state.claim)
      throw new RpcError("TAB_NOT_CLAIMED", `tab ${tabId} is not controlled`);
    if (sessionId && state.claim.sessionId !== sessionId)
      throw new RpcError("LEASE_REQUIRED", "session does not own this tab");
    if (leaseId && state.claim.leaseId !== leaseId)
      throw new RpcError("LEASE_REQUIRED", "lease does not own this tab");
    return state;
  }

  async release(tabId: string): Promise<TabState> {
    const state = this.fromHandle(tabId);
    state.claim = undefined;
    await this.#persist();
    return state;
  }

  async releaseAll(): Promise<TabState[]> {
    const claimed = this.all().filter((state) => state.claim);
    for (const state of claimed) state.claim = undefined;
    await this.#persist();
    return claimed;
  }

  noteNavigation(chromeTabId: number): TabState | undefined {
    const state = this.#byChromeId.get(chromeTabId);
    if (state) state.documentEpoch += 1;
    return state;
  }

  async remove(chromeTabId: number): Promise<TabState | undefined> {
    const state = this.#byChromeId.get(chromeTabId);
    if (!state) return undefined;
    this.#byChromeId.delete(chromeTabId);
    this.#byHandle.delete(state.tabId);
    await this.#persist();
    return state;
  }

  toPublic(state: TabState, tab?: chrome.tabs.Tab): Record<string, unknown> {
    return {
      tabId: state.tabId,
      windowId: tab?.windowId,
      index: tab?.index,
      url: safeTabUrl(tab?.url),
      title: tab?.title,
      active: tab?.active,
      pinned: tab?.pinned,
      groupId:
        tab?.groupId === chrome.tabGroups.TAB_GROUP_ID_NONE
          ? undefined
          : tab?.groupId,
      lastAccessed: tab?.lastAccessed,
      audible: tab?.audible,
      status: tab?.status,
      documentEpoch: state.documentEpoch,
      owned: state.owned,
      claimed: Boolean(state.claim),
    };
  }

  #register(chromeTabId: number, handle: string, owned: boolean): TabState {
    const state: TabState = {
      tabId: handle,
      chromeTabId,
      documentEpoch: 1,
      owned,
    };
    this.#byChromeId.set(chromeTabId, state);
    this.#byHandle.set(handle, state);
    return state;
  }

  async #persist(): Promise<void> {
    const tabHandles: Record<string, string> = {};
    const ownedTabs: number[] = [];
    const claimedChromeTabIds: number[] = [];
    for (const state of this.#byChromeId.values()) {
      tabHandles[String(state.chromeTabId)] = state.tabId;
      if (state.owned) ownedTabs.push(state.chromeTabId);
      if (state.claim) claimedChromeTabIds.push(state.chromeTabId);
    }
    await chrome.storage.session.set({
      tabHandles,
      ownedTabs,
      claimedChromeTabIds,
    });
  }
}

function safeTabUrl(value: string | undefined): string {
  if (!value) return "";
  try {
    const url = new URL(value);
    if (!(["http:", "https:"] as string[]).includes(url.protocol))
      return `${url.protocol}//`;
    return `${url.origin}${url.pathname}`;
  } catch {
    return "";
  }
}
