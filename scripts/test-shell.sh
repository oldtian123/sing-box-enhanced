#!/bin/bash
set -euo pipefail
umask 077

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
umask 077
printf '<%s>\n' "$@" >"$SBE_TEST_LOG"
printf '<%s>\n' "$@" >>"$SBE_TEST_HISTORY"
[[ ${SBE_TEST_EXIT:-0} == 0 ]] || exit "$SBE_TEST_EXIT"
helper_root=
while [[ $# -gt 0 ]]; do
    case $1 in
    --root) helper_root=$2; shift 2 ;;
    --core | --unit-dir) shift 2 ;;
    *) break ;;
    esac
done
if [[ ${1:-} == admin ]]; then
    operation=${2:-}
    case $operation in
    show)
        if [[ ${SBE_TEST_ADMIN_PRESENT:-0} == 1 || -f $helper_root/admin-fixture.marker ]]; then
            printf '%s\n' '{"id":"fixture-admin","name":"admin","unlimited":true}'
            exit 0
        fi
        exit 1
        ;;
    setup)
        [[ ${SBE_TEST_ADMIN_PRESENT:-0} == 0 && ! -f $helper_root/admin-fixture.marker ]] || exit 1
        [[ ${SBE_TEST_SETUP_FAIL:-0} == 0 ]] || exit 9
        ;;
    esac
    for argument in "$@"; do
        if [[ $argument == --password-stdin ]]; then
            IFS= read -r password || exit 1
            printf '%s' "$password" >"$SBE_TEST_STDIN_LOG"
        fi
    done
    if [[ $operation == setup ]]; then
        mkdir -p "$helper_root"
        printf '%s\n' 'fixture-admin' >"$helper_root/admin-fixture.marker"
        printf '%s\n' '{"password":"fixture-random-password"}' >"$helper_root/admin.initial-credentials.json"
        printf 'Initial credentials: %s\n' "$helper_root/admin.initial-credentials.json"
    fi
fi
exit "${SBE_TEST_EXIT:-0}"
HELPER
chmod +x "$fixture/helper"
export SBE_ROOT="$fixture/root" SBE_BIN="$fixture/helper" SBE_CORE="$fixture/core"
export SBE_TEST_LOG="$fixture/call" SBE_UNIT_DIR="$fixture/units"
export SBE_TEST_HISTORY="$fixture/history"
export SBE_TEST_STDIN_LOG="$fixture/stdin"

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
run_menu 26 0
[[ $(cat "$fixture/call") == *$'<admin>\n<show>' ]]
run_menu 27 'admin name with spaces' 0
[[ $(cat "$fixture/call") == *$'<admin>\n<rename>\n<--name>\n<admin name with spaces>' ]]
password='fixture-password; $(touch ignored)'
run_menu 28 "$password" "$password" 0
[[ $(cat "$fixture/call") == *$'<admin>\n<passwd>\n<--password-stdin>' ]]
[[ $(cat "$fixture/stdin") == "$password" ]]
[[ $(cat "$fixture/call") != *"$password"* && $(cat "$fixture/menu") != *"$password"* ]]
rm -f "$fixture/call" "$fixture/stdin"
run_menu 28 first different 0
[[ ! -e $fixture/call && ! -e $fixture/stdin ]]
run_menu 28 '' '' 0
[[ ! -e $fixture/call && ! -e $fixture/stdin ]]
printf '%s\n' "$password" | bash "$repo/sing-box.sh" admin passwd --password-stdin >/dev/null
[[ $(cat "$fixture/stdin") == "$password" ]]
[[ $(cat "$fixture/call") == *$'<admin>\n<passwd>\n<--password-stdin>' && $(cat "$fixture/call") != *"$password"* ]]
rm -f "$fixture/stdin"
printf '%s\n' 28 "$password" "$password" 0 | bash -x "$repo/sing-box.sh" >"$fixture/traced-menu" 2>&1
[[ $(cat "$fixture/traced-menu") != *"$password"* ]]

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
    rm -f "$fixture/history" "$fixture/stdin"
    bash "$repo/install.sh" --enhanced --root "$fixture/installed" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/entry" \
        --unit-dir "$fixture/install-units" --no-start >/dev/null
    [[ -f $fixture/installed/sh/scripts/install-warp.sh ]]
    [[ -f $fixture/installed/managed.marker ]]
    [[ -f $fixture/installed/admin.initial-credentials.json ]]
    [[ $(cat "$fixture/history") == *$'<admin>\n<show>'* ]]
    [[ $(cat "$fixture/history") == *$'<admin>\n<setup>\n<--name>\n<admin>'* ]]
    [[ ! -e $fixture/stdin ]]
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
    printf '%s\n' "$password" | bash "$repo/install.sh" --enhanced --root "$fixture/custom" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/custom-entry" \
        --unit-dir "$fixture/custom-units" --admin-name 'custom admin' --admin-password-stdin --no-start >"$fixture/install-output"
    [[ $(cat "$fixture/call") == *$'<admin>\n<setup>\n<--name>\n<custom admin>\n<--password-stdin>' ]]
    [[ $(cat "$fixture/stdin") == "$password" ]]
    [[ $(cat "$fixture/install-output") != *"$password"* && $(cat "$fixture/call") != *"$password"* ]]
    if SBE_TEST_ADMIN_PRESENT=1 bash "$repo/install.sh" --enhanced --root "$fixture/already-admin" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/already-entry" \
        --unit-dir "$fixture/already-units" --no-start >/dev/null 2>&1; then
        printf '%s\n' 'FAIL：安装器覆盖已有应用管理员' >&2
        exit 1
    fi
    [[ ! -e $fixture/already-admin && ! -e $fixture/already-entry ]]
    if SBE_TEST_SETUP_FAIL=1 bash "$repo/install.sh" --enhanced --root "$fixture/setup-failed" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/failed-entry" \
        --unit-dir "$fixture/failed-units" --no-start >/dev/null 2>&1; then
        printf '%s\n' 'FAIL：管理员初始化失败后继续安装' >&2
        exit 1
    fi
    [[ ! -e $fixture/failed-entry && ! -e $fixture/setup-failed/managed.marker && ! -e $fixture/failed-units/sing-box-enhanced-meter.service ]]
else
    if bash "$repo/install.sh" --enhanced --root "$fixture/installed" \
        --core "$fixture/helper" --helper "$fixture/helper" --entry "$fixture/entry" \
        --unit-dir "$fixture/install-units" --no-start >/dev/null 2>&1; then
        printf '%s\n' 'FAIL：安装器允许非 root 安装' >&2
        exit 1
    fi
    [[ ! -e $fixture/installed ]]
fi
printf '%s\n' 'PASS：参数保留、配额/只读别名、旧写路径拦截、安全交互菜单、管理员密码 stdin/隐藏输入、显式 WARP 条款、迁移门禁、错误码及安装保护'
