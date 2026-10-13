#!/bin/bash

script_root=${SBE_SH_ROOT:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)}
if [[ ! -f $script_root/src/enhanced.sh ]]; then
    printf '%s\n' '增强管理入口缺失，停止执行。' >&2
    exit 1
fi
. "$script_root/src/enhanced.sh"
if sbe_dispatch "$@"; then
    exit 0
fi
printf '%s\n' '这是增强管理入口，请使用 sb enhanced help 查看命令。' >&2
exit 1
