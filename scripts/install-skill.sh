#!/bin/sh

set -eu
umask 077

fail() {
  printf 'browser-control skill install: %s\n' "$*" >&2
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
source_skill="$repo_dir/skills/control-local-chrome"
owner_source="$repo_dir/packaging/SKILL_INSTALL_OWNER"
codex_home=${CODEX_HOME:-"$HOME/.codex"}
ensure_absolute CODEX_HOME "$codex_home"
skills_dir="$codex_home/skills"
destination="$skills_dir/control-local-chrome"
marker="$destination/.browser-control-skill-install"
legacy_runtime_skill="$HOME/Library/Application Support/browser-control/skills/control-local-chrome"

[ -d "$source_skill" ] || fail "skill source not found: $source_skill"
[ ! -L "$source_skill" ] || fail "refusing a symlinked skill source: $source_skill"
[ -f "$source_skill/SKILL.md" ] || fail "skill source is missing SKILL.md: $source_skill"
[ -f "$owner_source" ] || fail "skill ownership metadata not found: $owner_source"
owner_value=$(tr -d '\r\n' <"$owner_source")
[ -n "$owner_value" ] || fail "skill ownership marker is empty"

if [ -L "$destination" ]; then
  existing_target=$(readlink "$destination")
  if [ "$existing_target" != "$legacy_runtime_skill" ] && [ "$existing_target" != "$source_skill" ]; then
    fail "refusing to replace an unowned skill symlink: $destination"
  fi
elif [ -e "$destination" ]; then
  [ ! -L "$marker" ] || fail "refusing a symlinked ownership marker: $marker"
  [ -f "$marker" ] || fail "refusing to replace an unowned skill: $destination"
  installed_owner=$(tr -d '\r\n' <"$marker")
  [ "$installed_owner" = "$owner_value" ] || fail "skill ownership marker is not recognized: $marker"
fi

install -d -m 700 "$skills_dir"
stage_dir="$skills_dir/.control-local-chrome.install-new.$$"
new_dir="$stage_dir/control-local-chrome"
old_dir="$skills_dir/.control-local-chrome.install-old.$$"
cleanup() {
  rm -rf "$stage_dir"
  if [ -e "$old_dir" ] || [ -L "$old_dir" ]; then
    if [ ! -e "$destination" ] && [ ! -L "$destination" ]; then
      mv "$old_dir" "$destination" 2>/dev/null || :
    else
      rm -rf "$old_dir"
    fi
  fi
}
trap cleanup EXIT HUP INT TERM

rm -rf "$stage_dir" "$old_dir"
mkdir -m 700 "$stage_dir"
mkdir -m 700 "$new_dir"
cp -R "$source_skill/." "$new_dir/"
install -m 600 "$owner_source" "$new_dir/.browser-control-skill-install"
find "$new_dir" -type d -exec chmod 700 {} \;
find "$new_dir" -type f -exec chmod 600 {} \;
chmod 700 "$new_dir/scripts/browserctl"

validator=${SKILL_VALIDATOR:-}
if [ -n "$validator" ]; then
  [ -f "$validator" ] || fail "SKILL_VALIDATOR does not exist: $validator"
  command -v python3 >/dev/null 2>&1 || fail "python3 is required to run the skill validator: $validator"
  python3 "$validator" "$new_dir"
fi

if [ -L "$destination" ] || [ -e "$destination" ]; then
  mv "$destination" "$old_dir"
fi
if ! mv "$new_dir" "$destination"; then
  if [ -e "$old_dir" ]; then
    mv "$old_dir" "$destination"
  fi
  fail "could not install skill at $destination"
fi
rm -rf "$old_dir"
rm -rf "$stage_dir"

printf '%s\n' "control-local-chrome skill installed successfully."
printf '  Skill: %s\n' "$destination"
printf '%s\n' "Start a new Codex task so it can discover the skill."
printf '%s\n' "The browser-control runtime remains a separate installation."
