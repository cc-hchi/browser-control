#!/bin/sh

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
corepack_home=${COREPACK_HOME:-"$HOME/Library/Caches/node/corepack"}
pnpm_store_dir=$(sed -n 's/^storeDir: //p' node_modules/.modules.yaml 2>/dev/null | head -n 1)
if [ -z "$pnpm_store_dir" ]; then
  pnpm_store_dir=$(pnpm store path)
fi

node scripts/check-protocol.mjs
scripts/check-packaging.sh

fixture_log=$(mktemp -t browser-control-fixture.XXXXXX)
fixture_port=$(node scripts/find-port-pair.mjs)
FIXTURE_PORT="$fixture_port" node tests/fixtures/server.mjs >"$fixture_log" 2>&1 &
fixture_pid=$!
cleanup_fixture() {
  kill "$fixture_pid" 2>/dev/null || true
  wait "$fixture_pid" 2>/dev/null || true
  rm -f "$fixture_log"
}
trap cleanup_fixture EXIT INT TERM
fixture_ready=false
for _ in 1 2 3 4 5; do
  if ! kill -0 "$fixture_pid" 2>/dev/null; then
    break
  fi
  if curl -fsS "http://127.0.0.1:$fixture_port/" | grep -Fq "Browser control fixture" && \
      curl -fsS "http://127.0.0.1:$((fixture_port + 1))/frame/2" | grep -Fq "Frame fixture"; then
    fixture_ready=true
    break
  fi
  sleep 0.2
done
if [ "$fixture_ready" != true ]; then
  cat "$fixture_log" >&2
  exit 1
fi
cleanup_fixture
trap - EXIT INT TERM

go test ./...
go test -race ./...
go vet ./...
go build ./...
pnpm check
pnpm test
pnpm build

skill_validator=${SKILL_VALIDATOR:-"${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py"}
[ -f "$skill_validator" ] || {
  printf 'skill validator not found; set SKILL_VALIDATOR: %s\n' "$skill_validator" >&2
  exit 1
}
python3 "$skill_validator" skills/control-local-chrome
sh -n skills/control-local-chrome/scripts/browserctl

# Unix-domain socket paths are short on macOS; keep the isolated HOME under the
# compact /tmp spelling instead of the much longer per-user TMPDIR path.
install_home=$(mktemp -d "/tmp/bc-accept.XXXXXX")
install_log=$(mktemp -t browser-control-install.XXXXXX)
cleanup_install() {
  if [ -n "${installed_daemon_pid:-}" ]; then
    kill "$installed_daemon_pid" 2>/dev/null || true
    wait "$installed_daemon_pid" 2>/dev/null || true
  fi
  rm -rf "$install_home"
  rm -f "$install_log"
}
trap cleanup_install EXIT INT TERM
mkdir -p "$install_home/.codex/skills"
touch "$install_home/.codex/skills/runtime-must-not-touch"
HOME="$install_home" COREPACK_HOME="$corepack_home" \
  CI=1 npm_config_store_dir="$pnpm_store_dir" BROWSER_CONTROL_SKIP_LAUNCHD=1 \
  scripts/install.sh >"$install_log"
installed_root="$install_home/Library/Application Support/browser-control"
[ -x "$installed_root/bin/browserd" ]
[ -x "$installed_root/bin/browser-native-host" ]
[ -x "$installed_root/bin/browserctl" ]
[ -x "$installed_root/support/uninstall.sh" ]
[ -f "$installed_root/extension/manifest.json" ]
[ ! -e "$installed_root/skills" ]
[ ! -e "$install_home/.codex/skills/control-local-chrome" ]
[ -f "$install_home/.codex/skills/runtime-must-not-touch" ]

HOME="$install_home" CODEX_HOME="$install_home/.codex" scripts/install-skill.sh >/dev/null
installed_skill="$install_home/.codex/skills/control-local-chrome"
[ -d "$installed_skill" ]
[ ! -L "$installed_skill" ]
[ -f "$installed_skill/.browser-control-skill-install" ]
[ -x "$installed_skill/scripts/browserctl" ]
python3 "$skill_validator" "$installed_skill"
HOME="$install_home" "$installed_root/bin/browserd" >"$installed_root/logs/acceptance.stdout.log" 2>"$installed_root/logs/acceptance.stderr.log" &
installed_daemon_pid=$!
daemon_ready=false
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if [ -S "$installed_root/browserd.sock" ]; then
    daemon_ready=true
    break
  fi
  sleep 0.1
done
if [ "$daemon_ready" != true ]; then
  cat "$installed_root/logs/acceptance.stderr.log" >&2
  exit 1
fi
HOME="$install_home" "$installed_root/bin/browserctl" --json rpc daemon.diagnostics --params '{}' >/dev/null
if [ "${BROWSER_CONTROL_CHROME_E2E:-0}" = "1" ]; then
  BROWSER_CONTROL_INSTALL_ROOT="$installed_root" scripts/run-chrome-e2e.sh
fi
kill "$installed_daemon_pid"
wait "$installed_daemon_pid" || true
installed_daemon_pid=
HOME="$install_home" BROWSER_CONTROL_SKIP_LAUNCHD=1 \
  "$installed_root/support/uninstall.sh" >/dev/null
[ ! -e "$installed_root" ]
[ -d "$installed_skill" ]
[ -f "$install_home/.codex/skills/runtime-must-not-touch" ]
HOME="$install_home" CODEX_HOME="$install_home/.codex" scripts/uninstall-skill.sh >/dev/null
[ ! -e "$installed_skill" ]
[ -f "$install_home/.codex/skills/runtime-must-not-touch" ]
cleanup_install
trap - EXIT INT TERM
git diff --check
