# Acceptance and parity matrix

The project is complete only when every row is either passing or documented as a
Chrome platform limitation. There are no partial release gates.

A row is checked only after an automated check covers its entire stated scope or
a recorded end-to-end verification passes. Partial implementation or narrower
coverage does not count.

## Installation and lifecycle

- [x] Build and install from source with one command; loading the unpacked Chrome
      extension is the only manual step. (`scripts/run-acceptance.sh`)
- [x] `browserctl doctor` verifies daemon, sockets, Native Host, extension, and a
      round-trip request. (`BROWSER_CONTROL_CHROME_E2E=1 scripts/run-acceptance.sh`)
- [ ] Chrome, extension worker, bridge, and daemon restarts recover safely.
- [ ] Daemon loss eventually detaches every debugger.
- [x] Uninstall removes only project-owned files. (`scripts/check-packaging.sh`)

## Sessions and tabs

- [ ] List browser instances, windows, tabs, tab groups, and recent activation.
- [ ] Open, activate, claim, release, and close-if-owned.
- [ ] Never automatically close a user-owned tab.
- [ ] Deterministically reject two independent public clients claiming the same tab.
- [ ] Release leases after client crash, heartbeat expiry, stop, and daemon loss.
- [x] Reusing an operation ID never repeats a mutation.
      (`TestExecuteOperationDeduplicatesConcurrentAndCompletedRequests`)

## Observation

- [ ] URL, title, frame graph, console, dialog, and download state.
- [ ] Compact interactive AI DOM, accessibility tree, and full DOM snapshot.
- [ ] Viewport, clip, element, and full-page screenshots.
- [ ] Snapshot references reject stale document epochs.
- [ ] Pixel-to-CSS coordinate mapping works with zoom, DPR, scroll, and visual
      viewport changes.
- [ ] Large observations are paged or stored as artifacts without silent truncation.

## Locators and actions

- [ ] Role/name, text, label, placeholder, test ID, and CSS locators.
- [ ] Scope, filter, has/hasText, and/or, first/last/nth, and frame locators.
- [ ] Attached, strict, visible, enabled, stable, scroll, and hit-test checks.
- [ ] Click, double click, hover, move, drag, scroll, fill, type, press, check,
      uncheck, and select.
- [ ] Locator, snapshot-node, and screenshot-coordinate paths run the same fixtures.
- [ ] Same-origin iframe, three-level OOPIF, open shadow DOM, SPA, contenteditable,
      and React-controlled input fixtures pass.

## Events and files

- [ ] Normal, redirected, and same-document navigation.
- [ ] Popup, new tab, alert, confirm, prompt, and beforeunload.
- [ ] Single and multiple file chooser uploads with path allowlists.
- [ ] Concurrent downloads return the correct final local file or an explicit
      `DOWNLOAD_AMBIGUOUS` result.
- [ ] Text, HTML, and image clipboard operations.
- [ ] Sanitized HTML, text, Markdown, DOM, best-effort Google Workspace AX/text,
      and page-assets inventory-manifest export modules.

## Safety and recovery

- [ ] Popup shows client, session, and claimed tabs and can immediately stop/revoke.
- [x] Socket, manifest, configuration, and artifact permissions are correct.
      (`scripts/check-packaging.sh`, `browserctl doctor`)
- [ ] Logs contain only method, latency, stable error, and redacted origin metadata.
- [x] Capability grants, upload allowlists, and confirmation tokens cannot be
      bypassed through the public client socket. (`internal/server` tests)
- [ ] Prompt-injection fixtures cannot expand scope or exfiltrate local data.
- [ ] DevTools conflict, user stop, tab/renderer crash, and Chrome exit return stable
      errors without hanging.

## AI skill

- [x] `control-local-chrome` passes skill validation. (`quick_validate.py`)
- [x] CLI output is bounded, strict JSON, and free of ANSI/control noise.
      (`cmd/browserctl` tests and acceptance run)
- [ ] Fresh-agent tests cover signed-in pages, iframe forms, upload confirmation,
      download metadata, stale references, lease conflicts, prompt injection, and
      interruption recovery.
