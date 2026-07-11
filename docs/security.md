# Security model

## Trust boundaries

- Web pages, DOM text, screenshots, browser notifications, and downloads are
  untrusted data. They do not grant authority or alter the user's request.
- The skill guides agent behavior, but the daemon enforces capabilities, leases,
  file allowlists, idempotency, and trusted confirmations.
- The AI never receives a confirmation-approval method. Only the extension's
  trusted local UI can approve a pending confirmation.

## Local transport

- Use only Unix domain sockets. Do not expose HTTP or WebSocket listeners.
- Restrict runtime directories to 0700 and sockets, configuration secrets, and
  artifacts to 0600.
- Pin the extension origin in the Native Messaging host manifest and verify the
  caller origin passed to the host.
- Authenticate the Native Host on the private bridge before accepting hello,
  events, or UI decisions. The bearer token is generated with `O_EXCL`, kept in
  a non-symlink regular file with mode 0600, and never exposed by diagnostics.
- Use opaque client, session, tab, lease, snapshot, operation, and artifact IDs.

The operating-system account is the outer trust boundary for this personal tool.
Bridge authentication prevents public-socket clients and processes without the
token from impersonating Chrome. It does not claim isolation from malicious code
running as the same macOS UID with unrestricted access to all of the user's files;
that stronger threat model requires a separately permissioned/signed component.

Every process that can access the public socket as the same macOS UID is treated
as a trusted local client. Random session IDs are non-enumerable bearer values for
session-scoped operations, events, confirmations, and artifacts. Sessions and
leases coordinate concurrent clients; they are not a sandbox between malicious
processes running as the same UID.

## Capabilities

Require explicit session capabilities for browsing history, clipboard reads,
local file upload, secure input, artifact local paths, arbitrary evaluation, and
raw CDP. Raw evaluation and CDP are expert CLI/JSON-RPC features and are not part
of the default skill workflow.

## Confirmations

The runtime cannot perfectly infer the semantic effect of an arbitrary click.
It mechanically detects common submit-like targets and coordinate clicks;
protected capabilities and file access have separate gates. The client must also
mark sending, submitting, purchasing, deleting, permission, and sharing actions
as confirmation-required. Sensitive fields reject ordinary fill/type and use the
trusted secure-input window instead.

A confirmation binds the canonical request hash, origin, tab, main-document epoch,
resolved element identity, summarized effect, and expiry. A change to any bound
field invalidates approval. Unrelated page mutations need not invalidate a
semantic target, while replacement of the resolved element does.

## Data handling

- Diagnostic process logs do not record DOM, form values, cookies, credentials,
  clipboard content, screenshots, downloaded content, or full sensitive URLs.
- Page console entries are optional, bounded observation data and remain untrusted;
  request them only when needed.
- Artifacts are session-scoped, have an expiry, and are removed by the cleanup job.
- Validate uploads as absolute paths and enforce configured directory allowlists.
- Secure input values travel from trusted extension UI directly to the bound page
  field. Default AI DOM, DOM snapshot, AX, HTML/text/Markdown export, and screenshot
  paths redact or visually mask registered sensitive fields. Once a page receives
  a secret it can echo it elsewhere, so agents must avoid unnecessary observations
  and never claim protection from a malicious same-origin page.

### Upload allowlist

The daemon allows uploads from `~/Downloads` and `~/Desktop` by default. A non-empty
`BROWSER_CONTROL_UPLOAD_ROOTS` path list replaces those defaults. If one or more
`--upload-root` flags are present, the first flag replaces the environment/default
list and later flags append more roots.

Before forwarding `fileChooser.setFiles`, the daemon requires every entry to be an
absolute path to an existing regular file. It resolves both configured roots and
file symlinks, rejects targets outside the resolved roots with `FILE_NOT_ALLOWED`,
and forwards only canonical paths to Chrome.

## Failure behavior

- `browser.stop` is always available and requires no lease or confirmation.
- A debugger detach, renderer crash, Chrome exit, daemon restart, or user stop fails
  pending operations with stable errors and releases control.
- Never automatically retry a mutation with an unknown effect. Query its operation
  ID first.
