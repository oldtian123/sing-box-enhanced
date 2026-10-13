#!/bin/bash
set -euo pipefail

# Tests only the shell routing and installation contract with fake binaries.
# These checks do not demonstrate real sing-box traffic or WARP connectivity.
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/sbe-shell-check.XXXXXX")
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/root" "$fixture/legacy/conf"
printf 'managed-v1\n' >"$fixture/root/managed.marker"
printf '{}\n' >"$fixture/legacy/config.json"
cat >"$fixture/helper" <<'HELPER'
#!/bin/bash
printf '<%s>\n' "$@" >"$SBE_TEST_LOG"
printf '<%s>\n' "$@" >>"$SBE_TEST_HISTORY"
exit "${SBE_TEST_EXIT:-0}"
HELPER
chmod +x "$fixture/helper"
export SBE_ROOT="$fixture/root" SBE_BIN="$fixture/helper" SBE_CORE="$fixture/core"
export SBE_TEST_LOG="$fixture/call" SBE_UNIT_DIR="$fixture/units"
export SBE_TEST_HISTORY="$fixture/history"

run_menu() {
    printf '%s\n' "$@" | bash "$repo/sing-box.sh" >"$fixture/menu" 2>&1
}

bash -n "$repo/sing-box.sh" "$repo/install.sh" "$repo/src/enhanced.sh" \
    "$repo/src/help.sh" "$repo/scripts/install-enhanced.sh" "$repo/scripts/sb-enhanced.sh"
bash "$repo/sing-box.sh" account create --name 'name with spaces' --bytes 1000
[[ $(cat "$fixture/call") == *'<name with spaces>'* ]]
[[ $(cat "$fixture/call") == *'<--unit-dir>'* ]]
bash "$repo/sing-box.sh" quota --id 'name with spaces' --bytes 2000
[[ $(cat "$fixture/call") == *$'<account>\n<quota>\n<--id>\n<name with spaces>\n<--bytes>\n<2000>'* ]]

bash "$repo/sing-box.sh" info
[[ $(cat "$fixture/call") == *$'<node>\n<list>' ]]
bash "$repo/sing-box.sh" i --account 'name with spaces'
[[ $(cat "$fixture/call") == *$'<node>\n<endpoints>\n<--account>\n<name with spaces>' ]]
rm -f "$fixture/call"
if bash "$repo/sing-box.sh" info old-node >/dev/null 2>&1; then
    printf '%s\n' 'FAIL：旧 info 位置参数被误当账号' >&2
    exit 1
fi
[[ ! -e $fixture/call ]]
for operation in url qr; do
    bash "$repo/sing-box.sh" "$operation" --account 'name with spaces' --server '[::1]:443' --node 'node with spaces' --insecure
    [[ $(cat "$fixture/call") == *"<node>"$'\n'"<$operation>"$'\n<--account>\n<name with spaces>\n<--server>\n<[::1]:443>\n<--node>\n<node with spaces>\n<--insecure>' ]]
done
bash "$repo/sing-box.sh" log --account 'name with spaces' --tail 12
[[ $(cat "$fixture/call") == *$'<service>\n<log>\n<--account>\n<name with spaces>\n<--tail>\n<12>' ]]
bash "$repo/sing-box.sh" service log --tail 12
[[ $(cat "$fixture/call") == *$'<service>\n<log>\n<--tail>\n<12>' ]]

run_menu 2 'menu name' unlimited 0
[[ $(cat "$fixture/call") == *$'<account>\n<create>\n<--name>\n<menu name>\n<--unlimited>' ]]
run_menu 5 'menu name' 无限 0
[[ $(cat "$fixture/call") == *$'<account>\n<quota>\n<--id>\n<menu name>\n<--unlimited>' ]]
run_menu 5 'menu name' 0 0
[[ $(cat "$fixture/call") == *$'<account>\n<quota>\n<--id>\n<menu name>\n<--bytes>\n<0>' ]]
rm -f "$fixture/call"
run_menu 2 'menu name' invalid 0
[[ ! -e $fixture/call ]]
printf -v literal 'name; $(touch "%s/sentinel")' "$fixture"
run_menu 2 "$literal" 1000 0
[[ $(cat "$fixture/call") == *"<$literal>"* && ! -e $fixture/sentinel ]]
for item in '3 enable' '4 disable' '6 reset'; do
    read -r choice operation <<<"$item"
    run_menu "$choice" 'menu name' 0
    [[ $(cat "$fixture/call") == *"<account>"$'\n'"<$operation>"$'\n<--id>\n<menu name>' ]]
done
printf '{}\n' >"$fixture/template with spaces.json"
rm -f "$fixture/history"
run_menu 8 'node with spaces' "$fixture/template with spaces.json" 'menu name' 12345 0
[[ $(cat "$fixture/history") == *$'<node>\n<add>\n<--name>\n<node with spaces>\n<--template>'$'\n'"<$fixture/template with spaces.json>"* ]]
[[ $(cat "$fixture/call") == *$'<node>\n<bind>\n<--node>\n<node with spaces>\n<--account>\n<menu name>\n<--port>\n<12345>' ]]
rm -f "$fixture/call"
run_menu 8 'node with spaces' "$fixture/template with spaces.json" 'menu name' 65536 0
[[ ! -e $fixture/call ]]
run_menu 15 no 0
[[ ! -e $fixture/call ]]
run_menu 15 ACCEPT 0
[[ $(cat "$fixture/call") == *$'<warp>\n<register>\n<--accept-terms>' ]]
for item in '12 add' '13 remove'; do
    read -r choice operation <<<"$item"
    run_menu "$choice" example.com 0
    [[ $(cat "$fixture/call") == *"<warp>"$'\n'"<$operation>"$'\n<--domain>\n<example.com>' ]]
done
for item in '14 install' '16 connect' '17 disconnect'; do
    read -r choice operation <<<"$item"
    run_menu "$choice" 0
    [[ $(cat "$fixture/call") == *"<warp>"$'\n'"<$operation>" ]]
done
run_menu 23 'menu name' '[::1]:443' 'node with spaces' 0
[[ $(cat "$fixture/call") == *$'<node>\n<url>\n<--account>\n<menu name>\n<--server>\n<[::1]:443>\n<--node>\n<node with spaces>' ]]
run_menu 24 'menu name' localhost '' 0
[[ $(cat "$fixture/call") == *$'<node>\n<qr>\n<--account>\n<menu name>\n<--server>\n<localhost>' ]]
run_menu 25 0
[[ $(cat "$fixture/call") == *$'<service>\n<log>' ]]

for blocked in add change del passwd test run bin update uninstall dns import fix-all main-extra; do
    rm -f "$fixture/call"
    if bash "$repo/sing-box.sh" "$blocked" >/dev/null 2>&1; then
        printf 'FAIL：原写命令被放行：%s\n' "$blocked" >&2
        exit 1
    fi
    [[ ! -e $fixture/call ]]
done
for operation in start stop restart r s status; do
    rm -f "$fixture/call"
    if bash "$repo/sing-box.sh" "$operation" caddy >/dev/null 2>&1; then
        printf 'FAIL：额外服务参数被放行：%s caddy\n' "$operation" >&2
        exit 1
    fi
    [[ ! -e $fixture/call ]]
done
if bash "$repo/sing-box.sh" service stop caddy >/dev/null 2>&1; then
    printf '%s\n' 'FAIL：受管服务接受会改变操作范围的额外参数' >&2
    exit 1
fi
[[ ! -e $fixture/call ]]
bash "$repo/sing-box.sh" restart
[[ $(cat "$fixture/call") == *$'<service>\n<restart>' ]]
printf '0\n' | bash "$repo/sing-box.sh" >/dev/null
bash "$repo/sing-box.sh" help >"$fixture/help"
[[ $(cat "$fixture/help") != *'sb migrate'* ]]
if bash "$repo/sing-box.sh" migrate inspect >/dev/null 2>&1; then
    printf '%s\n' 'FAIL：未启用开发者模式时允许迁移' >&2
    exit 1
fi
export SBE_DEVELOPER=1
bash "$repo/sing-box.sh" migrate inspect --legacy-root "$fixture/legacy"
[[ $(cat "$fixture/call") == *'<inspect>'* ]]
unset SBE_DEVELOPER

export SBE_TEST_EXIT=7
set +e
bash "$repo/sing-box.sh" account list >/dev/null 2>&1
actual=$?
set -e
[[ $actual == 7 ]]
unset SBE_TEST_EXIT
bash "$repo/scripts/sb-enhanced.sh" node list
[[ $(cat "$fixture/call") == *'<node>'* ]]
rm "$fixture/root/managed.marker"
bash "$repo/sing-box.sh" account list
[[ $(cat "$fixture/call") == *'<account>'* ]]

if [[ $EUID == 0 ]]; then
    bash "$repo/install.sh" --enhanced --root "$fixture/installed" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/entry" \
        --unit-dir "$fixture/install-units" --no-start >/dev/null
    [[ -f $fixture/installed/sh/scripts/install-warp.sh ]]
    [[ -f $fixture/installed/managed.marker ]]
    [[ $(cat "$fixture/install-units/sing-box-enhanced-meter.service") == *'service protect'* ]]
    bash "$fixture/entry" node list
    [[ $(cat "$fixture/call") == *'<node>'* ]]
    if bash "$repo/install.sh" --enhanced --root "$fixture/other" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/entry" \
        --unit-dir "$fixture/other-units" --no-start >/dev/null 2>&1; then
        printf '%s\n' 'FAIL：安装器覆盖已有入口' >&2
        exit 1
    fi
    [[ ! -e $fixture/other ]]
else
    if bash "$repo/install.sh" --enhanced --root "$fixture/installed" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/entry" \
        --unit-dir "$fixture/install-units" --no-start >/dev/null 2>&1; then
        printf '%s\n' 'FAIL：安装器允许非 root 安装' >&2
        exit 1
    fi
    [[ ! -e $fixture/installed ]]
fi
printf '%s\n' 'PASS：参数保留、配额/只读别名、旧写路径拦截、安全交互菜单、显式 WARP 条款、迁移门禁、错误码及安装保护'
