package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	runtimeManager "github.com/oldtian123/sing-box-enhanced/internal/runtime"
	"github.com/oldtian123/sing-box-enhanced/internal/store"
)

const viewTestInbound = `{"inbounds":[{"type":"hysteria2","listen":"::","listen_port":25000,"users":[{"name":"example","password":"secret-protocol-value"}],"tls":{"enabled":true,"server_name":"tls","key_path":"PRIVATE-KEY-PATH"}}]}`

func TestViewQuotaBytesExactAndIndependent(t *testing.T) {
	for _, item := range []struct{ input, want string }{
		{"300", "300000000000"}, {"0", "0"}, {"1.5", "1500000000"},
		{"0.000000001", "1"}, {"9223372036.854775807", "9223372036854775807"},
	} {
		var out bytes.Buffer
		if err := runView(options{Root: "/absent-no-database"}, []string{"quota-bytes", "--gb", item.input}, &out); err != nil || strings.TrimSpace(out.String()) != item.want {
			t.Errorf("quota %q = %q, %v", item.input, out.String(), err)
		}
	}
	for _, input := range []string{"", "-1", "+1", "1e3", "1.", ".1", "1.0000000001", "9223372036.854775808", "9223372037", "18446744073709551616", "1\n"} {
		if _, err := quotaBytes(input); err == nil {
			t.Errorf("accepted invalid quota %q", input)
		}
	}
}

func TestViewAndBusinessResolvePreferStableIDOverOtherName(t *testing.T) {
	// The name collision comes first, as it can in a name-sorted menu snapshot.
	snapshot := viewSnapshot{
		nodes: []viewNode{{id: "n-other", name: "n-target"}, {id: "n-target", name: "target node"}},
		accounts: []store.AccountStatus{
			{Account: store.Account{ID: "a-other", Name: "a-target"}},
			{Account: store.Account{ID: "a-target", Name: "target account"}},
		},
	}
	if n, err := snapshot.node("n-target"); err != nil || n.id != "n-target" {
		t.Fatalf("node stable ID was shadowed: %+v, %v", n, err)
	}
	if a, err := snapshot.account("a-target"); err != nil || a.Account.ID != "a-target" {
		t.Fatalf("account stable ID was shadowed: %+v, %v", a, err)
	}
	if n, err := snapshot.node("target node"); err != nil || n.id != "n-target" {
		t.Fatalf("node name fallback broken: %+v, %v", n, err)
	}
	if a, err := snapshot.account("target account"); err != nil || a.Account.ID != "a-target" {
		t.Fatalf("account name fallback broken: %+v, %v", a, err)
	}

	f := newCLIFixture(t, false)
	f.create(t, "bootstrap", "1")
	s := f.open(t)
	for _, a := range snapshot.accounts {
		if err := s.CreateAccount(a.Account.ID, a.Account.Name, 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range snapshot.nodes {
		if _, err := s.DB.Exec("INSERT INTO nodes(id,name,template) VALUES(?,?,?)", n.id, n.name, []byte(viewTestInbound)); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{DB: s}
	for _, item := range []struct{ table, key, want string }{
		{"nodes", "n-target", "n-target"}, {"nodes", "target node", "n-target"},
		{"accounts", "a-target", "a-target"}, {"accounts", "target account", "a-target"},
	} {
		if got, err := a.resolve(item.table, item.key); err != nil || got != item.want {
			t.Errorf("resolve(%s,%s)=%s,%v; want %s", item.table, item.key, got, err, item.want)
		}
	}
}

func TestHumanTrafficUsesDecimalUnits(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.00 KB", 1_000_000: "1.00 MB", 300_000_000_000: "300.00 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("%d: %s, want %s", n, got, want)
		}
	}
}

func TestChoicesSanitizeAndFilterWithoutCredentials(t *testing.T) {
	s := viewSnapshot{
		accounts: []store.AccountStatus{
			{Account: store.Account{ID: "a-one", Name: "Alice\t\n\x1b[31m红\x1b[0m\u202e"}, Allowed: true},
			{Account: store.Account{ID: "a-two", Name: "Bob"}, Reason: "stats_fault"},
		},
		nodes:    []viewNode{{id: "n-one", name: "节点\t一\n\x1b]0;injection\x07", template: []byte(viewTestInbound)}, {id: "n-two", name: "节点二"}},
		bindings: []viewBinding{{account: "a-one", node: "n-one", inbound: []byte(viewTestInbound)}},
	}
	states := map[string]string{runtimeManager.UnitName("a-one"): "active"}
	var out bytes.Buffer
	if err := s.choices("account", "n-one", states, &out); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(rows) != 1 || len(strings.Split(rows[0], "\t")) != 3 || !strings.HasPrefix(rows[0], "a-one\t") || strings.ContainsAny(out.String(), "\x1b\u202e") || strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "Bob") {
		t.Fatalf("unsafe or unfiltered choices: %q", out.String())
	}
	out.Reset()
	if err := s.choices("node", "", states, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "injection") || strings.Contains(out.String(), "secret") {
		t.Fatal(out.String())
	}
	for _, row := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if len(strings.Split(row, "\t")) != 3 {
			t.Fatalf("TSV field split: %q", row)
		}
	}
}

func TestViewNodeAggregatesActualProcessesAndMasksSecrets(t *testing.T) {
	s := viewSnapshot{
		accounts: []store.AccountStatus{
			{Account: store.Account{ID: "a-one", Name: "Alice"}, Allowed: true},
			{Account: store.Account{ID: "a-two", Name: "Bob"}, Reason: "quota_exceeded"},
		},
		nodes:    []viewNode{{id: "n-one", name: "Hysteria2", template: []byte(viewTestInbound)}},
		bindings: []viewBinding{{account: "a-one", node: "n-one", inbound: []byte(viewTestInbound)}, {account: "a-two", node: "n-one", inbound: []byte(viewTestInbound)}},
	}
	states := map[string]string{runtimeManager.UnitName("a-one"): "active", runtimeManager.UnitName("a-two"): "inactive"}
	if got := s.nodeState("n-one", states); got != "部分运行" {
		t.Fatal(got)
	}
	var out bytes.Buffer
	if err := s.render("node", "", "n-one", "", "", false, states, nil, options{}, &out); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"部分运行", "Alice", "Bob", "当月流量已达限额", "已隐藏"} {
		if !strings.Contains(out.String(), required) {
			t.Fatalf("missing %s: %s", required, out.String())
		}
	}
	if strings.Contains(out.String(), "secret-protocol-value") || strings.Contains(out.String(), "PRIVATE-KEY") || strings.Contains(out.String(), "25000") {
		t.Fatalf("default node view leaked connection details: %s", out.String())
	}
	states[runtimeManager.UnitName("a-one")] = "failed"
	if got := s.nodeState("n-one", states); got != "运行故障" {
		t.Fatal(got)
	}
	delete(states, runtimeManager.UnitName("a-one"))
	if got := s.nodeState("n-one", states); got != "状态未知" {
		t.Fatal(got)
	}
	for state, want := range map[string]string{"activating": "启动中", "deactivating": "停止中", "reloading": "重新加载中", "not-found": "未创建实例"} {
		states[runtimeManager.UnitName("a-one")] = state
		states[runtimeManager.UnitName("a-two")] = state
		if got := s.nodeState("n-one", states); got != want {
			t.Errorf("%s => %s, want %s", state, got, want)
		}
	}
}

func TestViewEndpointOnlySelectedBinding(t *testing.T) {
	s := viewSnapshot{
		accounts: []store.AccountStatus{{Account: store.Account{ID: "a-one", Name: "Alice"}}},
		nodes:    []viewNode{{id: "n-one", name: "Selected"}, {id: "n-two", name: "Other"}},
		bindings: []viewBinding{
			{account: "a-one", node: "n-one", inbound: []byte(viewTestInbound)},
			{account: "a-one", node: "n-two", inbound: []byte(strings.ReplaceAll(viewTestInbound, "secret-protocol-value", "other-node-secret"))},
		},
	}
	var out bytes.Buffer
	if err := s.render("endpoint", "", "n-one", "a-one", "", false, nil, nil, options{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "secret-protocol-value") || strings.Contains(out.String(), "other-node-secret") || strings.Contains(out.String(), "PRIVATE-KEY") || strings.Contains(out.String(), `"inbounds"`) {
		t.Fatal(out.String())
	}
	s.bindings = s.bindings[1:]
	if err := s.render("endpoint", "", "n-one", "a-one", "", false, nil, nil, options{}, &out); err == nil {
		t.Fatal("accepted account without this node binding")
	}
}

func TestViewAccountShowsZeroQuotaAndUnlimitedSeparately(t *testing.T) {
	for _, unlimited := range []bool{false, true} {
		a := store.AccountStatus{Account: store.Account{ID: "a-one", Name: "Alice", QuotaBytes: 0, Unlimited: unlimited}, Unlimited: unlimited, Usage: store.Usage{Period: "2026-10"}, Reason: "stats_fault"}
		s := viewSnapshot{accounts: []store.AccountStatus{a}, paused: true}
		var out bytes.Buffer
		if err := s.render("account", "", "", "a-one", "", false, map[string]string{runtimeManager.UnitName("a-one"): "active"}, nil, options{}, &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"计量故障保护", "独立进程：运行中", "北京时间", "每月 1 日 00:00", "全局服务已暂停"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %s: %s", want, out.String())
			}
		}
		want := "月度限额：0 B"
		if unlimited {
			want = "月度限额：无限额"
		}
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
}

func TestViewsEmptyAndAdminDoNotLeakHash(t *testing.T) {
	s := viewSnapshot{port: 40000}
	for operation, want := range map[string]string{"nodes": "暂无节点", "rules": "尚未配置域名规则", "admin": "尚未配置应用管理员", "services": "暂无账号实例"} {
		var out bytes.Buffer
		if err := s.render(operation, "", "", "", "", false, nil, nil, options{}, &out); err != nil || !strings.Contains(out.String(), want) || json.Valid(out.Bytes()) {
			t.Fatalf("%s: %s, %v", operation, out.String(), err)
		}
	}
	s.admin = &store.Administrator{ID: "admin", Name: "应用管理员", Hash: "SECRET-HASH"}
	var out bytes.Buffer
	if err := s.render("admin", "", "", "", "", false, nil, nil, options{}, &out); err != nil || strings.Contains(out.String(), "SECRET-HASH") || !strings.Contains(out.String(), "应用管理员") {
		t.Fatalf("admin: %s, %v", out.String(), err)
	}
}

func TestViewDoesNotMutateLedgerOrExistingJSON(t *testing.T) {
	f := newCLIFixture(t, false)
	id := f.create(t, "Alice", "300000000000")
	s := f.open(t)
	if _, err := s.DB.Exec("INSERT INTO nodes(id,name,template) VALUES('n-one','Example',?)", []byte(viewTestInbound)); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM usage").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("unexpected fixture usage: %d", before)
	}
	data := f.mustRun(t, "view", "account", "--account", id)
	if json.Valid(data) || !strings.Contains(string(data), "300.00 GB") || !strings.Contains(string(data), "状态未知") {
		t.Fatal(string(data))
	}
	var after int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM usage").Scan(&after); err != nil || after != before {
		t.Fatalf("read-only view mutated ledger: %d -> %d, %v", before, after, err)
	}
	var nodeResult []map[string]any
	if err := json.Unmarshal(f.mustRun(t, "node", "list"), &nodeResult); err != nil || len(nodeResult) != 1 || nodeResult[0]["id"] != "n-one" {
		t.Fatalf("legacy JSON changed: %+v, %v", nodeResult, err)
	}
	var accounts []store.AccountStatus
	if err := json.Unmarshal(f.mustRun(t, "account", "list"), &accounts); err != nil || len(accounts) != 1 || accounts[0].Account.ID != id {
		t.Fatalf("account JSON changed: %+v, %v", accounts, err)
	}
	// The meter normally owns the ledger boundary; the read-only view leaves it
	// untouched even when no row has yet been created for the current month.
	if accounts[0].Usage.Period != store.Period(time.Now()) {
		t.Fatal(accounts[0].Usage.Period)
	}
}

func TestWarpViewPreservesPartialStatusAndProbesOutsideLock(t *testing.T) {
	f := newCLIFixture(t, false)
	f.create(t, "Alice", "300")
	bin := filepath.Join(filepath.Dir(f.core), "warp-cli")
	// Acquiring the management lock from the status process would fail if the
	// view kept it through this external probe.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIEW_WARP_HELPER_BINARY", binary)
	t.Setenv("VIEW_WARP_HELPER_ROOT", f.root)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in\nsettings) printf 'Mode: WarpProxy on port 40000\\n';;\nstatus) exec \"$VIEW_WARP_HELPER_BINARY\" -test.run=^TestViewWarpProbeLockHelper$;;\nesac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBE_WARP_BIN", bin)
	data, err := f.run("view", "warp")
	if err != nil || !strings.Contains(string(data), "已安装") || !strings.Contains(string(data), "检测失败") || !strings.Contains(string(data), "已保留") {
		t.Fatalf("partial WARP status lost: %s, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "probe-lock-acquired")); err != nil {
		t.Fatalf("WARP probe could not acquire management lock: %v", err)
	}
}

func TestViewWarpProbeLockHelper(t *testing.T) {
	root := os.Getenv("VIEW_WARP_HELPER_ROOT")
	if root == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(root, "manage.lock"), os.O_RDWR, 0)
	if err != nil {
		os.Exit(2)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(root, "probe-lock-acquired"), []byte("ok"), 0600); err != nil {
		os.Exit(4)
	}
	os.Exit(1) // Deliberately report a failed probe after proving lock release.
}
