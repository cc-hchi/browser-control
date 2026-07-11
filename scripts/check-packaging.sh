#!/bin/sh

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

sh -n scripts/install.sh
sh -n scripts/uninstall.sh
sh -n scripts/install-skill.sh
sh -n scripts/uninstall-skill.sh
node --check scripts/extension-id.mjs
node --check scripts/render-native-manifest.mjs
node --check packaging/render-launchd.mjs

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/browser-control-packaging.XXXXXX")
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT HUP INT TERM

install_root="$work_dir/Application Support/browser-control"
upload_roots="$work_dir/Uploads & one:$work_dir/Uploads two"
mkdir -p "$install_root/bin" "$work_dir/output"
touch "$install_root/bin/browser-native-host"

node scripts/render-native-manifest.mjs \
  --extension-manifest "$repo_dir/apps/extension/manifest.json" \
  --host-path "$install_root/bin/browser-native-host" \
  --output "$work_dir/output/com.browser_control.native_host.json"
node scripts/render-native-manifest.mjs \
  --check-owned "$work_dir/output/com.browser_control.native_host.json" \
  --install-root "$install_root"

extension_id=$(node scripts/extension-id.mjs "$repo_dir/apps/extension/manifest.json")
actual_origin=$(node -e '
  const fs = require("node:fs");
  const manifest = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  process.stdout.write(manifest.allowed_origins?.[0] ?? "");
' "$work_dir/output/com.browser_control.native_host.json")
[ "$actual_origin" = "chrome-extension://$extension_id/" ] || {
  printf 'Native Messaging origin does not match extension ID: %s\n' "$actual_origin" >&2
  exit 1
}

node packaging/render-launchd.mjs \
  --template "$repo_dir/packaging/launchd/com.browser-control.browserd.plist.in" \
  --output "$work_dir/output/com.browser-control.browserd.plist" \
  --label com.browser-control.browserd \
  --browserd "$install_root/bin/browserd" \
  --state-dir "$install_root" \
  --socket "$install_root/browserd.sock" \
  --bridge-socket "$install_root/bridge.sock" \
  --bridge-token-file "$install_root/bridge.token" \
  --artifact-dir "$install_root/artifacts" \
  --upload-roots "$upload_roots" \
  --home "$work_dir/home & user" \
  --stdout-log "$install_root/logs/browserd.stdout.log" \
  --stderr-log "$install_root/logs/browserd.stderr.log"

if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$work_dir/output/com.browser-control.browserd.plist" >/dev/null
fi
upload_flag_count=$(grep -c '<string>--upload-root</string>' "$work_dir/output/com.browser-control.browserd.plist")
[ "$upload_flag_count" -eq 2 ] || {
  printf 'LaunchAgent has %s upload roots, expected 2\n' "$upload_flag_count" >&2
  exit 1
}
grep -Fq '<string>'"$work_dir"'/Uploads &amp; one</string>' "$work_dir/output/com.browser-control.browserd.plist"
grep -Fq '<string>'"$work_dir"'/Uploads two</string>' "$work_dir/output/com.browser-control.browserd.plist"
grep -Fq '<string>'"$install_root"'/bridge.token</string>' "$work_dir/output/com.browser-control.browserd.plist"

# Exercise uninstall ownership against an isolated HOME. This must remove all
# project-owned integration files without touching the real user environment.
test_home="$work_dir/home"
test_app="$test_home/Library/Application Support/browser-control"
test_native="$test_home/Library/Application Support/Google/Chrome/NativeMessagingHosts/com.browser_control.native_host.json"
test_agent="$test_home/Library/LaunchAgents/com.browser-control.browserd.plist"
test_codex="$test_home/.codex"
mkdir -p "$test_app/bin" "$(dirname -- "$test_native")" \
  "$(dirname -- "$test_agent")" "$test_codex/skills/control-local-chrome"
cp packaging/INSTALL_OWNER "$test_app/.browser-control-install"
touch "$test_app/bin/browser-native-host"
node scripts/render-native-manifest.mjs \
  --extension-manifest "$repo_dir/apps/extension/manifest.json" \
  --host-path "$test_app/bin/browser-native-host" \
  --output "$test_native"
node packaging/render-launchd.mjs \
  --template "$repo_dir/packaging/launchd/com.browser-control.browserd.plist.in" \
  --output "$test_agent" \
  --label com.browser-control.browserd \
  --browserd "$test_app/bin/browserd" \
  --state-dir "$test_app" \
  --socket "$test_app/browserd.sock" \
  --bridge-socket "$test_app/bridge.sock" \
  --bridge-token-file "$test_app/bridge.token" \
  --artifact-dir "$test_app/artifacts" \
  --upload-roots "$test_home/Downloads:$test_home/Desktop" \
  --home "$test_home" \
  --stdout-log "$test_app/logs/browserd.stdout.log" \
  --stderr-log "$test_app/logs/browserd.stderr.log"
touch "$test_home/unrelated-sentinel"
touch "$test_codex/skills/control-local-chrome/runtime-uninstall-must-not-touch"
HOME="$test_home" PATH=/usr/bin:/bin BROWSER_CONTROL_SKIP_LAUNCHD=1 \
  scripts/uninstall.sh >/dev/null
[ ! -e "$test_app" ]
[ ! -e "$test_native" ]
[ ! -e "$test_agent" ]
[ -f "$test_codex/skills/control-local-chrome/runtime-uninstall-must-not-touch" ]
[ -f "$test_home/unrelated-sentinel" ]

# Skill installation is an independent, owned copy. It must refuse unrelated
# content, survive runtime removal, and be removable without touching runtime
# state or neighboring Codex skills.
skill_log="$work_dir/skill-install.log"
if HOME="$test_home" CODEX_HOME="$test_codex" scripts/install-skill.sh >"$skill_log" 2>&1; then
  printf '%s\n' "Skill installer overwrote an unowned skill" >&2
  exit 1
fi
grep -Fq "refusing to replace an unowned skill" "$skill_log"
[ -f "$test_codex/skills/control-local-chrome/runtime-uninstall-must-not-touch" ]
rm -rf "$test_codex/skills/control-local-chrome"

HOME="$test_home" CODEX_HOME="$test_codex" scripts/install-skill.sh >/dev/null
installed_skill="$test_codex/skills/control-local-chrome"
[ -d "$installed_skill" ]
[ ! -L "$installed_skill" ]
[ -f "$installed_skill/SKILL.md" ]
[ -x "$installed_skill/scripts/browserctl" ]
cmp packaging/SKILL_INSTALL_OWNER "$installed_skill/.browser-control-skill-install"
HOME="$test_home" CODEX_HOME="$test_codex" scripts/install-skill.sh >/dev/null
HOME="$test_home" CODEX_HOME="$test_codex" scripts/uninstall-skill.sh >/dev/null
[ ! -e "$installed_skill" ]
[ -f "$test_home/unrelated-sentinel" ]

# Remove the exact symlink/payload shape created by older coupled installers,
# while preserving the owned runtime itself.
mkdir -p "$test_app/skills/control-local-chrome" "$test_codex/skills"
cp packaging/INSTALL_OWNER "$test_app/.browser-control-install"
ln -s "$test_app/skills/control-local-chrome" "$installed_skill"
HOME="$test_home" CODEX_HOME="$test_codex" scripts/uninstall-skill.sh >/dev/null
[ ! -L "$installed_skill" ]
[ ! -e "$test_app/skills" ]
[ -f "$test_app/.browser-control-install" ]

# A legacy runtime root symlink must never be followed while cleaning its old
# Skill payload. Removing the exact Codex link is safe; external data remains.
external_runtime="$work_dir/external-runtime"
rm -rf "$test_app"
mkdir -p "$external_runtime/skills/control-local-chrome"
cp packaging/INSTALL_OWNER "$external_runtime/.browser-control-install"
touch "$external_runtime/skills/control-local-chrome/external-sentinel"
ln -s "$external_runtime" "$test_app"
ln -s "$test_app/skills/control-local-chrome" "$installed_skill"
HOME="$test_home" CODEX_HOME="$test_codex" scripts/uninstall-skill.sh >/dev/null 2>&1
[ ! -L "$installed_skill" ]
[ -f "$external_runtime/skills/control-local-chrome/external-sentinel" ]

printf 'packaging checks passed (extension ID: %s)\n' "$extension_id"
