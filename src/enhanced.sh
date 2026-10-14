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

# Menus keep their own page and use stable IDs from one choices snapshot.
# EOF is shared by nested pages so a closed stdin exits the whole menu.
sbe_read() {
    local _sbe_input
    if ! IFS= read -r -p "$2" _sbe_input; then
        SBE_MENU_EOF=1
        return 1
    fi
    printf -v "$1" '%s' "$_sbe_input"
}

sbe_prompt() {
    sbe_read "$1" "$2" || return 1
    if [[ ${3:-required} == required && -z ${!1} ]]; then
        printf '%s\n' '未输入，已取消此次操作。'
        return 1
    fi
}

sbe_page() {
    [[ ! -t 1 ]] || printf '\033[H\033[2J'
    printf '\n%s\n\n' "sing-box Enhanced > $1"
    if [[ -n ${SBE_MENU_NOTICE:-} ]]; then
        printf '提示：%s\n\n' "$SBE_MENU_NOTICE"
        SBE_MENU_NOTICE=
    fi
}

sbe_pause() {
    local answer
    if [[ -n ${SBE_MENU_ERROR:-} ]]; then
        sbe_read answer '按回车返回；输入 9 查看技术错误：' || return
        if [[ $answer == 9 ]]; then
            printf '%s\n' "$SBE_MENU_ERROR"
            sbe_read answer '按回车返回：' || return
        fi
        SBE_MENU_ERROR=
    else
        sbe_read answer '按回车返回：' || return
    fi
}

sbe_confirm() {
    local answer
    printf '%s\n' "$1"
    sbe_read answer '1. 确认执行  0. 取消（默认取消）：' || return 1
    [[ $answer == 1 ]] || { printf '%s\n' '已取消，未执行。'; return 1; }
}

sbe_view() {
    local output
    SBE_MENU_ERROR=
    if output=$(sbe_run view "$@" 2>&1); then
        printf '%s\n' "$output"
    else
        SBE_MENU_ERROR=$output
        printf '%s\n' '查询未完成，请检查服务状态或查看技术错误。'
        return 1
    fi
}

sbe_change() {
    local message=$1 output
    shift
    SBE_MENU_ERROR=
    if output=$(sbe_run "$@" 2>&1); then
        printf '%s\n' "$message"
    else
        SBE_MENU_ERROR=$output
        printf '%s\n' '操作未完成，请查看技术错误；如涉及多个步骤，请核对当前状态后重试。'
        return 1
    fi
}

# Stores both the selection ID and name; never re-fetches before dispatching it.
# Optional last menu entry is used for account creation.
sbe_select() {
    local kind=$1 title=$2 extra=${3:-} output id name status rest answer index auto=0
    local -a ids=() names=() states=() filters=()
    shift 3
    if [[ ${1:-} == --auto-single ]]; then auto=1; shift; fi
    filters=("$@")
    SBE_SELECTED= SBE_SELECTED_NAME= SBE_MENU_ERROR=
    if ! output=$(sbe_run view choices --kind "$kind" "${filters[@]}" 2>&1); then
        SBE_MENU_ERROR=$output
        printf '%s\n' '无法读取列表，请检查服务状态或查看技术错误。'
        sbe_pause
        return 1
    fi
    while IFS=$'\t' read -r id name status rest; do
        [[ -n $id ]] || continue
        ids+=("$id"); names+=("$name"); states+=("$status")
    done <<<"$output"
    if [[ $auto == 1 && ${#ids[@]} == 1 ]]; then
        SBE_SELECTED=${ids[0]}; SBE_SELECTED_NAME=${names[0]}; return 0
    fi
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page "$title"
        if [[ $kind == domain ]]; then
            printf '%s\n' '匹配方式：精确域名匹配' '作用范围：全部受管新旧节点' '故障策略：WARP 故障时，命中规则的流量阻断'
        fi
        if [[ ${#ids[@]} == 0 ]]; then
            printf '%s\n' '暂无可选项目。'
        else
            printf '%s\n' '编号  名称 / 状态'
            for ((index=0; index<${#ids[@]}; index++)); do
                printf '%d. %s  [%s]\n' "$((index+1))" "${names[index]}" "${states[index]}"
            done
        fi
        [[ -z $extra ]] || printf '%d. %s\n' "$((${#ids[@]}+1))" "$extra"
        printf '\n%s\n' '0. 返回上一层'
        sbe_read answer '请选择编号：' || return 1
        [[ $answer == 0 ]] && return 1
        if [[ -n $extra && $answer == "$((${#ids[@]}+1))" ]]; then
            SBE_SELECTED=__new__
            return 0
        fi
        if [[ $answer =~ ^[1-9][0-9]{0,5}$ ]] && (( answer <= ${#ids[@]} )); then
            SBE_SELECTED=${ids[answer-1]}
            SBE_SELECTED_NAME=${names[answer-1]}
            return 0
        fi
        SBE_MENU_NOTICE='请输入本页显示的编号。'
    done
    return 1
}

sbe_quota_input() {
    local mode amount output
    SBE_QUOTA_FLAGS=()
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        printf '%s\n' '1. 固定月额度（GB，1 GB = 1,000,000,000 字节）' '2. 无限额度' '0. 取消'
        sbe_read mode '请选择额度模式：' || return 1
        case $mode in
        0|'') return 1 ;;
        2) SBE_QUOTA_FLAGS=(--unlimited); SBE_QUOTA_LABEL=无限额度; return 0 ;;
        1)
            sbe_prompt amount '每月额度 GB（支持小数；0 表示零额度，留空取消）：' || return 1
            if output=$(sbe_run view quota-bytes --gb "$amount" 2>&1) && [[ $output =~ ^[0-9]+$ ]]; then
                SBE_QUOTA_FLAGS=(--bytes "$output"); SBE_QUOTA_LABEL="$amount GB/月"
                return 0
            fi
            printf '%s\n' '额度无效，请输入非负数、最多 9 位小数，且不超过可支持的额度。'
            ;;
        *) printf '%s\n' '请输入本页显示的编号。' ;;
        esac
    done
    return 1
}

sbe_menu_share() {
    local action=$1 node=$2 account=$3 server
    sbe_prompt server '客户端使用的服务器地址（HOST 或 HOST:PORT，留空取消）：' || return
    printf '%s\n' '以下内容包含此账号的协议认证信息，请妥善保存。'
    sbe_view "$action" --node "$node" --account "$account" --server "$server"
    sbe_pause
}

sbe_menu_node_detail() {
    local node=$1 name=$2 choice account
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page "查询节点 > $name"
        if ! sbe_view node --node "$node"; then sbe_pause; return; fi
        printf '\n%s\n' '1. 查看连接信息' '2. 查看分享链接' '3. 查看二维码' '0. 返回节点列表'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1|2|3)
            if sbe_select account "查询节点 > $name > 选择已绑定账号" '' --auto-single --node "$node"; then
                account=$SBE_SELECTED
                case $choice in
                1) sbe_view endpoint --node "$node" --account "$account"; sbe_pause ;;
                2) sbe_menu_share url "$node" "$account" ;;
                3) sbe_menu_share qr "$node" "$account" ;;
                esac
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_nodes() {
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_select node '查询节点' '' || return
        sbe_menu_node_detail "$SBE_SELECTED" "$SBE_SELECTED_NAME"
    done
}

sbe_menu_account_create() {
    local name
    sbe_page '账号与流量 > 创建流量账号'
    sbe_prompt name '新账号名称（留空取消）：' || return
    sbe_quota_input || return
    sbe_confirm "创建流量账号：$name；额度：$SBE_QUOTA_LABEL。账号不是 Linux 系统用户。" || return
    sbe_change '流量账号已创建；请在节点配置管理中绑定节点。' account create --name "$name" "${SBE_QUOTA_FLAGS[@]}"
    sbe_pause
}

sbe_menu_account_detail() {
    local account=$1 name=$2 choice
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page "账号与流量 > $name"
        if ! sbe_view account --account "$account"; then sbe_pause; return; fi
        printf '\n%s\n' '1. 启用账号' '2. 停用账号' '3. 修改月额度' '4. 手动重置当月流量' '5. 恢复计量故障' '6. 删除账号' '0. 返回账号列表'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1|2|4|5|6)
            case $choice in
            1) local action=enable message='账号已设为启用；访问仍受额度、计量故障及全局服务状态约束。' impact='启用后，符合访问条件的关联节点可能启动。' ;;
            2) local action=disable message='账号已停用。' impact='该账号全部受管节点将停止，现有连接会中断。' ;;
            4) local action=reset message='当月流量已重置；手动停用和计量故障不会因此解除。' impact='当月上传、下载计量将清零；因额度停止的节点可能恢复。' ;;
            5) local action=recover message='计量故障恢复操作已完成，请核对当前账号状态。' impact='清除计量故障限制；符合访问条件的节点可能启动。' ;;
            6) local action=delete message='账号已删除。' impact='删除账号及其关联身份；该账号连接会中断，操作不可撤销。' ;;
            esac
            if sbe_confirm "对象：$name。$impact"; then
                if sbe_change "$message" account "$action" --id "$account"; then
                    sbe_pause
                    [[ $choice != 6 ]] || return
                else sbe_pause; fi
            fi
            ;;
        3)
            if sbe_quota_input && sbe_confirm "对象：$name；新额度：$SBE_QUOTA_LABEL。已用流量保留，全部关联节点可能停止或重新启动。"; then
                sbe_change '月额度已修改，请核对当前账号访问状态。' account quota --id "$account" "${SBE_QUOTA_FLAGS[@]}"
                sbe_pause
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_accounts() {
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_select account '账号与流量' '创建流量账号' || return
        if [[ $SBE_SELECTED == __new__ ]]; then sbe_menu_account_create
        else sbe_menu_account_detail "$SBE_SELECTED" "$SBE_SELECTED_NAME"; fi
    done
}

sbe_port_input() {
    SBE_PORT_FLAGS=()
    local port
    sbe_prompt port '专用端口（留空自动分配）：' optional || return 1
    if [[ -n $port ]]; then
        if [[ ! $port =~ ^[0-9]{1,5}$ ]] || (( 10#$port < 1 || 10#$port > 65535 )); then
            printf '%s\n' '端口须为 1 至 65535，未执行。'
            return 1
        fi
        SBE_PORT_FLAGS=(--port "$port")
    fi
}

sbe_menu_import_node() {
    local name template account account_name
    sbe_prompt name '逻辑节点名字（留空取消）：' || return
    sbe_prompt template '单入站 JSON 模板文件路径（留空取消）：' || return
    [[ -f $template && -r $template ]] || { printf '%s\n' '模板文件不存在或不可读，未执行。'; sbe_pause; return; }
    sbe_select account '节点配置管理 > 选择绑定账号' '' || return
    account=$SBE_SELECTED; account_name=$SBE_SELECTED_NAME
    sbe_port_input || { sbe_pause; return; }
    sbe_confirm "导入节点：$name；绑定账号：$account_name。该账号的实例将重新加载，现有连接可能中断。" || return
    if sbe_change '节点模板已导入。' node add --name "$name" --template "$template"; then
        if ! sbe_change '节点已绑定，请到查询节点查看连接信息。' node bind --node "$name" --account "$account" "${SBE_PORT_FLAGS[@]}"; then
            printf '%s\n' '模板已导入但绑定未完成，节点保持未绑定；请从绑定节点继续处理。'
        fi
    fi
    sbe_pause
}

sbe_menu_node_config() {
    local choice node name account account_name action
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page '节点配置管理'
        printf '%s\n' '1. 导入模板并绑定账号' '2. 将节点绑定到账号' '3. 解绑节点与账号' '4. 删除节点' '0. 返回主菜单'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1) sbe_menu_import_node ;;
        2|3|4)
            sbe_select node '节点配置管理 > 选择节点' '' || continue
            node=$SBE_SELECTED; name=$SBE_SELECTED_NAME
            if [[ $choice == 4 ]]; then
                if sbe_confirm "删除节点：$name。所有关联账号的此节点身份将被移除，连接会中断；操作不可撤销。"; then
                    sbe_change '节点已删除。' node delete --node "$node"; sbe_pause
                fi
                continue
            fi
            if [[ $choice == 3 ]]; then
                sbe_select account "节点配置管理 > $name > 选择已绑定账号" '' --node "$node" || continue
                action=unbind
            else
                sbe_select account "节点配置管理 > $name > 选择账号" '' || continue
                action=bind
            fi
            account=$SBE_SELECTED; account_name=$SBE_SELECTED_NAME
            SBE_PORT_FLAGS=()
            [[ $choice != 2 ]] || sbe_port_input || { sbe_pause; continue; }
            if sbe_confirm "节点：$name；账号：$account_name。将执行 $([[ $choice == 2 ]] && printf 绑定 || printf 解绑)，此账号现有连接可能中断。"; then
                sbe_change '节点关联已更新。' node "$action" --node "$node" --account "$account" "${SBE_PORT_FLAGS[@]}"; sbe_pause
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_warp_rules() {
    local domain choice
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_select domain 'WARP 管理 > 域名规则' '' || return
        domain=$SBE_SELECTED
        while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
            sbe_page "WARP 管理 > 域名规则 > $domain"
            printf '%s\n' "精确匹配域名：$domain" '作用范围：全部受管节点，包括迁入的旧节点。' '故障策略：WARP 故障时，命中规则的流量阻断。' '1. 删除此规则' '0. 返回规则列表'
            sbe_read choice '请选择：' || return
            case $choice in
            0) break ;;
            1)
                if sbe_confirm "删除 WARP 规则：$domain。全部受管账号实例可能重新加载，现有连接可能中断；该域名将使用常规路由。"; then
                    if sbe_change 'WARP 域名规则已删除。' warp remove --domain "$domain"; then
                        sbe_pause
                        break
                    fi
                    sbe_pause
                fi
                ;;
            *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
            esac
        done
    done
}

sbe_menu_warp() {
    local choice domain acceptance
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page 'WARP 管理'
        printf '%s\n' '1. 查看 WARP 状态' '2. 查看域名规则' '3. 增加域名规则' '4. 安装 WARP' '5. 接受条款并注册' '6. 连接 WARP' '7. 断开 WARP' '0. 返回主菜单'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1) sbe_view warp; sbe_pause ;;
        2) sbe_menu_warp_rules ;;
        3)
            if sbe_prompt domain '新增固定域名（精确匹配，留空取消）：' && sbe_confirm "增加 WARP 域名：$domain；应用于全部受管新旧节点。账号实例可能重新加载，现有连接可能中断。"; then
                sbe_change 'WARP 域名规则已增加。' warp add --domain "$domain"; sbe_pause
            fi
            ;;
        4)
            if sbe_confirm '将安装 Cloudflare WARP，可能需要等待；不更改账号额度。'; then
                if sbe_run warp install; then printf '%s\n' 'WARP 安装已完成。'
                else printf '%s\n' 'WARP 安装未完成，请根据上方安装日志处理。'; fi
                sbe_pause
            fi
            ;;
        5)
            printf '%s\n' 'Cloudflare 应用服务条款：https://www.cloudflare.com/application/terms/'
            if sbe_prompt acceptance '输入 ACCEPT 表示接受条款并注册（留空取消）：' && [[ $acceptance == ACCEPT ]]; then
                sbe_change 'WARP 注册已完成。' warp register --accept-terms; sbe_pause
            else SBE_MENU_NOTICE='未接受条款，未执行注册。'; fi
            ;;
        6|7)
            if [[ $choice == 6 ]]; then
                if sbe_confirm '连接 WARP 独立代理出口；只有命中固定域名规则的流量使用此出口。'; then
                    sbe_change 'WARP 连接命令已完成，请通过状态页确认实际出口。' warp connect; sbe_pause
                fi
            elif sbe_confirm '断开 WARP 后，全部受管节点命中规则的流量将阻断；账号配额限制仍生效。'; then
                sbe_change 'WARP 已断开。' warp disconnect; sbe_pause
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_services() {
    local choice action
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page '服务管理'
        sbe_view services || sbe_pause
        printf '\n%s\n' '1. 启动受管服务' '2. 停止全部受管服务' '3. 重启受管服务' '0. 返回主菜单'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1|2|3)
            case $choice in 1) action=start;; 2) action=stop;; 3) action=restart;; esac
            if sbe_confirm "操作：$([[ $choice == 1 ]] && printf 启动 || { [[ $choice == 2 ]] && printf 停止 || printf 重启; })全部受管服务。停止或重启将中断全部受管账号连接；启动仍遵守各账号额度和停用限制。"; then
                sbe_change '服务命令已完成，请核对服务状态。' service "$action"; sbe_pause
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_logs() {
    local choice
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page '查看日志'
        printf '%s\n' '1. 流量守护日志（最近 50 行）' '2. 账号运行日志（最近 50 行）' '0. 返回主菜单'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1) sbe_run service log --tail 50; sbe_pause ;;
        2)
            if sbe_select account '查看日志 > 选择账号' ''; then
                sbe_run service log --account "$SBE_SELECTED" --tail 50; sbe_pause
            fi
            ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu_admin_password() {
    local password confirmation result=1 tracing=0 output
    if [[ $- == *x* ]]; then tracing=1; set +x; fi
    if sbe_read_password password '新应用管理员密码（12–1024 字节，隐藏输入，留空取消）：' && [[ -n $password ]]; then
        if sbe_read_password confirmation '再次输入密码：'; then
            if [[ $password != "$confirmation" ]]; then
                printf '%s\n' '两次输入不一致，未修改。'
            elif output=$(printf '%s\n' "$password" | sbe_run admin passwd --password-stdin 2>&1); then
                printf '%s\n' '应用管理员密码已修改。'; result=0
            else
                SBE_MENU_ERROR=$output
                printf '%s\n' '密码修改未完成，请查看技术错误。'
            fi
        fi
    else printf '%s\n' '未输入密码，未修改。'; fi
    unset password confirmation output
    [[ $tracing == 0 ]] || set -x
    return "$result"
}

sbe_read_password() {
    local _sbe_password
    if ! IFS= read -r -s -p "$2" _sbe_password; then
        SBE_MENU_EOF=1; printf '\n'; return 1
    fi
    printf '\n'
    printf -v "$1" '%s' "$_sbe_password"
    unset _sbe_password
}

sbe_menu_admin() {
    local choice name
    while [[ ${SBE_MENU_EOF:-0} == 0 ]]; do
        sbe_page '应用管理员'
        sbe_view admin || sbe_pause
        printf '\n%s\n' '1. 修改应用管理员名称' '2. 修改应用管理员密码' '0. 返回主菜单'
        sbe_read choice '请选择：' || return
        case $choice in
        0) return ;;
        1)
            if sbe_prompt name '新的应用管理员名称（留空取消）：' && sbe_confirm "应用管理员新名称：$name。保持现有账号身份、节点和流量记录。"; then
                sbe_change '应用管理员名称已修改。' admin rename --name "$name"; sbe_pause
            fi
            ;;
        2) SBE_MENU_ERROR=; sbe_menu_admin_password; sbe_pause ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
}

sbe_menu() {
    local choice
    SBE_MENU_EOF=0 SBE_MENU_ERROR= SBE_MENU_NOTICE=
    while [[ $SBE_MENU_EOF == 0 ]]; do
        sbe_page '主菜单'
        printf '%s\n' '1. 查询节点' '2. 账号与流量' '3. 节点配置管理' '4. WARP 管理' '5. 服务管理' '6. 查看日志' '7. 应用管理员' '8. 使用帮助' '0. 退出'
        if [[ ${SBE_DEVELOPER:-0} == 1 ]] && sbe_legacy_present; then
            printf '%s\n' '90. 检查原脚本迁移（只读）'
        fi
        sbe_read choice '请选择：' || return 0
        case $choice in
        1) sbe_menu_nodes ;;
        2) sbe_menu_accounts ;;
        3) sbe_menu_node_config ;;
        4) sbe_menu_warp ;;
        5) sbe_menu_services ;;
        6) sbe_menu_logs ;;
        7) sbe_menu_admin ;;
        8) sbe_page '使用帮助'; sbe_help; sbe_pause ;;
        90)
            if [[ ${SBE_DEVELOPER:-0} == 1 ]] && sbe_legacy_present; then
                sbe_change '迁移检查已完成；开发数据可通过 sb migrate inspect 单独查看。' migrate inspect; sbe_pause
            else printf '%s\n' '迁移仅在开发者模式且检测到旧安装时提供。'; fi
            ;;
        0) return 0 ;;
        *) SBE_MENU_NOTICE='请输入本页显示的编号。' ;;
        esac
    done
    return 0
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
