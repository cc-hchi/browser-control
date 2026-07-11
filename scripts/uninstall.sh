#!/bin/sh

set -eu
umask 077

fail() {
  printf 'browser-control uninstall: %s\n' "$*" >&2
  exit 1
}

[ "$(uname -s)" = "Darwin" ] || fail "only macOS is supported"
[ -n "${HOME:-}" ] || fail "HOME is not set"
case "$HOME" in
  /*) ;;
  *) fail "HOME must be an absolute path: $HOME" ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -f "$script_dir/../packaging/INSTALL_OWNER" ]; then
  owner_source="$script_dir/../packaging/INSTALL_OWNER"
elif [ -f "$script_dir/INSTALL_OWNER" ]; then
  owner_source="$script_dir/INSTALL_OWNER"
else
  fail "could not locate project ownership metadata next to the uninstaller"
fi
app_dir="$HOME/Library/Application Support/browser-control"
native_manifest="$HOME/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.browser_control.native_host.json"
launch_label="com.browser-control.browserd"
launch_agent="$HOME/Library/LaunchAgents/$launch_label.plist"
owner_file="$app_dir/.browser-control-install"
owner_value=$(tr -d '\r\n' <"$owner_source")
skip_launchd=${BROWSER_CONTROL_SKIP_LAUNCHD:-0}

[ -n "$owner_value" ] || fail "installation ownership marker is empty"

owned_install=0
if [ -L "$app_dir" ] || [ -L "$owner_file" ]; then
  printf 'Leaving symlinked runtime path untouched: %s\n' "$app_dir" >&2
elif [ -f "$owner_file" ]; then
  installed_owner=$(tr -d '\r\n' <"$owner_file")
  if [ "$installed_owner" = "$owner_value" ]; then
    owned_install=1
  else
    fail "refusing to remove installation with an unrecognized ownership marker: $owner_file"
  fi
elif [ -e "$app_dir" ]; then
  printf 'Leaving unowned directory untouched: %s\n' "$app_dir" >&2
fi

owned_launch_agent=0
if [ -L "$launch_agent" ]; then
  printf 'Leaving symlinked LaunchAgent untouched: %s\n' "$launch_agent" >&2
elif [ -e "$launch_agent" ]; then
  if grep -Fq "<!-- $owner_value -->" "$launch_agent"; then
    owned_launch_agent=1
  else
    printf 'Leaving unowned LaunchAgent untouched: %s\n' "$launch_agent" >&2
  fi
fi

owned_native_manifest=0
if [ -L "$native_manifest" ]; then
  printf 'Leaving symlinked Native Messaging manifest untouched: %s\n' "$native_manifest" >&2
elif [ -e "$native_manifest" ]; then
  canonical_app_dir=
  if [ "$owned_install" = "1" ]; then
    canonical_app_dir=$(CDPATH= cd -L -- "$app_dir" && pwd -L)
  fi
  manifest_name=$(/usr/bin/plutil -extract name raw -o - "$native_manifest" 2>/dev/null || :)
  manifest_description=$(/usr/bin/plutil -extract description raw -o - "$native_manifest" 2>/dev/null || :)
  manifest_path=$(/usr/bin/plutil -extract path raw -o - "$native_manifest" 2>/dev/null || :)
  manifest_type=$(/usr/bin/plutil -extract type raw -o - "$native_manifest" 2>/dev/null || :)
  manifest_origin_count=$(/usr/bin/plutil -extract allowed_origins raw -o - "$native_manifest" 2>/dev/null || :)
  manifest_origin=$(/usr/bin/plutil -extract allowed_origins.0 raw -o - "$native_manifest" 2>/dev/null || :)
  if [ "$manifest_name" = "com.browser_control.native_host" ] && \
      [ "$manifest_description" = "browser-control native messaging bridge" ] && \
      [ "$manifest_path" = "$canonical_app_dir/bin/browser-native-host" ] && \
      [ "$manifest_type" = "stdio" ] && \
      [ "$manifest_origin_count" = "1" ] && \
      [ "$manifest_origin" = "chrome-extension://bfnlcmlokggpcgncalophomjeencbbli/" ]; then
    owned_native_manifest=1
  else
    printf 'Leaving unowned Native Messaging manifest untouched: %s\n' "$native_manifest" >&2
  fi
fi

# Validate and stop the live job before deleting its plist or executable. If the
# plist was manually removed, the running job must still point at our owned
# browserd path before the fixed label is touched.
if [ "$skip_launchd" != "1" ] && command -v launchctl >/dev/null 2>&1 && \
    { [ "$owned_launch_agent" = "1" ] || [ "$owned_install" = "1" ]; }; then
  launch_domain="gui/$(id -u)"
  job_state=$(mktemp -t browser-control-launchd.XXXXXX)
  if launchctl print "$launch_domain/$launch_label" >"$job_state" 2>/dev/null; then
    if ! grep -Fq "$app_dir/bin/browserd" "$job_state"; then
      rm -f "$job_state"
      fail "refusing to stop $launch_label because its live program is not project-owned"
    fi
    if ! launchctl bootout "$launch_domain/$launch_label" >/dev/null 2>&1; then
      rm -f "$job_state"
      fail "could not stop $launch_label; no files were removed"
    fi
    stop_attempt=0
    while launchctl print "$launch_domain/$launch_label" >/dev/null 2>&1 && [ "$stop_attempt" -lt 50 ]; do
      sleep 0.1
      stop_attempt=$((stop_attempt + 1))
    done
    if launchctl print "$launch_domain/$launch_label" >/dev/null 2>&1; then
      rm -f "$job_state"
      fail "$launch_label is still running; no files were removed"
    fi
  fi
  rm -f "$job_state"
fi

if [ "$owned_launch_agent" = "1" ]; then
  rm "$launch_agent"
fi
if [ "$owned_native_manifest" = "1" ]; then
  rm "$native_manifest"
fi
if [ "$owned_install" = "1" ]; then
  rm -rf "$app_dir"
fi

printf '%s\n' "browser-control project-owned files were removed."
printf '%s\n' "If the unpacked extension is still listed in chrome://extensions, remove it there manually."
