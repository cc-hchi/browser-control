# browserctl API reference

Use this reference when composing RPC calls, selecting capabilities, or interpreting JSON responses. Invoke every command through `scripts/browserctl`; the wrapper supplies `--json`.

## Contents

- [Invocation](#invocation)
- [Identifiers and state](#identifiers-and-state)
- [Methods](#methods)
- [Locators and targets](#locators-and-targets)
- [Actions](#actions)
- [Capabilities](#capabilities)
- [Errors](#errors)

## Invocation

Run diagnostics with the dedicated command:

```sh
scripts/browserctl doctor
```

Call every browser API method through the stable generic interface:

```sh
scripts/browserctl rpc METHOD --params 'JSON_OBJECT'
```

The CLI sends JSON-RPC 2.0 to the local daemon and prints one JSON response. Treat a nonzero exit status as failure even if stdout is parseable. Pass `--params -` and provide one JSON object on stdin when shell quoting would be fragile or the payload contains sensitive user-authored text.

Do not depend on convenience CLI verbs: they may wrap the methods below, but `rpc` is the canonical Skill interface.

## Identifiers and state

- Pass opaque `sessionId`, `tabId`, `leaseId`, `snapshotId`, `operationId`, `confirmationId`, and `artifactId` values exactly as returned.
- Treat `sessionId` as a non-enumerable bearer value. Pass it to every session-scoped operation, event, confirmation, and artifact method; never disclose another task's ID.
- Never infer a Chrome tab ID, CDP node ID, or local path from an opaque ID.
- Treat `documentEpoch` as the main-document generation. Navigation or reload invalidates old snapshot node references.
- Generate a collision-resistant `operationId` beginning with `op_` for each new mutation.
- Reuse an operation ID only to query or retransmit the exact same logical operation.

Tab-scoped reads and actions generally require:

```json
{
  "sessionId": "ses_...",
  "tabId": "tab_...",
  "leaseId": "lease_..."
}
```

## Methods

### Diagnostics and sessions

| Method | Purpose |
|---|---|
| `daemon.hello` | Negotiate protocol and client identity. |
| `daemon.status` | Read daemon and bridge status. |
| `daemon.diagnostics` | Return component diagnostics and remediation. |
| `session.open` | Open a named session with no protected capabilities. |
| `session.get` | Read session state and granted capabilities. |
| `session.requestCapabilities` | Request additional capabilities explicitly. |
| `session.close` | Release normal session resources. |
| `session.stop` | Cancel and fail closed for one session. |

Start with `doctor`, then call `session.open`. When protected capabilities are needed, call `session.requestCapabilities`, wait for the trusted extension UI decision, and verify the resulting grants with `session.get`. Parameters supplied to `session.open` never grant capabilities.

### Browser and tabs

| Method | Purpose |
|---|---|
| `browser.list` | List connected Chrome profile instances. |
| `browser.history.query` | Query profile history with capability protection. |
| `tab.list` | List safe tab metadata. |
| `tab.get` | Read one tab's metadata and ownership. |
| `tab.open` | Open a session-owned tab. |
| `tab.claim` | Acquire the tab's exclusive write lease. |
| `tab.lease.renew` | Renew an active lease during a long-running workflow. |
| `tab.activate` | Bring a claimed tab to the foreground. |
| `tab.release` | Release a claimed user or owned tab. |
| `tab.close` | Close a tab; use only when ownership metadata permits it. |
| `tab.navigate` | Navigate with operation and epoch protection. |
| `tab.back`, `tab.forward`, `tab.reload` | Perform history/reload navigation. |

Claim before observing or acting. Keep the returned `leaseId`, expiry, ownership, and `documentEpoch`. Never close a pre-existing user tab.

### Observation, location, action, and wait

| Method | Purpose |
|---|---|
| `observation.capture` | Capture interactive/full DOM, AX data, screenshot, frame graph, and logs. |
| `observation.diff` | Capture a fresh immutable replacement snapshot linked to a base snapshot. |
| `locator.query` | Resolve a locator and read count, text, attributes, state, or bounds. |
| `action.perform` | Perform one idempotency-protected browser action. |
| `condition.wait` | Wait for a URL, locator state, or registered browser condition. |

Prefer compact interactive DOM. Request a screenshot when layout, canvas state, drag-and-drop, or visual fallback matters. Native transport messages are chunked, AI DOM reports `truncated: true` explicitly, and artifacts alone support `artifact.readChunk`; narrow and recapture a truncated observation.

### Dialogs, files, downloads, and exports

| Method | Purpose |
|---|---|
| `dialog.get`, `dialog.respond` | Inspect and accept/dismiss/prompt a JavaScript dialog. |
| `fileChooser.setFiles` | Set user-authorized absolute paths on a registered chooser. |
| `download.get`, `download.wait` | Read or wait for associated terminal download metadata, including final local `fileName`. |
| `content.export` | Export sanitized `html`, `text`, `markdown`, or `dom`; `googleWorkspace` is a best-effort visible-text/AX JSON export for `docs.google.com`. |
| `pageAssets.list`, `pageAssets.export` | Inspect page assets or export their redacted inventory manifest. |
| `artifact.get`, `artifact.readChunk`, `artifact.delete` | Read metadata, stream content, or delete a same-session artifact; always pass `sessionId`. |

Register popup, dialog, file chooser, download, and navigation expectations in `action.perform` before dispatching input.

### Clipboard, operations, events, and confirmation

| Method | Purpose |
|---|---|
| `clipboard.read`, `clipboard.write` | Access capability-protected clipboard MIME data. |
| `operation.get`, `operation.wait`, `operation.cancel` | Inspect or control a same-session operation without repeating it; pass `sessionId`. |
| `event.next`, `event.replay` | Consume ordered same-session runtime events; pass `sessionId`. |
| `confirmation.get`, `confirmation.list` | Observe same-session trusted confirmation state; pass `sessionId`; no AI approval method exists. |
| `secureInput.request` | Ask trusted UI to collect and fill a secret. |
| `browser.stop` | Emergency stop without requiring a lease or confirmation. |

### Expert escape hatches

`unsafe.evaluate` and `unsafe.cdp.send` require explicit capabilities and user intent. Do not use them to bypass locator actionability, confirmations, unsupported browser surfaces, or page security boundaries.

## Locators and targets

A semantic locator has `by` equal to `role`, `text`, `label`, `placeholder`, `testId`, or `css`. A role locator carries `role` and may carry a string or exact-text `name`. Locators may include a frame path, scope, `has`, `hasText`, `and`, `or`, `first`, `last`, `nth`/`index`, and open-shadow policy.

```json
{
  "locator": {
    "by": "role",
    "role": "button",
    "name": {"text": "Save", "exact": true}
  }
}
```

A snapshot-node target is bound to one snapshot:

```json
{"snapshotId":"snap_...","nodeRef":"e14"}
```

A coordinate target is bound to the screenshot's snapshot and CSS viewport:

```json
{"snapshotId":"snap_...","point":{"x":420,"y":315}}
```

Prefer semantic locators, then a current node reference, then a current screenshot point. Require a unique, visible, enabled, stable, and unobstructed target for mutation.

## Actions

Call `action.perform` with an `ActionRequest`:

```sh
scripts/browserctl rpc action.perform --params '{
  "sessionId":"ses_...",
  "tabId":"tab_...",
  "leaseId":"lease_...",
  "operationId":"op_...",
  "expectedDocumentEpoch":18,
  "action":{
    "type":"click",
    "target":{"locator":{"by":"role","role":"button","name":"Save"}}
  },
  "confirmation":{"required":true,"reason":"submit profile changes"},
  "expect":[{"type":"navigation","timeoutMs":30000}],
  "observeAfter":{"dom":"interactive"},
  "timeoutMs":30000
}'
```

Action types are `click`, `doubleClick`, `hover`, `move`, `drag`, `scroll`, `fill`, `type`, `press`, `focus`, `check`, `uncheck`, `select`, `navigate`, `back`, `forward`, `reload`, `downloadMedia`, `dialogAccept`, `dialogDismiss`, and `dialogPrompt`.

Expected event types are `navigation`, `popup`, `download`, `fileChooser`, and `dialog`. Put expectations in the same request as the triggering action.

## Capabilities

Request only those required by the task:

- `history.read`
- `clipboard.read`
- `clipboard.write`
- `files.upload`
- `files.download`
- `secureInput`
- `artifact.localPath`
- `unsafe.evaluate`
- `unsafe.cdp`

Capability grants do not replace trusted confirmation for consequential actions.

`artifact.get` returns only portable metadata by default. Set `includeLocalPath: true` only when the user needs a filesystem path and the session holds `artifact.localPath`; never infer the daemon's private artifact path.

For a consequential `action.perform`, set `confirmation.required` and a short non-secret `reason`. The first call returns `CONFIRMATION_REQUIRED` with a `confirmationId` without executing the action. Poll `confirmation.get` with both `sessionId` and `confirmationId`; only after status becomes `approved`, retransmit the unchanged request with the same `operationId` plus that `confirmationId`. Never invent, approve, or substitute a confirmation ID.

## Errors

A protocol failure uses JSON-RPC error data:

```json
{
  "jsonrpc":"2.0",
  "id":"request-id",
  "error":{
    "code":-32010,
    "message":"The snapshot no longer belongs to the current document.",
    "data":{
      "kind":"STALE_REFERENCE",
      "retryable":true,
      "effect":"none",
      "operationId":"op_...",
      "expectedDocumentEpoch":18,
      "actualDocumentEpoch":19,
      "recovery":{"method":"observation.capture","reason":"Observe the new document."}
    }
  }
}
```

Branch on `error.data.kind`, `retryable`, and `effect`; never branch on message text alone. Follow `error.data.recovery` when present. Read [recovery.md](recovery.md) before retrying a mutation.
