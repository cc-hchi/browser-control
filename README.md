# browser-control

`browser-control` lets AI agents and local programs safely operate the user's
existing signed-in Google Chrome profile. It is a personal, macOS-only,
local-first tool: it has no MCP server, cloud service, or TCP listener.

The system is intentionally local-only and macOS-focused:

```text
AI skill / browserctl / JSON-RPC client
                  -> browserd
                  -> Chrome Native Messaging host
                  -> MV3 extension
                  -> chrome.debugger/CDP + Chrome extension APIs
```

The delivery target includes tab claiming, session-scoped state with exclusive tab leases, compact AI DOM
observations, screenshots, semantic locators, coordinate input, frames and open
shadow DOM, navigation waits, dialogs, upload/download, clipboard, page export,
trusted confirmations, emergency stop, and the `control-local-chrome` skill.

Local clients use a permission-restricted Unix domain socket or the strict-JSON
`browserctl` CLI. The extension popup is the trusted surface for capability and
consequential-action approval; an AI client cannot approve its own request.

## Requirements

- macOS and Google Chrome
- Go 1.23 or newer
- Node.js 20 or newer
- pnpm 10 (`corepack enable` can provide it)

## Quick install on another Mac

If the required developer tools are not installed yet and you use Homebrew:

```sh
brew install go node pnpm
```

Clone and install:

```sh
git clone https://github.com/cc-hchi/browser-control.git
cd browser-control
scripts/install.sh
```

After the installer finishes, load the unpacked extension in Chrome as described
below. To also make the browser-control Skill available to Codex, run
`scripts/install-skill.sh` from the same checkout.

## Install the runtime

On this Mac or another Mac, copy or clone the complete source checkout, then run
the installer from its root:

```sh
cd /path/to/browser-control
scripts/install.sh
```

The installer builds the Go binaries and extension, installs them under
`~/Library/Application Support/browser-control`, registers the Native Messaging
host and LaunchAgent, starts `browserd`, and verifies that the daemon responds.
It does not read or write `CODEX_HOME`, and it never installs a Skill. The first
run needs network access for `pnpm install` unless the dependency store is
already populated.

There is no separate start command. The installer starts the per-user
LaunchAgent immediately, and macOS starts it again after login or reboot.

Loading the unpacked extension is the only manual installation step:

1. Open `chrome://extensions` in Google Chrome.
2. Enable **Developer mode**.
3. Click **Load unpacked** and select
   `~/Library/Application Support/browser-control/extension`.
4. If that directory was already loaded, click **Reload** on the existing
   **Local Chrome Control** extension instead of loading a duplicate.

Keep the extension installed from that exact directory so upgrades retain its
stable extension identity and Native Messaging permission.

## Verify

After loading or reloading the extension, run:

```sh
"$HOME/Library/Application Support/browser-control/bin/browserctl" --json doctor
```

A healthy result reports the daemon, Native Messaging bridge, and Chrome
extension as connected. On failure, follow the single `remediation` field in the
JSON response before retrying.

For the isolated automated browser acceptance run, install Chrome for Testing or
Chromium and run `BROWSER_CONTROL_CHROME_E2E=1 scripts/run-acceptance.sh`.
Branded Google Chrome intentionally ignores command-line unpacked-extension
loading; normal personal use still targets the manually loaded extension there.

For direct integrations, use the canonical CLI shape:

```sh
"$HOME/Library/Application Support/browser-control/bin/browserctl" --json rpc METHOD --params 'JSON_OBJECT'
```

Use `--params -` to read one JSON object from stdin when shell quoting is unsafe.
Inspect a method before composing an unfamiliar request:

```sh
"$HOME/Library/Application Support/browser-control/bin/browserctl" --json describe action.perform
```

The description reports required and optional fields, lease and epoch requirements,
protected capabilities, and a minimal example when one is available.

## Install the Skill separately (optional)

Runtime installation and Skill installation are deliberately independent. To
copy the `control-local-chrome` Skill into Codex, run this separately:

```sh
scripts/install-skill.sh
```

Then start a new Codex task and ask naturally to operate local Chrome, or invoke
`$control-local-chrome`. The Skill always starts with `doctor`, uses the JSON
CLI, and leaves capability decisions to the extension popup.

Set `CODEX_HOME` only for the Skill command when installing into a non-default
Codex home:

```sh
CODEX_HOME=/absolute/path/to/codex-home scripts/install-skill.sh
```

Remove only the Skill with:

```sh
scripts/uninstall-skill.sh
```

The Skill installer refuses to overwrite an unrelated Skill. The uninstaller
also recognizes the legacy symlink created by older browser-control installers.
Neither command installs, starts, stops, or removes the runtime.

## Upgrade

Pull or copy the new source over this checkout, then rerun:

```sh
scripts/install.sh
```

Open `chrome://extensions`, click **Reload** on **Local Chrome Control**, and run
the `doctor` command above. The installer replaces only a runtime root carrying
this project's ownership marker and project-owned integration metadata; it
refuses to overwrite unrelated installations.

## Uninstall

First remove **Local Chrome Control** from `chrome://extensions`, then run the
installed self-contained uninstaller:

```sh
"$HOME/Library/Application Support/browser-control/support/uninstall.sh"
```

The uninstaller stops the LaunchAgent and removes only project-owned runtime,
Native Messaging, and LaunchAgent files. It never reads or modifies
`CODEX_HOME`; an independently installed Skill remains installed until
`scripts/uninstall-skill.sh` is run. It does not remove this source checkout or
unrelated files.

## Project status

See `docs/architecture.md`, `docs/security.md`, and `docs/parity-matrix.md` for
the implementation contract and current acceptance status. A checked parity row
means the complete row has been verified; partially implemented rows remain
unchecked.
