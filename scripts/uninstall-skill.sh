#!/bin/sh

set -eu
umask 077

fail() {
  printf 'browser-control skill uninstall: %s\n' "$*" >&2
  exit 1
}

ensure_absolute() {
  case "$2" in
    /*) ;;
    *) fail "$1 must be an absolute path: $2" ;;
  esac
}

[ -n "${HOME:-}" ] || fail "HOME is not set"
ensure_absolute HOME "$HOME"

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
owner_source="$repo_dir/packaging/SKILL_INSTALL_OWNER"
runtime_owner_source="$repo_dir/packaging/INSTALL_OWNER"
source_skill="$repo_dir/skills/control-local-chrome"
codex_home=${CODEX_HOME:-"$HOME/.codex"}
ensure_absolute CODEX_HOME "$codex_home"
destination="$codex_home/skills/control-local-chrome"
marker="$destination/.browser-control-skill-install"
runtime_root="$HOME/Library/Application Support/browser-control"
legacy_runtime_skill="$runtime_root/skills/control-local-chrome"
runtime_owner_file="$runtime_root/.browser-control-install"

[ -f "$owner_source" ] || fail "skill ownership metadata not found: $owner_source"
owner_value=$(tr -d '\r\n' <"$owner_source")
[ -n "$owner_value" ] || fail "skill ownership marker is empty"

removed_legacy_link=0
if [ -L "$destination" ]; then
  existing_target=$(readlink "$destination")
  if [ "$existing_target" != "$legacy_runtime_skill" ] && [ "$existing_target" != "$source_skill" ]; then
    fail "refusing to remove an unowned skill symlink: $destination"
  fi
  if [ "$existing_target" = "$legacy_runtime_skill" ]; then
    removed_legacy_link=1
  fi
  rm "$destination"
elif [ -e "$destination" ]; then
  [ ! -L "$marker" ] || fail "refusing a symlinked ownership marker: $marker"
  [ -f "$marker" ] || fail "refusing to remove an unowned skill: $destination"
  installed_owner=$(tr -d '\r\n' <"$marker")
  [ "$installed_owner" = "$owner_value" ] || fail "skill ownership marker is not recognized: $marker"
  rm -rf "$destination"
else
  printf '%s\n' "control-local-chrome skill is not installed at $destination."
  exit 0
fi

# Older releases stored the skill payload inside the runtime and linked Codex to
# it. Remove that payload only when this command just removed the exact legacy
# link and the runtime still carries this project's ownership marker.
if [ "$removed_legacy_link" = "1" ] && [ -L "$runtime_root" ]; then
  printf 'Leaving legacy skill payload behind a symlinked runtime root untouched: %s\n' "$runtime_root" >&2
elif [ "$removed_legacy_link" = "1" ] && [ -f "$runtime_owner_source" ] && \
    [ -f "$runtime_owner_file" ] && [ ! -L "$runtime_owner_file" ]; then
  expected_runtime_owner=$(tr -d '\r\n' <"$runtime_owner_source")
  installed_runtime_owner=$(tr -d '\r\n' <"$runtime_owner_file")
  if [ -n "$expected_runtime_owner" ] && [ "$installed_runtime_owner" = "$expected_runtime_owner" ]; then
    if [ -L "$legacy_runtime_skill" ]; then
      printf 'Leaving symlinked legacy skill payload untouched: %s\n' "$legacy_runtime_skill" >&2
    elif [ -d "$legacy_runtime_skill" ]; then
      rm -rf "$legacy_runtime_skill"
      rmdir "$(dirname -- "$legacy_runtime_skill")" 2>/dev/null || :
    fi
  fi
fi

printf '%s\n' "control-local-chrome skill was removed from $destination."
printf '%s\n' "The browser-control runtime was left untouched."
