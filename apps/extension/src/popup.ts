export {};

interface PopupState {
  bridge: { connected: boolean; helloComplete: boolean; lastError?: string };
  claimedTabs: Array<{
    tabId: string;
    title?: string;
    url?: string;
    sessionId?: string;
    sessionName?: string;
    clientId?: string;
  }>;
  confirmations: Array<{
    confirmationId: string;
    title?: string;
    summary?: string;
    origin?: string;
    expiresAt?: string;
    sessionName?: string;
    clientId?: string;
  }>;
}

const dot = document.querySelector("#connection-dot")!;
const label = document.querySelector("#connection-label")!;
const tabs = document.querySelector("#claimed-tabs")!;
const tabCount = document.querySelector("#tab-count")!;
const confirmations = document.querySelector("#confirmations")!;
const confirmationCount = document.querySelector("#confirmation-count")!;

function text(tag: string, value: string, className?: string): HTMLElement {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}

function button(
  labelText: string,
  handler: () => Promise<void>,
  className?: string,
): HTMLButtonElement {
  const element = text("button", labelText, className) as HTMLButtonElement;
  element.addEventListener("click", () => void handler().then(refresh));
  return element;
}

function render(state: PopupState): void {
  dot.classList.toggle("connected", state.bridge.helloComplete);
  label.textContent = state.bridge.helloComplete
    ? "Connected to browserd"
    : state.bridge.lastError ||
      (state.bridge.connected
        ? "Negotiating protocol…"
        : "Native host unavailable");

  tabCount.textContent = String(state.claimedTabs.length);
  tabs.replaceChildren();
  if (!state.claimedTabs.length)
    tabs.append(text("div", "No tabs are controlled.", "empty"));
  for (const tab of state.claimedTabs) {
    const item = text("div", "", "item");
    item.append(text("div", tab.title || "Untitled tab", "item-title"));
    item.append(
      text(
        "div",
        [tab.url, tab.sessionName, tab.clientId, tab.sessionId]
          .filter(Boolean)
          .join("\n"),
        "item-detail",
      ),
    );
    const actions = text("div", "", "actions");
    actions.append(
      button("Release", () =>
        chrome.runtime.sendMessage({
          source: "popup",
          type: "release",
          tabId: tab.tabId,
        }),
      ),
    );
    item.append(actions);
    tabs.append(item);
  }

  confirmationCount.textContent = String(state.confirmations.length);
  confirmations.replaceChildren();
  if (!state.confirmations.length)
    confirmations.append(
      text("div", "Nothing is waiting for approval.", "empty"),
    );
  for (const confirmation of state.confirmations) {
    const item = text("div", "", "item");
    item.append(
      text("div", confirmation.title || "Confirm browser action", "item-title"),
    );
    item.append(
      text(
        "div",
        [
          confirmation.summary,
          confirmation.origin,
          [confirmation.sessionName, confirmation.clientId]
            .filter(Boolean)
            .join(" · "),
          confirmation.expiresAt,
        ]
          .filter(Boolean)
          .join("\n"),
        "item-detail",
      ),
    );
    const actions = text("div", "", "actions");
    actions.append(
      button(
        "Approve",
        () =>
          chrome.runtime.sendMessage({
            source: "popup",
            type: "confirmation",
            confirmationId: confirmation.confirmationId,
            approved: true,
          }),
        "approve",
      ),
      button("Deny", () =>
        chrome.runtime.sendMessage({
          source: "popup",
          type: "confirmation",
          confirmationId: confirmation.confirmationId,
          approved: false,
        }),
      ),
    );
    item.append(actions);
    confirmations.append(item);
  }
}

async function refresh(): Promise<void> {
  const response = (await chrome.runtime.sendMessage({
    source: "popup",
    type: "status",
  })) as PopupState;
  render(response);
}

document.querySelector("#stop-all")!.addEventListener("click", () => {
  void chrome.runtime
    .sendMessage({ source: "popup", type: "stopAll" })
    .then(refresh);
});
void refresh();
