#!/bin/bash

# Dispatch enhanced commands before the upstream initializer can touch a config.
_sbe_script=$(readlink -f "${BASH_SOURCE[0]}" 2>/dev/null) || _sbe_script=${BASH_SOURCE[0]}
_sbe_sh_root=${SBE_SH_ROOT:-$(cd -- "$(dirname -- "$_sbe_script")" && pwd)}
if [[ -f $_sbe_sh_root/src/enhanced.sh ]]; then
    . "$_sbe_sh_root/src/enhanced.sh"
    if sbe_dispatch "$@"; then
        exit 0
    fi
elif [[ -f ${SBE_ROOT:-/etc/sing-box-enhanced}/managed.marker ]]; then
    printf '%s\n' '增强管理入口缺失，已拒绝执行原脚本以保护账号配额。' >&2
    exit 1
fi

args=$@
is_sh_ver=v1.18

. /etc/sing-box/sh/src/init.sh
