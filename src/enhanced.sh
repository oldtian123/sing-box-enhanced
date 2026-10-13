#!/bin/bash

sbe_root() {
    printf '%s\n' "${SBE_ROOT:-/etc/sing-box-enhanced}"
}

sbe_help() {
    cat <<'HELP'
sing-box Enhanced（本地管理，不依赖 Agent/API）
  sb admin setup [--name admin] [--password-stdin]
  sb admin show
  sb admin rename --name NAME
  sb admin passwd [--password-stdin]
  sb account create --name NAME --bytes MONTHLY_BYTES
  sb account create --name NAME --unlimited
  sb account list
  sb account enable|disable|reset|recover|delete --id NAME_OR_ID
  sb account quota --id NAME_OR_ID --bytes MONTHLY_BYTES
  sb account quota --id NAME_OR_ID --unlimited
  sb quota --id NAME_OR_ID --bytes MONTHLY_BYTES（同 account quota）
  sb node add --name NAME --template NODE_JSON
  sb node list
  sb node bind|unbind --node NAME_OR_ID --account NAME_OR_ID [--port PORT]
  sb node endpoints --account NAME_OR_ID
  sb node delete --node NAME_OR_ID
  sb info|i [--account NAME_OR_ID]       节点列表或账号实际节点配置
  sb url|qr --account NAME_OR_ID --server HOST[:PORT] [--node NAME_OR_ID] [--insecure]
  sb log [--account NAME_OR_ID] [--tail 1..10000]
  sb warp install|status|list|connect|disconnect
  sb warp register --accept-terms       显式接受 Cloudflare 条款并注册
  sb warp add|remove --domain DOMAIN
  sb service status|reconcile|start|stop|restart
  sb service log [--account NAME_OR_ID] [--tail 1..10000]
  sb enhanced                            增强管理菜单
  sb enhanced help                       显示增强帮助
账号额度按北京时间自然月重置；停用账号的全部受管入口同时停止。
无限额度使用 --unlimited；--bytes 0 表示额度为零，不能访问。
接管后原 add/change/del/test/run/bin/update/uninstall 菜单不再直接执行。
请通过 node/account/service 管理，未实现的原写操作会明确拒绝。
node add 当前使用上游脚本生成的单入站 JSON 模板（如 conf/*.json）。
新模板绑定后生成独立凭据；用 node endpoints 查看实际端口及凭据。
endpoints 含认证信息，仅供本机 root 查看。账号不是 Linux 系统用户。
url/qr 显示单节点分享链接；qr 需要已安装 qrencode，不会自动安装。
log 默认查看流量守护日志，--account 查看该账号实例日志。
admin 是应用管理员（默认无限月额度），不是 Linux root 或协议认证凭据。
管理员密码仅通过 --password-stdin 的标准输入提供，禁止作为命令参数传入。
未提供密码时生成随机密码，仅保存在 root 可读的凭据文件，命令输出文件路径。
admin setup 仅用于首次初始化；已有管理员时拒绝覆盖。当前不提供登录/API。
账号订阅、Agent/API 和 Dashboard 不包含在本阶段。
HELP
    if [[ ${SBE_DEVELOPER:-0} == 1 ]] && sbe_legacy_present; then
        printf '%s\n' \
            '  sb migrate inspect|import --legacy-root PATH [--owner ACCOUNT]' \
            '  sb migrate apply [--takeover-entry /usr/local/bin/sb] | rollback' \
            '  sb migrate run --legacy-root PATH --owner ACCOUNT [--takeover-entry PATH]（仅开发者模式）'
    fi
}

sbe_legacy_present() {
    local legacy_root=${SBE_LEGACY_ROOT:-/etc/sing-box}
    while [[ $# -gt 0 ]]; do
        if [[ $1 == --legacy-root ]]; then
            [[ $# -ge 2 && -n $2 ]] || return 1
            legacy_root=$2
            break
        fi
        shift
    done
    [[ -f $legacy_root/config.json && -d $legacy_root/conf ]] ||
        [[ -f $legacy_root/sh/sing-box.sh && -d $legacy_root/conf ]]
}

sbe_run() {
    local root helper core
    root=$(sbe_root)
    helper=${SBE_BIN:-$root/bin/sbe}
    core=${SBE_CORE:-$root/bin/sing-box}
    if [[ ! -x $helper ]]; then
        printf '本地 Go 管理程序不可执行：%s\n' "$helper" >&2
        return 1
    fi
    SBE_ROOT="$root" SBE_CORE="$core" SBE_UNIT_DIR="${SBE_UNIT_DIR:-/etc/systemd/system}" \
        "$helper" --root "$root" --core "$core" --unit-dir "${SBE_UNIT_DIR:-/etc/systemd/system}" "$@"
}

sbe_prompt() {
    local _sbe_input
    IFS= read -r -p "$2" _sbe_input || return 1
    if [[ ${3:-required} == required && -z $_sbe_input ]]; then
        printf '%s\n' '未输入，已取消此次操作。' >&2
        return 1
    fi
    printf -v "$1" '%s' "$_sbe_input"
}

sbe_menu_account() {
    local action=$1 target quota target_flag=--id
    local -a quota_flags
    [[ $action == create ]] && target_flag=--name
    sbe_prompt target '应用账号名字或 ID：' || return
    case $action in
    create | quota)
        sbe_prompt quota '月额度（字节数；无限输入 unlimited 或 无限，0 表示零额度）：' || return
        case $quota in
        unlimited | 无限) quota_flags=(--unlimited) ;;
        *)
            if [[ ! $quota =~ ^[0-9]+$ ]]; then
                printf '%s\n' '额度须为非负整数字节数或 unlimited，操作未执行。' >&2
                return 1
            fi
            quota_flags=(--bytes "$quota")
            ;;
        esac
        sbe_run account "$action" "$target_flag" "$target" "${quota_flags[@]}"
        ;;
    *) sbe_run account "$action" --id "$target" ;;
    esac
}

sbe_menu_import_node() {
    local name template account port
    local -a binding_args
    sbe_prompt name '逻辑节点名字：' || return
    sbe_prompt template '单入站 JSON 模板文件路径：' || return
    [[ -f $template && -r $template ]] || { printf '%s\n' '模板文件不存在或不可读，操作未执行。' >&2; return 1; }
    sbe_prompt account '绑定的应用账号名字或 ID：' || return
    sbe_prompt port '专用端口（留空自动分配）：' optional || return
    binding_args=(--node "$name" --account "$account")
    if [[ -n $port ]]; then
        if [[ ! $port =~ ^[0-9]{1,5}$ ]] || (( 10#$port < 1 || 10#$port > 65535 )); then
            printf '%s\n' '端口须为 1 至 65535，操作未执行。' >&2
            return 1
        fi
        binding_args+=(--port "$port")
    fi
    sbe_run node add --name "$name" --template "$template" || return
    sbe_run node bind "${binding_args[@]}" || {
        printf '%s\n' '节点模板已导入但绑定失败；节点保持未绑定，请检查账号或端口后重新绑定。' >&2
        return 1
    }
}

sbe_menu_share() {
    local action=$1 account server node
    local -a share_args
    sbe_prompt account '应用账号名字或 ID：' || return
    sbe_prompt server '客户端使用的服务器地址（HOST 或 HOST:PORT）：' || return
    sbe_prompt node '节点名字或 ID（留空显示此账号全部节点）：' optional || return
    share_args=(--account "$account" --server "$server")
    [[ -z $node ]] || share_args+=(--node "$node")
    sbe_run node "$action" "${share_args[@]}"
}

sbe_menu_admin_password() {
    local password confirmation result=1 tracing=0
    if [[ $- == *x* ]]; then
        tracing=1
        set +x
    fi
    if IFS= read -r -s -p '新应用管理员密码（隐藏输入）：' password; then
        printf '\n'
        if IFS= read -r -s -p '再次输入密码：' confirmation; then
            printf '\n'
            if [[ -z $password ]]; then
                printf '%s\n' '密码为空，未修改。' >&2
            elif [[ $password != "$confirmation" ]]; then
                printf '%s\n' '两次输入不一致，未修改。' >&2
            else
                printf '%s\n' "$password" | sbe_run admin passwd --password-stdin
                result=$?
            fi
        else
            printf '\n%s\n' '输入已取消，未修改密码。' >&2
        fi
    else
        printf '\n%s\n' '输入已取消，未修改密码。' >&2
    fi
    unset password confirmation
    [[ $tracing == 0 ]] || set -x
    return "$result"
}

sbe_menu() {
    local choice account domain acceptance admin_name
    while true; do
        printf '\n%s\n' 'sing-box Enhanced' \
            '1. 应用账号列表' '2. 创建应用账号' '3. 启用账号' '4. 停用账号' \
            '5. 修改月额度' '6. 手动重置当月流量' '7. 节点列表' \
            '8. 导入 JSON 模板节点并绑定账号' '9. 查看账号实际节点配置' \
            '10. WARP 状态' '11. WARP 域名规则' '12. 增加 WARP 域名' '13. 删除 WARP 域名' \
            '14. 安装 WARP' '15. 接受条款并注册 WARP' '16. 连接 WARP' '17. 断开 WARP' \
            '18. 受管服务状态' '19. 启动受管服务' '20. 停止全部受管账号' \
            '21. 重启受管服务' '22. 命令帮助' '23. 查看单节点分享链接' \
            '24. 查看单节点二维码' '25. 查看流量守护日志' '26. 查看应用管理员' \
            '27. 修改应用管理员名字' '28. 修改应用管理员密码' '0. 退出'
        if [[ ${SBE_DEVELOPER:-0} == 1 ]] && sbe_legacy_present; then
            printf '%s\n' '90. 检查原脚本迁移（只读）'
        fi
        if ! IFS= read -r -p '请选择：' choice; then
            return 0
        fi
        case $choice in
        1) sbe_run account list ;;
        2) sbe_menu_account create ;;
        3) sbe_menu_account enable ;;
        4) sbe_menu_account disable ;;
        5) sbe_menu_account quota ;;
        6) sbe_menu_account reset ;;
        7) sbe_run node list ;;
        8) sbe_menu_import_node ;;
        9) sbe_prompt account '应用账号名字或 ID：' && sbe_run node endpoints --account "$account" ;;
        10) sbe_run warp status ;;
        11) sbe_run warp list ;;
        12) sbe_prompt domain '走 WARP 的固定域名（精确匹配）：' && sbe_run warp add --domain "$domain" ;;
        13) sbe_prompt domain '删除的固定域名：' && sbe_run warp remove --domain "$domain" ;;
        14) sbe_run warp install ;;
        15)
            printf '%s\n' 'Cloudflare 应用服务条款：https://www.cloudflare.com/application/terms/'
            if sbe_prompt acceptance '输入 ACCEPT 表示接受 Cloudflare 条款并注册：' && [[ $acceptance == ACCEPT ]]; then
                sbe_run warp register --accept-terms
            else
                printf '%s\n' '未接受条款，未执行注册。'
            fi
            ;;
        16) sbe_run warp connect ;;
        17) sbe_run warp disconnect ;;
        18) sbe_run service status ;;
        19) sbe_run service start ;;
        20) sbe_run service stop ;;
        21) sbe_run service restart ;;
        22) sbe_help ;;
        23) sbe_menu_share url ;;
        24) sbe_menu_share qr ;;
        25) sbe_run service log ;;
        26) sbe_run admin show ;;
        27) sbe_prompt admin_name '新的应用管理员名字：' && sbe_run admin rename --name "$admin_name" ;;
        28) sbe_menu_admin_password ;;
        90)
            if [[ ${SBE_DEVELOPER:-0} == 1 ]] && sbe_legacy_present; then
                sbe_run migrate inspect
            else
                printf '%s\n' '迁移仅在开发者模式且检测到旧安装时提供。' >&2
            fi
            ;;
        0) return 0 ;;
        *) printf '%s\n' '请输入菜单中的数字。' >&2 ;;
        esac
    done
}

# Return 1 only when an unmanaged installation may use the upstream dispatcher.
# Any intercepted failure exits, so it never falls through to an upstream writer.
sbe_dispatch() {
    local command=${1:-} root core
    root=$(sbe_root)
    case $command in
    admin | account | node | warp)
        sbe_run "$@" || exit "$?"
        return 0
        ;;
    service)
        if [[ $# -ne 2 && ${2:-} != log ]]; then
            printf '%s\n' '受管服务命令不接受额外参数，操作未执行。仅 sb service log 支持 --account/--tail。' >&2
            exit 1
        fi
        sbe_run "$@" || exit "$?"
        return 0
        ;;
    quota)
        shift
        sbe_run account quota "$@" || exit "$?"
        return 0
        ;;
    migrate)
        if [[ ${SBE_DEVELOPER:-0} != 1 ]] || ! sbe_legacy_present "$@"; then
            printf '%s\n' '迁移仅在 SBE_DEVELOPER=1 且检测到旧安装时提供。' >&2
            exit 1
        fi
        sbe_run "$@" || exit "$?"
        return 0
        ;;
    enhanced)
        case ${2:-} in
        '' | menu) sbe_menu ;;
        help | -h | --help) sbe_help ;;
        *) printf '%s\n' '请使用 sb enhanced 或 sb enhanced help。' >&2; exit 1 ;;
        esac
        return 0
        ;;
    esac

    [[ -f $root/managed.marker ]] || return 1
    core=${SBE_CORE:-$root/bin/sing-box}
    case $command in
    '' | main) sbe_menu ;;
    h | help | --help | -h) sbe_help ;;
    i | info)
        if [[ $# -eq 1 ]]; then
            sbe_run node list || exit "$?"
        elif [[ $# -eq 3 && $2 == --account && -n $3 ]]; then
            sbe_run node endpoints --account "$3" || exit "$?"
        else
            printf '%s\n' '请使用 sb info 查看节点列表，或 sb info --account NAME_OR_ID 查看账号实际配置。' >&2
            exit 1
        fi
        ;;
    url | qr)
        shift
        sbe_run node "$command" "$@" || exit "$?"
        ;;
    log)
        shift
        sbe_run service log "$@" || exit "$?"
        ;;
    s | status)
        [[ $# -eq 1 ]] || { printf '%s\n' '受管状态命令不接受额外参数，操作未执行。' >&2; exit 1; }
        sbe_run service status || exit "$?"
        ;;
    start | stop | restart | r)
        [[ $# -eq 1 ]] || { printf '%s\n' '受管服务命令不接受 caddy 等额外参数，操作未执行。' >&2; exit 1; }
        [[ $command == r ]] && command=restart
        sbe_run service "$command" || exit "$?"
        ;;
    v | ver | version)
        printf '%s\n' '233Boy sing-box Enhanced（保留 233Boy 上游署名）'
        "$core" version || exit "$?"
        ;;
    generate | pbk)
        shift
        [[ $command == pbk ]] && set -- reality-keypair "$@"
        "$core" generate "$@" || exit "$?"
        ;;
    *)
        printf '已接管账号配额，原命令 "%s" 不允许直接执行。\n' "$command" >&2
        printf '%s\n' '节点使用 sb node，账号使用 sb account，服务使用 sb service；帮助：sb enhanced help。' >&2
        exit 1
        ;;
    esac
    return 0
}
