#!/bin/bash
set -euo pipefail
umask 077

usage() {
    cat <<'HELP'
用法：install.sh --enhanced --core <已构建sing-box> --helper <已构建sbe> [选项]
  --root <路径>       增强版数据目录，默认 /etc/sing-box-enhanced
  --core <路径>       含 with_v2ray_api 的本地 sing-box 可执行文件
  --helper <路径>     本地 Go 管理程序可执行文件
  --entry <路径>      命令入口，默认 /usr/local/bin/sb
  --unit-dir <路径>   systemd unit 目录，默认 /etc/systemd/system
  --no-start          仅安装，不启动计量守护进程
  -h, --help          显示帮助
安装只使用本地产物，不安装工具链、不下载核心、不创建 Linux 账户。
已有原 sb 时请使用 --entry /usr/local/bin/sb-enhanced；迁移需单独执行。
HELP
}

fail() {
    printf '安装失败：%s\n' "$*" >&2
    exit 1
}

root=/etc/sing-box-enhanced
core_source=
helper_source=
entry=/usr/local/bin/sb
unit_dir=/etc/systemd/system
no_start=0
while [[ $# -gt 0 ]]; do
    case $1 in
    --root | --core | --helper | --entry | --unit-dir)
        [[ $# -ge 2 && -n $2 ]] || fail "$1 缺少参数"
        case $1 in
        --root) root=$2 ;;
        --core) core_source=$2 ;;
        --helper) helper_source=$2 ;;
        --entry) entry=$2 ;;
        --unit-dir) unit_dir=$2 ;;
        esac
        shift 2
        ;;
    --no-start) no_start=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) fail "未知参数：$1" ;;
    esac
done

[[ $EUID == 0 ]] || fail '请使用 root 执行；脚本不会创建其他账户'
[[ -n $core_source && -n $helper_source ]] || fail '必须提供 --core 和 --helper 本地产物'
[[ -f $core_source && -x $core_source ]] || fail 'sing-box 产物不存在或不可执行'
[[ -f $helper_source && -x $helper_source ]] || fail 'sbe 产物不存在或不可执行'
for install_path in "$root" "$entry" "$unit_dir"; do
    [[ $install_path == /* && $install_path != / && $install_path =~ ^/[a-zA-Z0-9._/-]+$ ]] ||
        fail '安装路径须为无空格的绝对路径，只包含字母、数字、点、斜杠、下划线和连字符'
    [[ $install_path != */../* && $install_path != */.. && $install_path != */./* ]] ||
        fail '安装路径不能包含 . 或 .. 路径段'
done
root=${root%/}
unit_dir=${unit_dir%/}
if [[ -e $root || -L $root ]]; then
    [[ -d $root && ! -L $root ]] || fail '数据目录不能是文件或符号链接'
    shopt -s nullglob dotglob
    root_contents=("$root"/*)
    shopt -u nullglob dotglob
    [[ ${#root_contents[@]} == 0 ]] || fail '目标数据目录已有内容，未修改；请使用独立空目录'
fi
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
[[ -f $script_root/src/enhanced.sh && -f $script_root/scripts/sb-enhanced.sh && -f $script_root/scripts/install-warp.sh ]] || fail '增强脚本不完整'
[[ ! -e $entry && ! -L $entry ]] || fail "入口已存在，未覆盖：$entry；请指定独立 --entry"
[[ ! -e $root/managed.marker && ! -e $root/bin/sbe && ! -e $root/bin/sing-box ]] ||
    fail '目标目录已有增强安装；请使用受管升级流程，避免替换运行中的核心'
unit=$unit_dir/sing-box-enhanced-meter.service
[[ ! -e $unit && ! -L $unit ]] || fail "systemd unit 已存在，未覆盖：$unit"
if [[ $no_start == 0 ]]; then
    command -v systemctl >/dev/null || fail '当前系统缺少 systemd；可使用 --no-start 仅安装'
    [[ -d /run/systemd/system ]] || fail 'systemd 未运行；可使用 --no-start 仅安装'
fi

install -d -m 0700 "$root" "$root/bin" "$root/sh" "$root/sh/src" "$root/sh/scripts" \
    "$root/instances" "$root/backups"
install -m 0700 "$core_source" "$root/bin/sing-box"
install -m 0700 "$helper_source" "$root/bin/sbe"
install -m 0600 "$script_root/src/enhanced.sh" "$root/sh/src/enhanced.sh"
install -m 0700 "$script_root/scripts/sb-enhanced.sh" "$root/sh/scripts/sb-enhanced.sh"
install -m 0700 "$script_root/scripts/install-warp.sh" "$root/sh/scripts/install-warp.sh"
mkdir -p -- "$(dirname -- "$entry")" "$unit_dir"

entry_tmp=$(mktemp "$(dirname -- "$entry")/.sbe-entry.XXXXXX")
unit_tmp=$(mktemp "$unit_dir/.sbe-meter.XXXXXX")
trap 'rm -f "$entry_tmp" "$unit_tmp"' EXIT
{
    printf '%s\n' '#!/bin/bash' '# sing-box-enhanced: local Go entry; original author 233Boy'
    printf 'export SBE_ROOT=%q\n' "$root"
    printf 'export SBE_BIN=%q\n' "$root/bin/sbe"
    printf 'export SBE_CORE=%q\n' "$root/bin/sing-box"
    printf 'export SBE_SH_ROOT=%q\n' "$root/sh"
    printf 'export SBE_UNIT_DIR=%q\n' "$unit_dir"
    printf 'exec bash %q "$@"\n' "$root/sh/scripts/sb-enhanced.sh"
} >"$entry_tmp"
chmod 0700 "$entry_tmp"

cat >"$unit_tmp" <<UNIT
[Unit]
Description=sing-box Enhanced local quota meter
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
UMask=0077
ExecStart=$root/bin/sbe --root $root --core $root/bin/sing-box --unit-dir $unit_dir daemon
ExecStopPost=$root/bin/sbe --root $root --core $root/bin/sing-box --unit-dir $unit_dir service protect
Restart=on-failure
RestartSec=2
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
UNIT
chmod 0600 "$unit_tmp"
mv -- "$entry_tmp" "$entry"
mv -- "$unit_tmp" "$unit"
printf '%s\n' 'managed-v1' >"$root/managed.marker"
chmod 0600 "$root/managed.marker"

if [[ $no_start == 0 ]]; then
    systemctl daemon-reload
    systemctl enable --now sing-box-enhanced-meter.service
fi
printf '增强版已安装：%s\n' "$entry"
printf '%s\n' '未生成默认节点，未迁移或重启原 sing-box；请先创建账号，再绑定受管节点。'
if [[ $no_start == 1 ]]; then
    printf '%s\n' '计量守护进程尚未启动，使用受管节点前必须启动它。'
fi
