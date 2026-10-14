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
if [[ ${SBE_TEST_FAIL_COMMAND:-} == "${1:-} ${2:-}" ]]; then
    printf '%s\n' '{"error":"fixture failure"}' >&2
    exit 7
fi
if [[ ${1:-} == view ]]; then
    operation=${2:-}; shift 2
    case $operation in
    choices)
        kind=
        while [[ $# -gt 0 ]]; do
            case $1 in --kind) kind=$2; shift 2;; *) shift;; esac
        done
        case $kind in
        node) printf 'node-stable-1\t旧 Hysteria2\t运行中\nnode-stable-2\t第二节点\t已停止\n' ;;
        account)
            printf 'account-stable-1\t普通账号\t允许访问\n'
            [[ ${SBE_TEST_SINGLE_BINDING:-0} == 1 ]] || printf 'account-stable-2\t备用账号\t手动停用\n'
            ;;
        domain) printf 'example.com\texample.com\t走 WARP\n' ;;
        esac
        ;;
    quota-bytes)
        if [[ ${2:-} =~ ^[0-9]+$ ]]; then printf '%s\n' "$((10#${2:-0} * 1000000000))"
        elif [[ ${2:-} == 0.5 ]]; then printf '%s\n' 500000000
        else exit 1; fi
        ;;
    node) printf '%s\n' '节点名称：旧 Hysteria2' '认证凭据：已隐藏' ;;
    account) printf '%s\n' '账号名称：普通账号' '当月已用：1 GB' '账号状态：手动停用' ;;
    endpoint) printf '%s\n' '协议：Hysteria2' '协议密码：fixture-protocol-password' ;;
    url) printf '%s\n' 'hysteria2://fixture-private-link' ;;
    qr) printf '%s\n' 'fixture-QR' ;;
    admin) printf '%s\n' '管理员名称：admin' '登录密码：已隐藏' ;;
    services) printf '%s\n' '流量计量服务：运行中' ;;
    warp) printf '%s\n' 'WARP 客户端：已安装' ;;
    rules) printf '%s\n' '匹配方式：精确域名匹配' ;;
    esac
    exit 0
fi
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
printf '%s\n' '{"result":"business-json"}'
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

# Nested pages remain in the chosen module, use stable IDs, and never print
# mutation JSON or credentials before an explicit detail action.
rm -f "$fixture/history"
run_menu 1 1 0 0 0
[[ $(cat "$fixture/history") == *$'<view>\n<node>\n<--node>\n<node-stable-1>'* ]]
[[ $(cat "$fixture/menu") != *'business-json'* && $(cat "$fixture/menu") != *'fixture-protocol-password'* ]]
[[ $(LC_ALL=C awk '/sing-box Enhanced > 主菜单/{n++}END{print n+0}' "$fixture/menu") == 2 ]]
[[ $(LC_ALL=C awk '/sing-box Enhanced > 查询节点/{n++}END{print n+0}' "$fixture/menu") == 3 ]]
rm -f "$fixture/history"
run_menu 1 1 1 2 '' 0 0 0
[[ $(cat "$fixture/history") == *$'<view>\n<endpoint>\n<--node>\n<node-stable-1>\n<--account>\n<account-stable-2>'* ]]
[[ $(cat "$fixture/menu") == *'fixture-protocol-password'* ]]
# A single bound account goes straight to its endpoint; no extra account prompt.
rm -f "$fixture/history"
export SBE_TEST_SINGLE_BINDING=1
run_menu 1 1 1 '' 0 0 0
unset SBE_TEST_SINGLE_BINDING
[[ $(cat "$fixture/history") == *$'<view>\n<endpoint>\n<--node>\n<node-stable-1>\n<--account>\n<account-stable-1>'* ]]
[[ $(cat "$fixture/menu") != *'sing-box Enhanced > 查询节点 > 旧 Hysteria2 > 选择已绑定账号'* ]]
for item in '2 url' '3 qr'; do
    read -r choice operation <<<"$item"
    rm -f "$fixture/history"
    run_menu 1 2 "$choice" 1 '[::1]:443' '' 0 0 0
    [[ $(cat "$fixture/history") == *"<view>"$'\n'"<$operation>"$'\n<--node>\n<node-stable-2>\n<--account>\n<account-stable-1>\n<--server>\n<[::1]:443>'* ]]
done
rm -f "$fixture/history"
run_menu 2 3 'menu name' 1 300 1 '' 0 0
[[ $(cat "$fixture/history") == *$'<account>\n<create>\n<--name>\n<menu name>\n<--bytes>\n<300000000000>'* ]]
[[ $(cat "$fixture/menu") != *'business-json'* ]]
for amount in 0 0.5; do
    rm -f "$fixture/history"
    run_menu 2 1 3 1 "$amount" 1 '' 0 0 0
    [[ $(cat "$fixture/history") == *$'<account>\n<quota>\n<--id>\n<account-stable-1>\n<--bytes>'* ]]
    [[ $amount != 0 || $(cat "$fixture/history") == *$'<--bytes>\n<0>'* ]]
    [[ $amount != 0.5 || $(cat "$fixture/history") == *$'<--bytes>\n<500000000>'* ]]
done
rm -f "$fixture/history"
run_menu 2 1 3 2 1 '' 0 0 0
[[ $(cat "$fixture/history") == *$'<account>\n<quota>\n<--id>\n<account-stable-1>\n<--unlimited>'* ]]
for item in '1 enable' '2 disable' '4 reset' '5 recover'; do
    read -r choice operation <<<"$item"
    rm -f "$fixture/history"
    run_menu 2 1 "$choice" 1 '' 0 0 0
    [[ $(cat "$fixture/history") == *"<account>"$'\n'"<$operation>"$'\n<--id>\n<account-stable-1>'* ]]
done
[[ $(cat "$fixture/menu") == *'计量故障恢复操作已完成'* ]]
run_menu 2 1 4 1 '' 0 0 0
[[ $(cat "$fixture/menu") == *'手动停用和计量故障不会因此解除'* ]]
# Blank input and unconfirmed actions must not mutate anything.
for inputs in '2 1 2 0 0 0 0' '5 2 0 0 0'; do
    rm -f "$fixture/history"
    read -r -a sequence <<<"$inputs"
    run_menu "${sequence[@]}"
    [[ $(cat "$fixture/history") != *$'<account>\n<disable>'* && $(cat "$fixture/history") != *$'<service>\n<stop>'* ]]
done
rm -f "$fixture/history"
run_menu 2 1 3 1 '' 0 0 0
[[ $(cat "$fixture/history") != *$'<account>\n<quota>'* ]]
rm -f "$fixture/history"
run_menu 2 3 invalid-amount 1 not-a-number 0 0 0
[[ $(cat "$fixture/history") != *$'<account>\n<create>'* ]]
# No evaluation of user-supplied account names.
printf -v literal 'name; $(touch "%s/sentinel")' "$fixture"
rm -f "$fixture/history"
run_menu 2 3 "$literal" 2 1 '' 0 0
[[ $(cat "$fixture/history") == *"<$literal>"* && ! -e $fixture/sentinel ]]
# Failed mutations stay on their page and do not announce success or expose JSON.
rm -f "$fixture/history"
export SBE_TEST_FAIL_COMMAND='account disable'
run_menu 2 1 2 1 '' 0 0 0
unset SBE_TEST_FAIL_COMMAND
[[ $(cat "$fixture/menu") == *'操作未完成'* && $(cat "$fixture/menu") != *'账号已停用。'* && $(cat "$fixture/menu") != *'fixture failure'* ]]
[[ $(LC_ALL=C awk '/账号与流量 > 普通账号/{n++}END{print n+0}' "$fixture/menu") == 2 ]]
# Node import and binding keep filenames literal and select existing account IDs.
printf '{}\n' >"$fixture/template with spaces.json"
rm -f "$fixture/history"
run_menu 3 1 'node with spaces' "$fixture/template with spaces.json" 2 12345 1 '' 0 0
[[ $(cat "$fixture/history") == *$'<node>\n<add>\n<--name>\n<node with spaces>\n<--template>'$'\n'"<$fixture/template with spaces.json>"* ]]
[[ $(cat "$fixture/history") == *$'<node>\n<bind>\n<--node>\n<node with spaces>\n<--account>\n<account-stable-2>\n<--port>\n<12345>'* ]]
rm -f "$fixture/history"
run_menu 3 1 node "$fixture/template with spaces.json" 1 65536 '' 0 0
[[ $(cat "$fixture/history") != *$'<node>\n<add>'* ]]
# WARP rules are selected by number, and accepting terms is always explicit.
rm -f "$fixture/history"
run_menu 4 5 no 0 0
[[ ! -e $fixture/history ]]
run_menu 4 5 ACCEPT '' 0 0
[[ $(cat "$fixture/history") == *$'<warp>\n<register>\n<--accept-terms>'* ]]
rm -f "$fixture/history"
run_menu 4 2 1 1 1 '' 0 0 0
[[ $(cat "$fixture/history") == *$'<warp>\n<remove>\n<--domain>\n<example.com>'* ]]
export SBE_TEST_FAIL_COMMAND='warp remove'
run_menu 4 2 1 1 1 '' 0 0 0 0
unset SBE_TEST_FAIL_COMMAND
[[ $(LC_ALL=C awk '/WARP 管理 > 域名规则 > example.com/{n++}END{print n+0}' "$fixture/menu") == 2 ]]
[[ $(cat "$fixture/menu") == *'操作未完成'* && $(cat "$fixture/menu") != *'WARP 域名规则已删除。'* ]]
run_menu 4 3 example.org 1 '' 0 0
[[ $(cat "$fixture/history") == *$'<warp>\n<add>\n<--domain>\n<example.org>'* ]]
for item in '4 install' '6 connect' '7 disconnect'; do
    read -r choice operation <<<"$item"
    run_menu 4 "$choice" 1 '' 0 0
    [[ $(cat "$fixture/history") == *"<warp>"$'\n'"<$operation>"* ]]
done
run_menu 6 1 '' 0 0
[[ $(cat "$fixture/history") == *$'<service>\n<log>\n<--tail>\n<50>'* ]]
run_menu 7 1 'admin name with spaces' 1 '' 0 0
[[ $(cat "$fixture/history") == *$'<admin>\n<rename>\n<--name>\n<admin name with spaces>'* ]]
password='fixture-password; $(touch ignored)'
run_menu 7 2 "$password" "$password" '' 0 0
[[ $(cat "$fixture/history") == *$'<admin>\n<passwd>\n<--password-stdin>'* ]]
[[ $(cat "$fixture/stdin") == "$password" ]]
[[ $(cat "$fixture/call") != *"$password"* && $(cat "$fixture/menu") != *"$password"* ]]
rm -f "$fixture/history" "$fixture/stdin"
run_menu 7 2 first different '' 0 0
[[ $(cat "$fixture/history") != *$'<admin>\n<passwd>'* && ! -e $fixture/stdin ]]
run_menu 7 2 '' '' 0 0
[[ $(cat "$fixture/history") != *$'<admin>\n<passwd>'* && ! -e $fixture/stdin ]]
printf '%s\n' "$password" | bash "$repo/sing-box.sh" admin passwd --password-stdin >/dev/null
[[ $(cat "$fixture/stdin") == "$password" ]]
[[ $(cat "$fixture/call") == *$'<admin>\n<passwd>\n<--password-stdin>' && $(cat "$fixture/call") != *"$password"* ]]
rm -f "$fixture/stdin"
printf '%s\n' 7 2 "$password" "$password" '' 0 0 | bash -x "$repo/sing-box.sh" >"$fixture/traced-menu" 2>&1
[[ $(cat "$fixture/traced-menu") != *"$password"* ]]
# EOF at any depth exits instead of repeatedly repainting menus.
for sequence in '' 1 '1 1' '2 1 3' '4 2 1' '7 2'; do
    read -r -a items <<<"$sequence"
    if [[ -n $sequence ]]; then run_menu "${items[@]}"; else run_menu; fi
    [[ $(wc -l <"$fixture/menu") -lt 200 ]]
done
# Direct CLI commands keep the original business JSON contract.
bash "$repo/sing-box.sh" account list >"$fixture/direct-json"
[[ $(cat "$fixture/direct-json") == '{"result":"business-json"}' ]]

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
printf '%s\n' 'PASS：命令行 JSON 兼容、分层中文菜单、稳定 ID 选择、GB/零额度、多层返回/EOF、危险确认、错误停留、管理员密码隐藏、WARP 条款、迁移门禁与安装保护'
