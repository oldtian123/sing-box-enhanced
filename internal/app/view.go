package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	runtimeManager "github.com/oldtian123/sing-box-enhanced/internal/runtime"
	"github.com/oldtian123/sing-box-enhanced/internal/share"
	"github.com/oldtian123/sing-box-enhanced/internal/store"
	"github.com/oldtian123/sing-box-enhanced/internal/warp"
)

// Views take a short database snapshot. No user input, network probe, service
// command, or QR renderer runs while holding the accounting lock.
type viewSnapshot struct {
	accounts []store.AccountStatus
	nodes    []viewNode
	bindings []viewBinding
	admin    *store.Administrator
	domains  []string
	paused   bool
	port     int
	accepted bool
}

type viewNode struct {
	id, name string
	legacy   bool
	template []byte
}

type viewBinding struct {
	account, node string
	inbound       []byte
}

// Remove complete terminal escape sequences before removing control/format
// characters. Keeping only the latter would leave visible escape payloads.
var terminalEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-_]`)

func viewText(s string) string {
	s = terminalEscape.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2f GB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2f MB", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.2f KB", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func viewPolicy(s store.AccountStatus) string {
	switch s.Reason {
	case "disabled":
		return "禁止访问：手动停用"
	case "stats_fault":
		return "禁止访问：计量故障保护，需人工恢复"
	case "quota_exceeded":
		return "禁止访问：当月流量已达限额"
	case "clock_rollback":
		return "禁止访问：服务器时间早于已记录账期"
	case "":
		if s.Allowed {
			return "允许访问"
		}
	}
	return "访问策略未知"
}

func humanUnitState(state string) string {
	switch state {
	case "active":
		return "运行中"
	case "inactive":
		return "已停止"
	case "failed":
		return "运行故障"
	case "activating":
		return "启动中"
	case "deactivating":
		return "停止中"
	case "reloading", "refreshing":
		return "重新加载中"
	case "maintenance":
		return "维护中"
	case "not-found":
		return "未安装"
	default:
		return "状态未知"
	}
}

func (a *App) viewSnapshot() (viewSnapshot, error) {
	s := viewSnapshot{port: 40000, paused: a.setting("paused") == "1", accepted: a.setting("warp_terms_accepted") == "1"}
	if v := a.setting("warp_port"); v != "" {
		var err error
		s.port, err = strconv.Atoi(v)
		if err != nil {
			return s, err
		}
	}
	var err error
	s.domains, err = a.domains()
	if err != nil {
		return s, err
	}
	accounts, err := a.DB.Accounts()
	if err != nil {
		return s, err
	}
	period := store.Period(time.Now())
	for _, account := range accounts {
		st := store.AccountStatus{Account: account, Unlimited: account.Unlimited,
			Usage: store.Usage{AccountID: account.ID, Period: period}}
		err = a.DB.DB.QueryRow("SELECT upload,download FROM usage WHERE account_id=? AND period=?", account.ID, period).Scan(&st.Usage.Upload, &st.Usage.Download)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return s, err
		}
		switch {
		case !account.Enabled:
			st.Reason = "disabled"
		case account.StatsFault:
			st.Reason = "stats_fault"
		case a.setting("active_period") > period:
			st.Reason = "clock_rollback"
		case !account.Unlimited && (st.Usage.Upload >= account.QuotaBytes || st.Usage.Download >= account.QuotaBytes-st.Usage.Upload):
			st.Reason = "quota_exceeded"
		default:
			st.Allowed = true
		}
		s.accounts = append(s.accounts, st)
	}
	rows, err := a.DB.DB.Query("SELECT id,name,legacy,template FROM nodes ORDER BY name,id")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var n viewNode
		if err = rows.Scan(&n.id, &n.name, &n.legacy, &n.template); err != nil {
			rows.Close()
			return s, err
		}
		s.nodes = append(s.nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = a.DB.DB.Query("SELECT account_id,node_id,inbound FROM bindings ORDER BY node_id,account_id")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var b viewBinding
		if err = rows.Scan(&b.account, &b.node, &b.inbound); err != nil {
			rows.Close()
			return s, err
		}
		s.bindings = append(s.bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return s, err
	}
	admin, err := a.DB.Administrator()
	if err == nil {
		admin.Hash = ""
		s.admin = &admin
	} else if !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	return s, nil
}

func (s viewSnapshot) node(key string) (viewNode, error) {
	for _, n := range s.nodes {
		if n.id == key {
			return n, nil
		}
	}
	for _, n := range s.nodes {
		if n.name == key {
			return n, nil
		}
	}
	return viewNode{}, errors.New("找不到所选节点，请返回列表刷新")
}

func (s viewSnapshot) account(key string) (store.AccountStatus, error) {
	for _, a := range s.accounts {
		if a.Account.ID == key {
			return a, nil
		}
	}
	for _, a := range s.accounts {
		if a.Account.Name == key {
			return a, nil
		}
	}
	return store.AccountStatus{}, errors.New("找不到所选账号，请返回列表刷新")
}

func (s viewSnapshot) nodeBindings(id string) []viewBinding {
	var result []viewBinding
	for _, b := range s.bindings {
		if b.node == id {
			result = append(result, b)
		}
	}
	return result
}

func (s viewSnapshot) nodeState(id string, states map[string]string) string {
	bindings := s.nodeBindings(id)
	if len(bindings) == 0 {
		return "未绑定"
	}
	active, unknown, fault, changing := 0, false, false, false
	firstState := states[runtimeManager.UnitName(bindings[0].account)]
	allSame := true
	for _, b := range bindings {
		state := states[runtimeManager.UnitName(b.account)]
		allSame = allSame && state == firstState
		switch state {
		case "active", "reloading", "refreshing":
			active++
		case "inactive", "not-found":
		case "deactivating", "activating":
			changing = true
		case "failed", "maintenance":
			fault = true
		default:
			unknown = true
		}
	}
	if allSame {
		if firstState == "not-found" {
			return "未创建实例"
		}
		return humanUnitState(firstState)
	}
	switch {
	case active == len(bindings):
		return "运行中"
	case active > 0:
		if unknown {
			return "部分运行（含未知状态）"
		}
		return "部分运行"
	case unknown:
		return "状态未知"
	case fault:
		return "运行故障"
	case changing:
		return "状态变化中"
	default:
		return "已停止"
	}
}

func runView(o options, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("缺少展示页面")
	}
	operation := args[0]
	if operation == "quota-bytes" {
		var gb string
		if _, err := commandFlags("view quota-bytes", args[1:], func(f *flag.FlagSet) {
			f.StringVar(&gb, "gb", "", "decimal GB quota")
		}); err != nil {
			return err
		}
		n, err := quotaBytes(gb)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, n)
		return err
	}
	var node, account, kind, server string
	var insecure bool
	if _, err := commandFlags("view", args[1:], func(f *flag.FlagSet) {
		f.StringVar(&node, "node", "", "node name or ID")
		f.StringVar(&account, "account", "", "account name or ID")
		f.StringVar(&kind, "kind", "", "choice type")
		f.StringVar(&server, "server", "", "public server address")
		f.BoolVar(&insecure, "insecure", false, "explicitly allow untrusted client certificate")
	}); err != nil {
		return err
	}
	switch operation {
	case "nodes", "node", "account", "warp", "rules", "services", "admin", "endpoint", "url", "qr", "choices":
	default:
		return errors.New("未知展示页面")
	}
	if operation == "choices" && kind != "node" && kind != "account" && kind != "domain" {
		return errors.New("choices --kind 必须是 node、account 或 domain")
	}
	var snapshot viewSnapshot
	if err := withApp(o, out, func(a *App) error {
		var err error
		snapshot, err = a.viewSnapshot()
		return err
	}); err != nil {
		return err
	}
	states := map[string]string{}
	var stateErr error
	if operation == "nodes" || operation == "node" || operation == "account" || operation == "services" || (operation == "choices" && kind != "domain") {
		units := []string{}
		if operation == "services" {
			units = append(units, "sing-box-enhanced-meter.service", "warp-svc.service")
		}
		for _, a := range snapshot.accounts {
			units = append(units, runtimeManager.UnitName(a.Account.ID))
		}
		if len(units) > 0 {
			states, stateErr = (runtimeManager.Manager{Root: o.Root, Core: o.Core, UnitDir: o.UnitDir}).UnitStates(units)
		}
	}
	return snapshot.render(operation, kind, node, account, server, insecure, states, stateErr, o, out)
}

var decimalGB = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,9})?$`)

func quotaBytes(gb string) (int64, error) {
	if !decimalGB.MatchString(gb) {
		return 0, errors.New("额度须为非负 GB 数字，最多 9 位小数")
	}
	whole, fraction, _ := strings.Cut(gb, ".")
	n, err := strconv.ParseUint(whole, 10, 64)
	if err != nil || n > math.MaxInt64/1_000_000_000 {
		return 0, errors.New("额度超过可支持的最大值")
	}
	n *= 1_000_000_000
	if fraction != "" {
		f, _ := strconv.ParseUint(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
		n += f
	}
	if n > math.MaxInt64 {
		return 0, errors.New("额度超过可支持的最大值")
	}
	return int64(n), nil
}

func (s viewSnapshot) render(operation, kind, nodeKey, accountKey, server string, insecure bool, states map[string]string, stateErr error, o options, out io.Writer) error {
	if operation == "choices" {
		return s.choices(kind, nodeKey, states, out)
	}
	if stateErr != nil {
		fmt.Fprintln(out, "提示：部分服务状态读取失败，未知状态不代表已停止。")
	}
	switch operation {
	case "nodes":
		if len(s.nodes) == 0 {
			fmt.Fprintln(out, "暂无节点。")
		}
		for i, n := range s.nodes {
			fmt.Fprintf(out, "%d. %s  [%s]\n", i+1, viewText(n.name), s.nodeState(n.id, states))
		}
	case "node":
		n, err := s.node(nodeKey)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "节点名称：%s\n节点状态：%s\n协议类型：%s\n", viewText(n.name), s.nodeState(n.id, states), viewProtocol(n.template))
		if n.legacy {
			fmt.Fprintln(out, "配置来源：旧配置迁入（单账号归属）")
		} else {
			fmt.Fprintln(out, "配置来源：新建模板")
		}
		bindings := s.nodeBindings(n.id)
		if len(bindings) == 0 {
			fmt.Fprintln(out, "关联账号：暂无")
		}
		for _, b := range bindings {
			a, err := s.account(b.account)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "关联账号：%s；%s；进程%s\n", viewText(a.Account.Name), viewPolicy(a), humanUnitState(states[runtimeManager.UnitName(b.account)]))
		}
		fmt.Fprintln(out, "认证凭据：已隐藏，请选择“查看连接信息”主动展开。")
		if s.paused {
			fmt.Fprintln(out, "全局服务已暂停。")
		}
	case "account":
		a, err := s.account(accountKey)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "账号名称：%s\n访问策略：%s\n独立进程：%s\n", viewText(a.Account.Name), viewPolicy(a), humanUnitState(states[runtimeManager.UnitName(a.Account.ID)]))
		fmt.Fprintf(out, "当月上传：%s\n当月下载：%s\n当月合计：%s\n", humanBytes(a.Usage.Upload), humanBytes(a.Usage.Download), humanBytes(a.Usage.Upload+a.Usage.Download))
		if a.Unlimited {
			fmt.Fprintln(out, "月度限额：无限额\n剩余额度：无限额")
		} else {
			remaining := a.Account.QuotaBytes - a.Usage.Upload - a.Usage.Download
			if remaining < 0 {
				remaining = 0
			}
			fmt.Fprintf(out, "月度限额：%s\n剩余额度：%s\n", humanBytes(a.Account.QuotaBytes), humanBytes(remaining))
		}
		fmt.Fprintf(out, "当前账期：%s（北京时间）\n重置规则：每月 1 日 00:00，不结转\n单位：1 GB = 1,000,000,000 B\n", a.Usage.Period)
		fmt.Fprintln(out, "关联节点：")
		count := 0
		for _, b := range s.bindings {
			if b.account == a.Account.ID {
				n, err := s.node(b.node)
				if err != nil {
					return err
				}
				count++
				fmt.Fprintf(out, "  %d. %s\n", count, viewText(n.name))
			}
		}
		if count == 0 {
			fmt.Fprintln(out, "  暂无绑定节点")
		}
		if s.paused {
			fmt.Fprintln(out, "全局服务已暂停。")
		}
		fmt.Fprintln(out, "访问策略与进程状态分别展示；运行中不代表公网连接已验证。")
	case "rules":
		if len(s.domains) == 0 {
			fmt.Fprintln(out, "当前尚未配置域名规则。")
		}
		for i, domain := range s.domains {
			fmt.Fprintf(out, "%d. %s\n", i+1, viewText(domain))
		}
		fmt.Fprintf(out, "匹配方式：精确域名匹配\n作用范围：全部受管节点，包含迁入的旧节点\n故障策略：WARP 故障时，命中规则的流量阻断\n本地代理：127.0.0.1:%d\n", s.port)
	case "warp":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := (warp.Client{Port: s.port, Binary: warpBinary(o.Root), AcceptTerms: s.accepted}).Status(ctx)
		fmt.Fprintf(out, "WARP 客户端：%s\n守护进程连接：%s\n本地代理：127.0.0.1:%d\n代理连通检测：%s\n", yesNo(status.Installed, "已安装", "未检测到"), yesNo(status.DaemonConnected, "已连接", "未确认连接"), s.port, yesNo(status.ProxyReachable, "成功", "未确认可达"))
		fmt.Fprintf(out, "出口 IP：%s\n实际地区：%s\nWARP 标识：%s\n", orUnknown(status.ExitIP), orUnknown(status.Location), orUnknown(status.WARP))
		fmt.Fprintln(out, "出口地区可能变化，以本次检测为准。")
		if err != nil {
			fmt.Fprintf(out, "检测失败：%s\n已保留上方能够取得的状态信息。\n", viewText(err.Error()))
		}
	case "services":
		fmt.Fprintf(out, "全局管理：%s\n流量计量服务：%s\nWARP 服务：%s\n", yesNo(s.paused, "已暂停", "未暂停"), humanUnitState(states["sing-box-enhanced-meter.service"]), humanUnitState(states["warp-svc.service"]))
		if len(s.accounts) == 0 {
			fmt.Fprintln(out, "暂无账号实例。")
		}
		for _, a := range s.accounts {
			fmt.Fprintf(out, "%s：进程%s；%s\n", viewText(a.Account.Name), humanUnitState(states[runtimeManager.UnitName(a.Account.ID)]), viewPolicy(a))
		}
	case "admin":
		if s.admin == nil {
			fmt.Fprintln(out, "尚未配置应用管理员。")
		} else {
			fmt.Fprintf(out, "管理员名称：%s\n管理员身份：应用管理员\n登录密码：已隐藏\n", viewText(s.admin.Name))
		}
	case "endpoint", "url", "qr":
		n, err := s.node(nodeKey)
		if err != nil {
			return err
		}
		a, err := s.account(accountKey)
		if err != nil {
			return err
		}
		var data []byte
		for _, b := range s.bindings {
			if b.node == n.id && b.account == a.Account.ID {
				data = b.inbound
				break
			}
		}
		if data == nil {
			return errors.New("该账号未绑定所选节点，请返回列表刷新")
		}
		if operation == "endpoint" {
			fmt.Fprintf(out, "节点名称：%s\n所属账号：%s\n", viewText(n.name), viewText(a.Account.Name))
			return renderEndpoint(data, out)
		}
		if server == "" {
			return errors.New("请填写用于客户端连接的服务器公网地址")
		}
		uri, err := share.GenerateWithOptions(data, server, n.name, insecure)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, viewText(n.name))
		if operation == "url" {
			fmt.Fprintln(out, uri)
			return nil
		}
		binary, err := exec.LookPath("qrencode")
		if err != nil {
			return errors.New("尚未安装 qrencode，请使用分享链接")
		}
		command := exec.Command(binary, "-t", "ANSIUTF8")
		command.Stdin = strings.NewReader(uri)
		command.Stdout, command.Stderr = out, io.Discard
		return command.Run()
	}
	return nil
}

func (s viewSnapshot) choices(kind, nodeKey string, states map[string]string, out io.Writer) error {
	write := func(id, name, state string) {
		fmt.Fprintf(out, "%s\t%s\t%s\n", viewText(id), viewText(name), viewText(state))
	}
	switch kind {
	case "node":
		for _, n := range s.nodes {
			write(n.id, n.name, s.nodeState(n.id, states))
		}
	case "account":
		allowed := map[string]bool{}
		if nodeKey != "" {
			n, err := s.node(nodeKey)
			if err != nil {
				return err
			}
			for _, b := range s.nodeBindings(n.id) {
				allowed[b.account] = true
			}
		}
		for _, a := range s.accounts {
			if nodeKey != "" && !allowed[a.Account.ID] {
				continue
			}
			quota := "无限额"
			if !a.Unlimited {
				quota = humanBytes(a.Account.QuotaBytes)
			}
			state := fmt.Sprintf("%s；进程%s；%s / %s", viewPolicy(a), humanUnitState(states[runtimeManager.UnitName(a.Account.ID)]), humanBytes(a.Usage.Upload+a.Usage.Download), quota)
			write(a.Account.ID, a.Account.Name, state)
		}
	case "domain":
		for _, d := range s.domains {
			write(d, d, "精确匹配 → WARP；故障阻断")
		}
	}
	return nil
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func orUnknown(s string) string {
	if s == "" {
		return "未取得"
	}
	return viewText(s)
}

func viewInbound(data []byte) (map[string]any, error) {
	var root struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &root); err != nil || len(root.Inbounds) != 1 {
		return nil, errors.New("节点连接配置无效，请检查原始配置")
	}
	return root.Inbounds[0], nil
}

func viewProtocol(data []byte) string {
	in, err := viewInbound(data)
	if err != nil {
		return "未知"
	}
	protocol, _ := in["type"].(string)
	names := map[string]string{"hysteria2": "Hysteria2", "anytls": "AnyTLS", "vmess": "VMess", "vless": "VLESS", "tuic": "TUIC", "trojan": "Trojan", "shadowsocks": "Shadowsocks", "socks": "SOCKS"}
	if name := names[protocol]; name != "" {
		return name
	}
	return orUnknown(protocol)
}

// Explicit connection details use a whitelist: server private keys and the
// complete inbound JSON are never rendered, even on this privileged page.
func renderEndpoint(data []byte, out io.Writer) error {
	in, err := viewInbound(data)
	if err != nil {
		return err
	}
	field := func(label string, value any) {
		if value != nil && fmt.Sprint(value) != "" {
			fmt.Fprintf(out, "%s：%s\n", label, viewText(fmt.Sprint(value)))
		}
	}
	fmt.Fprintf(out, "协议类型：%s\n", viewProtocol(data))
	field("监听地址", in["listen"])
	field("监听端口", in["listen_port"])
	field("加密方式", in["method"])
	field("协议密码", in["password"])
	if users, ok := in["users"].([]any); ok {
		for _, value := range users {
			if user, ok := value.(map[string]any); ok {
				field("认证用户名", user["username"])
				field("认证 UUID", user["uuid"])
				field("认证密码", user["password"])
				field("协议流控", user["flow"])
			}
		}
	}
	if tls, ok := in["tls"].(map[string]any); ok {
		field("TLS 启用", yesNo(tls["enabled"] == true, "是", "否"))
		field("TLS 服务器名称", tls["server_name"])
		if alpn, ok := tls["alpn"].([]any); ok {
			values := []string{}
			for _, v := range alpn {
				values = append(values, fmt.Sprint(v))
			}
			field("ALPN", strings.Join(values, ", "))
		}
		if reality, ok := tls["reality"].(map[string]any); ok && reality["enabled"] == true {
			field("REALITY", "已启用；客户端公钥请查看分享链接")
		}
	}
	if transport, ok := in["transport"].(map[string]any); ok {
		field("传输类型", transport["type"])
		field("传输路径", transport["path"])
		field("gRPC 服务名", transport["service_name"])
	}
	if obfs, ok := in["obfs"].(map[string]any); ok {
		field("混淆方式", obfs["type"])
		field("混淆密码", obfs["password"])
	}
	fmt.Fprintln(out, "服务器地址请使用服务器公网 IP 或域名；本页已主动展开协议凭据，请妥善保存。")
	return nil
}
