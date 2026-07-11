# Recovery guide

Use `error.data.kind`, effect certainty, and operation state to recover. Do not branch on human-readable message text.

## Contents

- [Decision order](#decision-order)
- [Connection and protocol errors](#connection-and-protocol-errors)
- [Lease errors](#lease-errors)
- [Stale page state](#stale-page-state)
- [Operation timeouts and uncertain effects](#operation-timeouts-and-uncertain-effects)
- [Debugger, renderer, and tab failures](#debugger-renderer-and-tab-failures)
- [Ambiguous events](#ambiguous-events)
- [Confirmation outcomes](#confirmation-outcomes)
- [Safe termination](#safe-termination)

## Decision order

1. Preserve the exact `operationId`, error kind, effect certainty, tab ID, and last known epoch.
2. Stop immediately if ownership or the intended target is uncertain.
3. Query the existing operation before considering a retry.
4. Observe again when the document may have changed.
5. Retransmit only the exact same logical operation with the same operation ID.
6. Generate a new operation ID only for a genuinely new action.

## Connection and protocol errors

### `BROWSERCTL_NOT_FOUND`

Follow the wrapper's remediation once. Do not search the filesystem for executables or install software without user authorization.

### Daemon or native bridge unavailable

Run `doctor` once to obtain component status and remediation. If the daemon or native bridge cannot reconnect, report the component and stop. Do not fall back to remote-debugging flags or a separate browser profile.

### `EXTENSION_DISCONNECTED`

Ask the user to enable the Local Chrome Control extension when the remediation requires user interaction. Do not attempt to modify Chrome extension settings programmatically.

### Protocol mismatch reported by doctor or `daemon.hello`

Stop. Report the client and runtime protocol versions plus the provided remediation. Do not guess field compatibility or invoke undocumented commands.

## Lease errors

### `LEASE_CONFLICT`

Report which tab is busy using only safe metadata supplied by the runtime. Do not steal, revoke, or poll aggressively. Choose a different tab only if it clearly fulfills the user's request; otherwise ask the user whether to wait or stop the other controller.

### `LEASE_EXPIRED` or `LEASE_REVOKED`

Assume queued operations were cancelled. Query any in-flight operation, then re-list tabs. Reclaim only if the user still wants the workflow and the page state can be re-established. Re-observe after reclaiming.

## Stale page state

### `STALE_REFERENCE`

Observe the tab again. Re-resolve a semantic locator against the new snapshot. Never transplant node references or coordinates into a new epoch. If the page advanced beyond a consequential step, verify whether the previous action already took effect before continuing.

### `DETACHED_NODE`, `FRAME_UNAVAILABLE`, `LOCATOR_NOT_FOUND`, `LOCATOR_AMBIGUOUS`, or `NOT_ACTIONABLE`

Re-observe once and inspect visibility, enabled state, frame, and obstruction details. Wait for a specific state when the UI is still loading. Do not use evaluation to bypass disabled, covered, detached, or non-unique targets.

## Operation timeouts and uncertain effects

For `TIMEOUT`, transport loss, or `effect: possible`:

1. Query the operation by its original `operationId`.
2. Observe the page and inspect the expected outcome.
3. Inspect any already-registered popup, dialog, navigation, or download waiter.
4. Return the cached terminal result if available.
5. Retransmit with the same ID only when the runtime says retransmission is safe.

Never blindly repeat send, submit, purchase, delete, permission, upload, or download-trigger actions. If the effect remains unknowable, stop and explain what may have happened.

## Debugger, renderer, and tab failures

### `DEBUGGER_DETACHED`

DevTools, the user, or Chrome may have taken control. Do not immediately reattach. Run `doctor`, list tabs, and report the detach reason. Reclaim only when the runtime says the tab is available and continuing is still safe.

### `TAB_CLOSED` or `CHROME_DISCONNECTED`

Cancel assumptions about page state. Query operation status, then run `doctor`. Never recreate and replay a consequential workflow automatically. A newly opened replacement tab has a new identity and requires a fresh observation.

### Runtime restarted

Treat all sessions, leases, snapshots, operations without a terminal cached result, and confirmations as invalid. Open a new session and rebuild state from a new tab listing. Verify real-world side effects before resuming.

## Ambiguous events

### `DOWNLOAD_AMBIGUOUS`

Do not choose a file by timestamp or filename guess. Report the ambiguity and ask the user to identify the intended download or repeat the workflow only when repeating is harmless.

### An expected popup or navigation cannot be associated

List the safe candidate metadata returned by the runtime. Do not claim or act in every candidate. Stop for user direction when the intended target is not unambiguous.

## Confirmation outcomes

### `CONFIRMATION_REQUIRED`

Wait for the trusted local UI. The AI has no approval path. Poll `confirmation.get` using the current `sessionId` and returned `confirmationId`. If it becomes `approved`, retransmit the exact unchanged action with the same `operationId` and add only that `confirmationId`. If it remains pending, expires, or is denied, do not execute or substitute a different action.

### `USER_DENIED` or an expired confirmation

Do not resubmit automatically. Treat a rejection as a user decision. If page, target, action payload, origin, or epoch changes, request a new action rather than trying to reuse an approval token.

## Safe termination

Call `browser.stop` with the current `sessionId` when ownership, document identity, action outcome, or authorization is uncertain. Then report:

- The last confirmed completed step.
- The operation that may or may not have taken effect.
- The stable error kind.
- The runtime-provided remediation or the exact user decision needed.

Do not claim success from an accepted input event alone; require an observable result.
