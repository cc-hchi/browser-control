#!/bin/sh

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
install_root=${BROWSER_CONTROL_INSTALL_ROOT:-"$HOME/Library/Application Support/browser-control"}
chrome_bin=${CHROME_BIN:-}

if [ -z "$chrome_bin" ]; then
  for candidate in \
    "/Applications/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing" \
    "/Applications/Chromium.app/Contents/MacOS/Chromium"; do
    if [ -x "$candidate" ]; then
      chrome_bin=$candidate
      break
    fi
  done
fi
if [ -z "$chrome_bin" ] && [ -d "$HOME/.cache/puppeteer/chrome" ]; then
  chrome_bin=$(find "$HOME/.cache/puppeteer/chrome" -type f \
    -path '*/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing' \
    -perm -111 | sort | tail -n 1)
fi

[ -x "$chrome_bin" ] || {
  printf '%s\n' "Chrome for Testing or Chromium was not found. Set CHROME_BIN to a compatible executable." >&2
  exit 1
}
chrome_version=$("$chrome_bin" --version 2>/dev/null || :)
case "$chrome_version" in
  "Google Chrome for Testing "*) ;;
  "Google Chrome "*)
    printf '%s\n' "Branded Google Chrome ignores --load-extension. Use Chrome for Testing or Chromium for the isolated E2E." >&2
    exit 1
    ;;
esac
[ -x "$install_root/bin/browserctl" ] || {
  printf 'browser-control is not installed at %s\n' "$install_root" >&2
  exit 1
}
[ -f "$install_root/extension/manifest.json" ] || {
  printf 'installed extension is missing at %s\n' "$install_root/extension" >&2
  exit 1
}

profile_dir=$(mktemp -d "${TMPDIR:-/tmp}/browser-control-chrome-profile.XXXXXX")
fixture_log=$(mktemp -t browser-control-chrome-fixture.XXXXXX)
chrome_log=$(mktemp -t browser-control-chrome.XXXXXX)
fixture_port=${FIXTURE_PORT:-$(node "$repo_dir/scripts/find-port-pair.mjs")}
native_dir="$profile_dir/NativeMessagingHosts"
native_manifest="$native_dir/com.browser_control.native_host.json"
mkdir -m 700 "$native_dir"
node "$repo_dir/scripts/render-native-manifest.mjs" \
  --extension-manifest "$install_root/extension/manifest.json" \
  --host-path "$install_root/bin/browser-native-host" \
  --output "$native_manifest"
chmod 600 "$native_manifest"

export BROWSER_CONTROL_STATE_DIR=${BROWSER_CONTROL_STATE_DIR:-$install_root}
export BROWSER_CONTROL_SOCKET=${BROWSER_CONTROL_SOCKET:-$install_root/browserd.sock}
export BROWSER_CONTROL_NATIVE_MANIFEST=$native_manifest

FIXTURE_PORT="$fixture_port" node "$repo_dir/tests/fixtures/server.mjs" >"$fixture_log" 2>&1 &
fixture_pid=$!
chrome_pid=
cleanup() {
  if [ -n "$chrome_pid" ]; then
    kill "$chrome_pid" 2>/dev/null || true
    cleanup_attempt=0
    while kill -0 "$chrome_pid" 2>/dev/null && [ "$cleanup_attempt" -lt 20 ]; do
      sleep 0.1
      cleanup_attempt=$((cleanup_attempt + 1))
    done
    kill -9 "$chrome_pid" 2>/dev/null || true
    wait "$chrome_pid" 2>/dev/null || true
  fi
  pkill -f "user-data-dir=$profile_dir" 2>/dev/null || true
  kill "$fixture_pid" 2>/dev/null || true
  wait "$fixture_pid" 2>/dev/null || true
  rm -rf "$profile_dir"
  rm -f "$fixture_log" "$chrome_log"
}
trap cleanup EXIT HUP INT TERM

fixture_ready=false
attempt=0
while [ "$attempt" -lt 30 ]; do
  if ! kill -0 "$fixture_pid" 2>/dev/null; then
    break
  fi
  if curl -fsS "http://127.0.0.1:$fixture_port/" | grep -Fq "Browser control fixture" && \
      curl -fsS "http://127.0.0.1:$((fixture_port + 1))/frame/2" | grep -Fq "Frame fixture"; then
    fixture_ready=true
    break
  fi
  sleep 0.1
  attempt=$((attempt + 1))
done
if [ "$fixture_ready" != true ]; then
  cat "$fixture_log" >&2
  exit 1
fi

"$chrome_bin" \
  --user-data-dir="$profile_dir" \
  --no-first-run \
  --no-default-browser-check \
  --disable-sync \
  --disable-component-update \
  --load-extension="$install_root/extension" \
  --new-window "http://127.0.0.1:$fixture_port/" >"$chrome_log" 2>&1 &
chrome_pid=$!

extension_ready=false
attempt=0
while [ "$attempt" -lt 80 ]; do
  if ! kill -0 "$chrome_pid" 2>/dev/null; then
    break
  fi
  if "$install_root/bin/browserctl" --json --timeout 2s doctor >/dev/null 2>&1; then
    extension_ready=true
    break
  fi
  sleep 0.25
  attempt=$((attempt + 1))
done
if [ "$extension_ready" != true ]; then
  cat "$chrome_log" >&2
  "$install_root/bin/browserctl" --json doctor >&2 || true
  exit 1
fi

BROWSERCTL_BIN="$install_root/bin/browserctl" FIXTURE_PORT="$fixture_port" node "$repo_dir/tests/chrome-e2e.mjs"
