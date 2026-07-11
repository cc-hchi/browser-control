# Architecture

## Product boundary

`browserd` is the only state and policy authority. The skill and CLI are clients;
they do not own browser state or security decisions. The Chrome extension is the
source of truth for live targets, frames, debugger attachment, and Chrome events.
The Native Messaging host is a stateless framing bridge.

The project is one complete delivery target. The implementation order does not
define partial product releases.

## Components

```text
control-local-chrome skill
          | (browserctl --json)
browserctl / JSON-RPC clients
          | (0600 Unix domain socket)
       browserd
          | (private bridge socket)
browser-native-host
          | (Chrome Native Messaging)
     MV3 extension
       /       \
Chrome APIs   chrome.debugger/CDP
```

### browserd

- Own sessions, exclusive tab leases, operation queues, idempotency records,
  confirmations, bounded event history, and artifacts.
- Serialize every operation for a claimed tab.
- Invalidate sessions and leases after daemon restart rather than attempting an
  unsafe state reconstruction.
- Never listen on TCP. Keep the client and admin sockets in a mode-0700 runtime
  directory and create the sockets with mode 0600.

### Native Messaging host

- Implement Chrome's native-endian 32-bit length prefix and UTF-8 JSON payload.
- Keep stdout protocol-clean and write diagnostics only to stderr.
- Forward messages between the extension and the daemon without interpreting
  browser operations.
- Split large logical messages into 256-512 KiB chunks.

### Chrome extension

- Use an MV3 service worker and `runtime.connectNative()` with reconnect.
- Attach to claimed tabs through `chrome.debugger` using CDP 1.3.
- Track same-process frame execution contexts and recursively auto-attach to OOPIF
  targets with flat sessions.
- Dynamically inject the locator/overlay runtime only into claimed tabs.
- Expose a trusted popup for connection state, confirmations, lease revocation,
  and emergency stop.

## Ownership and lifecycle

- A tab has at most one controlling lease. Listing unclaimed tabs does not create
  a lease.
- User tabs are released and kept open by default. Only session-owned tabs may be
  automatically closed.
- Every page-side or browser-side mutation forwarded to Chrome has a
  caller-generated operation ID. Exact retries are deduplicated and never
  execute that browser action more than once; terminal outcomes remain
  queryable through the operation API.
- A main-document commit increments `documentEpoch`. Same-document navigation,
  layout, scroll, and DOM changes increment `revision` and invalidate snapshot
  references when appropriate.
- If the daemon/bridge is unavailable beyond a short grace period, the extension
  detaches every debugger target and clears claimed-tab state.

## Browser automation model

Observations return compact AI DOM by default, with optional full DOM,
accessibility, screenshot, console, dialog, and frame graph data. Every observation
creates an immutable snapshot. Node references are scoped to that snapshot.

Targets are serializable locator ASTs, snapshot node references, or screenshot
points. Locator actions resolve afresh and check attached, unique, visible, enabled,
stable, in-view, and unobscured state before issuing CDP Input events.

Waiters for navigation, popup, download, file chooser, and JavaScript dialogs are
armed before the input event in the same operation to avoid races.
