#!/bin/sh

set -eu
umask 077

fail() {
  printf 'browser-control install: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_uint() {
  case "$2" in
    ''|*[!0-9]*) fail "could not parse $1 version: $2" ;;
  esac
}

check_tool_versions() {
  go_version=$(go env GOVERSION 2>/dev/null || :)
  go_numeric=${go_version#go}
  go_major=${go_numeric%%.*}
  go_rest=${go_numeric#*.}
  go_minor=${go_rest%%.*}
  go_minor=${go_minor%%[!0-9]*}
  require_uint Go "$go_major"
  require_uint Go "$go_minor"
  if [ "$go_major" -lt 1 ] || { [ "$go_major" -eq 1 ] && [ "$go_minor" -lt 23 ]; }; then
    fail "Go 1.23 or newer is required; found $go_version"
  fi

  node_version=$(node --version 2>/dev/null || :)
  node_numeric=${node_version#v}
  node_major=${node_numeric%%.*}
  require_uint Node.js "$node_major"
  [ "$node_major" -ge 20 ] || fail "Node.js 20 or newer is required; found $node_version"

  pnpm_version=$(pnpm --version 2>/dev/null || :)
  pnpm_major=${pnpm_version%%.*}
  require_uint pnpm "$pnpm_major"
  [ "$pnpm_major" -ge 10 ] || fail "pnpm 10 or newer is required; found $pnpm_version"
}

ensure_absolute() {
  case "$2" in
    /*) ;;
    *) fail "$1 must be an absolute path: $2" ;;
  esac
}

replace_directory() {
  source_dir=$1
  destination_dir=$2
  new_dir="${destination_dir}.install-new.$$"
  old_dir="${destination_dir}.install-old.$$"

  rm -rf "$new_dir" "$old_dir"
  cp -R "$source_dir" "$new_dir"
  if [ -e "$destination_dir" ]; then
    mv "$destination_dir" "$old_dir"
  fi
  if ! mv "$new_dir" "$destination_dir"; then
    if [ -e "$old_dir" ]; then
      mv "$old_dir" "$destination_dir"
    fi
    fail "could not install $destination_dir"
  fi
  rm -rf "$old_dir"
}

[ "$(uname -s)" = "Darwin" ] || fail "only macOS is supported"
[ -n "${HOME:-}" ] || fail "HOME is not set"
ensure_absolute HOME "$HOME"

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
app_dir="$HOME/Library/Application Support/browser-control"
native_dir="$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts"
native_manifest="$native_dir/com.browser_control.native_host.json"
launch_agents_dir="$HOME/Library/LaunchAgents"
launch_label="com.browser-control.browserd"
launch_agent="$launch_agents_dir/$launch_label.plist"
owner_file="$app_dir/.browser-control-install"
owner_value=$(tr -d '\r\n' <"$repo_dir/packaging/INSTALL_OWNER")
skip_launchd=${BROWSER_CONTROL_SKIP_LAUNCHD:-0}
upload_roots=${BROWSER_CONTROL_UPLOAD_ROOTS:-"$HOME/Downloads:$HOME/Desktop"}

[ -n "$owner_value" ] || fail "installation ownership marker is empty"

require_command go
require_command node
require_command pnpm
require_command install
check_tool_versions

if [ -L "$app_dir" ]; then
  fail "refusing to install through a symlinked runtime directory: $app_dir"
fi
if [ -e "$app_dir" ]; then
  [ ! -L "$owner_file" ] || fail "refusing a symlinked ownership marker: $owner_file"
  [ -f "$owner_file" ] || fail "refusing to overwrite unowned directory: $app_dir"
  installed_owner=$(tr -d '\r\n' <"$owner_file")
  [ "$installed_owner" = "$owner_value" ] || fail "installation ownership marker is not recognized: $owner_file"
  for managed_path in bin extension artifacts logs support; do
    [ ! -L "$app_dir/$managed_path" ] || fail "refusing a symlinked managed path: $app_dir/$managed_path"
  done
fi

if [ -L "$native_manifest" ]; then
  fail "refusing to overwrite a symlinked Native Messaging manifest: $native_manifest"
fi
if [ -e "$native_manifest" ]; then
  if ! node "$repo_dir/scripts/render-native-manifest.mjs" --check-owned "$native_manifest" --install-root "$app_dir"; then
    fail "refusing to overwrite an unowned Native Messaging manifest: $native_manifest"
  fi
fi

if [ -L "$launch_agent" ]; then
  fail "refusing to overwrite a symlinked LaunchAgent: $launch_agent"
fi
if [ -e "$launch_agent" ] && ! grep -Fq "<!-- $owner_value -->" "$launch_agent"; then
  fail "refusing to overwrite an unowned LaunchAgent: $launch_agent"
fi

stage_dir=$(mktemp -d "${TMPDIR:-/tmp}/browser-control-install.XXXXXX")
cleanup() {
  rm -rf "$stage_dir"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$stage_dir/bin" "$stage_dir/extension" "$stage_dir/support"

printf '%s\n' "Building Go binaries..."
(cd "$repo_dir" && go build -trimpath -o "$stage_dir/bin/browserd" ./cmd/browserd)
(cd "$repo_dir" && go build -trimpath -o "$stage_dir/bin/browser-native-host" ./cmd/browser-native-host)
(cd "$repo_dir" && go build -trimpath -o "$stage_dir/bin/browserctl" ./cmd/browserctl)

printf '%s\n' "Installing TypeScript dependencies and building the workspace..."
if [ -f "$repo_dir/pnpm-lock.yaml" ]; then
  (cd "$repo_dir" && CI=1 pnpm install --frozen-lockfile)
else
  (cd "$repo_dir" && CI=1 pnpm install --no-lockfile)
fi
(cd "$repo_dir" && pnpm build)

[ -f "$repo_dir/apps/extension/dist/manifest.json" ] || fail "extension build did not produce dist/manifest.json"
cp -R "$repo_dir/apps/extension/dist/." "$stage_dir/extension/"
cp "$repo_dir/scripts/uninstall.sh" "$stage_dir/support/uninstall.sh"
cp "$repo_dir/packaging/INSTALL_OWNER" "$stage_dir/support/INSTALL_OWNER"

find "$stage_dir/bin" -type d -exec chmod 700 {} \;
find "$stage_dir/bin" -type f -exec chmod 700 {} \;
find "$stage_dir/extension" -type d -exec chmod 700 {} \;
find "$stage_dir/extension" -type f -exec chmod 600 {} \;
find "$stage_dir/support" -type d -exec chmod 700 {} \;
find "$stage_dir/support" -type f -exec chmod 600 {} \;
chmod 700 "$stage_dir/support/uninstall.sh"

extension_id=$(node "$repo_dir/scripts/extension-id.mjs" "$stage_dir/extension/manifest.json")
[ -n "$extension_id" ] || fail "could not derive the extension ID"

# Render and validate every metadata file before changing the live install.
node "$repo_dir/scripts/render-native-manifest.mjs" \
  --extension-manifest "$stage_dir/extension/manifest.json" \
  --host-path "$app_dir/bin/browser-native-host" \
  --output "$stage_dir/com.browser_control.native_host.json"
node "$repo_dir/packaging/render-launchd.mjs" \
  --template "$repo_dir/packaging/launchd/com.browser-control.browserd.plist.in" \
  --output "$stage_dir/$launch_label.plist" \
  --label "$launch_label" \
  --browserd "$app_dir/bin/browserd" \
  --state-dir "$app_dir" \
  --socket "$app_dir/browserd.sock" \
  --bridge-socket "$app_dir/bridge.sock" \
  --bridge-token-file "$app_dir/bridge.token" \
  --artifact-dir "$app_dir/artifacts" \
  --upload-roots "$upload_roots" \
  --home "$HOME" \
  --stdout-log "$app_dir/logs/browserd.stdout.log" \
  --stderr-log "$app_dir/logs/browserd.stderr.log"
if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$stage_dir/$launch_label.plist" >/dev/null
fi

install -d -m 700 "$app_dir"
install -d -m 700 "$app_dir/artifacts" "$app_dir/logs"
touch "$app_dir/logs/browserd.stdout.log" "$app_dir/logs/browserd.stderr.log"
chmod 600 "$app_dir/logs/browserd.stdout.log" "$app_dir/logs/browserd.stderr.log"
install -m 600 "$repo_dir/packaging/INSTALL_OWNER" "$owner_file"
replace_directory "$stage_dir/bin" "$app_dir/bin"
replace_directory "$stage_dir/extension" "$app_dir/extension"
replace_directory "$stage_dir/support" "$app_dir/support"

if [ ! -d "$native_dir" ]; then
  install -d -m 700 "$native_dir"
fi
install -m 600 "$stage_dir/com.browser_control.native_host.json" "$native_manifest"

if [ ! -d "$launch_agents_dir" ]; then
  install -d -m 700 "$launch_agents_dir"
fi

if [ "$skip_launchd" != "1" ]; then
  require_command launchctl
  launch_domain="gui/$(id -u)"
  launchctl bootout "$launch_domain" "$launch_agent" >/dev/null 2>&1 || \
    launchctl bootout "$launch_domain/$launch_label" >/dev/null 2>&1 || true
fi
install -m 600 "$stage_dir/$launch_label.plist" "$launch_agent"
if [ "$skip_launchd" != "1" ]; then
  launchctl bootstrap "$launch_domain" "$launch_agent"
  launchctl enable "$launch_domain/$launch_label"
  launchctl kickstart -k "$launch_domain/$launch_label"
  daemon_ready=false
  daemon_attempt=0
  while [ "$daemon_attempt" -lt 100 ]; do
    if [ -S "$app_dir/browserd.sock" ] && \
        "$app_dir/bin/browserctl" --json --socket "$app_dir/browserd.sock" --timeout 1s \
          rpc daemon.status --params '{}' >/dev/null 2>&1; then
      daemon_ready=true
      break
    fi
    sleep 0.1
    daemon_attempt=$((daemon_attempt + 1))
  done
  if [ "$daemon_ready" != "true" ]; then
    printf '%s\n' "browserd did not become ready; recent daemon errors:" >&2
    tail -n 20 "$app_dir/logs/browserd.stderr.log" >&2 || true
    fail "LaunchAgent started but browserd is not responding at $app_dir/browserd.sock"
  fi
fi

printf '\n%s\n' "browser-control installed successfully."
printf '  Extension ID: %s\n' "$extension_id"
printf '  Runtime:      %s\n' "$app_dir"
printf '  Uninstaller:  %s\n' "$app_dir/support/uninstall.sh"
printf '\n%s\n' "One manual Chrome step remains:"
printf '%s\n' "  1. Open chrome://extensions and enable Developer mode."
printf '  2. Click Load unpacked and select: %s\n' "$app_dir/extension"
printf '%s\n' "  If it was already loaded, click Reload instead. The stable extension ID must be $extension_id."
printf '\nRun after loading the extension:\n  %s doctor\n' "$app_dir/bin/browserctl"
