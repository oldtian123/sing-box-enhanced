package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oldtian123/sing-box-enhanced/internal/config"
	"github.com/oldtian123/sing-box-enhanced/internal/meter"
	runtimeManager "github.com/oldtian123/sing-box-enhanced/internal/runtime"
	"github.com/oldtian123/sing-box-enhanced/internal/store"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type options struct{ Root, Core, UnitDir string }
type App struct {
	options
	DB      *store.Store
	Manager runtimeManager.Manager
	Out     io.Writer
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func randomID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func output(w io.Writer, value any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(value)
}
func commandFlags(name string, args []string, configure func(*flag.FlagSet)) (*flag.FlagSet, error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	configure(f)
	if err := f.Parse(args); err != nil {
		return nil, err
	}
	if f.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", f.Args())
	}
	return f, nil
}

func Run(args []string, out io.Writer) error {
	o := options{}
	f := flag.NewFlagSet("sbe", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&o.Root, "root", env("SBE_ROOT", "/etc/sing-box-enhanced"), "private state directory")
	f.StringVar(&o.Core, "core", env("SBE_CORE", "/etc/sing-box-enhanced/bin/sing-box"), "statistics-enabled sing-box")
	f.StringVar(&o.UnitDir, "unit-dir", env("SBE_UNIT_DIR", "/etc/systemd/system"), "systemd unit directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	args = f.Args()
	if len(args) == 0 || args[0] == "help" {
		_, err := fmt.Fprintln(out, helpText)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(out, "sing-box-enhanced 0.1.0-dev")
		return err
	}
	for _, p := range []string{o.Root, o.Core, o.UnitDir} {
		if !filepath.IsAbs(p) {
			return errors.New("root, core and unit-dir must be absolute paths")
		}
	}
	if len(args) > 1 && args[0] == "service" && args[1] == "protect" {
		return protect(o)
	}
	if args[0] == "daemon" {
		return daemon(o, out)
	}
	if args[0] == "admin" {
		return runAdmin(o, args[1:], out, os.Stdin)
	}
	if args[0] == "view" {
		return runView(o, args[1:], out)
	}
	if args[0] == warpJobCommand {
		return runWARPJobWorker(o, args[1:])
	}
	if len(args) >= 2 && args[0] == "warp" {
		switch args[1] {
		case "add", "remove":
			return runWARPJob(o, args[1:], out)
		case "register":
			accepted := false
			if _, err := commandFlags("warp register", args[2:], func(f *flag.FlagSet) {
				f.BoolVar(&accepted, "accept-terms", false, "explicitly accept Cloudflare WARP terms")
			}); err != nil {
				return err
			}
			if !accepted {
				return errors.New("warp register requires explicit --accept-terms after reviewing https://www.cloudflare.com/application/terms/")
			}
			if err := withApp(o, out, func(a *App) error { return a.warp([]string{"accept-terms"}) }); err != nil {
				return err
			}
			return warpExternal(o, "register", out)
		case "install", "status", "connect", "disconnect":
			if len(args) != 2 {
				return errors.New("WARP external operation accepts no additional arguments")
			}
			return warpExternal(o, args[1], out)
		}
	}
	return withApp(o, out, func(a *App) error { return a.dispatch(args) })
}

func withApp(o options, out io.Writer, fn func(*App) error) error {
	if err := os.MkdirAll(o.Root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(o.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("root directory must be private (0700) and not a symlink")
	}
	lock, err := os.OpenFile(filepath.Join(o.Root, "manage.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := store.Open(filepath.Join(o.Root, "state.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	a := &App{options: o, DB: s, Manager: runtimeManager.Manager{Root: o.Root, Core: o.Core, UnitDir: o.UnitDir}, Out: out}
	return fn(a)
}

func (a *App) setting(key string) string {
	var v string
	_ = a.DB.DB.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}
func (a *App) set(key, value string) error {
	_, e := a.DB.DB.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return e
}
func (a *App) audit(event string) {
	_, _ = a.DB.DB.Exec("INSERT INTO operations(event,created_at) VALUES(?,?)", event, time.Now().UTC().Format(time.RFC3339Nano))
}
func (a *App) resolve(table, key string) (string, error) {
	if table != "accounts" && table != "nodes" {
		return "", errors.New("invalid lookup")
	}
	var id string
	err := a.DB.DB.QueryRow("SELECT id FROM "+table+" WHERE id=?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = a.DB.DB.QueryRow("SELECT id FROM "+table+" WHERE name=?", key).Scan(&id)
	}
	return id, err
}
func (a *App) dispatch(args []string) error {
	if len(args) < 2 {
		return errors.New("missing subcommand; run sb enhanced help")
	}
	switch args[0] {
	case "account", "quota":
		return a.account(args[1:])
	case "node":
		return a.node(args[1:])
	case "warp":
		return a.warp(args[1:])
	case "migrate":
		return a.migrate(args[1:])
	case "service":
		if args[1] == "log" {
			return a.logs(args[2:])
		}
		if len(args) != 2 {
			return errors.New("service operation accepts no additional arguments")
		}
		switch args[1] {
		case "status":
			accounts, err := a.DB.Accounts()
			if err != nil {
				return err
			}
			statuses := make([]store.AccountStatus, 0, len(accounts))
			for _, v := range accounts {
				s, e := a.DB.Status(v.ID, time.Now())
				if e != nil {
					return e
				}
				statuses = append(statuses, s)
			}
			return output(a.Out, map[string]any{"accounts": statuses, "paused": a.setting("paused") == "1", "period": store.Period(time.Now())})
		case "stop":
			if err := a.set("paused", "1"); err != nil {
				return err
			}
			return a.reconcile()
		case "start":
			if err := a.set("paused", "0"); err != nil {
				return err
			}
			return a.reconcile()
		case "restart":
			if err := a.stopAll(false); err != nil {
				return err
			}
			return a.reconcile()
		case "reconcile":
			return a.reconcile()
		default:
			return errors.New("unknown service operation")
		}
	default:
		return errors.New("unknown command")
	}
}

func (a *App) account(args []string) error {
	if args[0] == "list" {
		accounts, err := a.DB.Accounts()
		if err != nil {
			return err
		}
		result := make([]store.AccountStatus, 0, len(accounts))
		for _, v := range accounts {
			s, e := a.DB.Status(v.ID, time.Now())
			if e != nil {
				return e
			}
			result = append(result, s)
		}
		return output(a.Out, result)
	}
	var name, id string
	var quota int64 = -1
	unlimited := false
	flags, err := commandFlags("account", args[1:], func(f *flag.FlagSet) {
		f.StringVar(&name, "name", "", "account name")
		f.StringVar(&id, "id", "", "account name or ID")
		f.Int64Var(&quota, "bytes", -1, "monthly byte limit")
		f.BoolVar(&unlimited, "unlimited", false, "record traffic without a quota limit")
	})
	if err != nil {
		return err
	}
	quotaProvided := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "bytes" {
			quotaProvided = true
		}
	})
	if args[0] == "create" {
		if name == "" || (!unlimited && quota < 0) || (unlimited && quotaProvided) {
			return errors.New("account create requires --name and exactly one of --bytes or --unlimited")
		}
		id = randomID("a-")
		if unlimited {
			err = a.DB.CreateUnlimitedAccount(id, name)
		} else {
			err = a.DB.CreateAccount(id, name, quota)
		}
		if err != nil {
			return err
		}
		a.audit("account created " + id)
		return output(a.Out, map[string]string{"id": id, "name": name})
	}
	if id == "" {
		id = name
	}
	if id == "" {
		return errors.New("--id is required")
	}
	id, err = a.resolve("accounts", id)
	if err != nil {
		return err
	}
	switch args[0] {
	case "enable":
		err = a.DB.SetEnabled(id, true)
	case "disable":
		err = a.DB.SetEnabled(id, false)
	case "quota", "set":
		if (!unlimited && quota < 0) || (unlimited && quotaProvided) {
			return errors.New("exactly one of --bytes or --unlimited is required")
		}
		if unlimited {
			err = a.DB.SetUnlimited(id)
		} else {
			err = a.DB.SetQuota(id, quota)
		}
	case "rename":
		if strings.TrimSpace(name) == "" {
			return errors.New("--name is required")
		}
		_, err = a.DB.DB.Exec("UPDATE accounts SET name=? WHERE id=?", name, id)
	case "reset":
		if err = a.collect(id); err != nil {
			return err
		}
		err = a.DB.ResetUsage(id, time.Now())
	case "recover":
		running, e := a.Manager.Running(id)
		if e != nil {
			return e
		}
		if running {
			return errors.New("stop the faulty account before recovery")
		}
		err = a.DB.SetFault(id, false)
		if err == nil {
			_, err = a.DB.DB.Exec("UPDATE instances SET process_id='' WHERE account_id=?", id)
		}
	case "delete":
		if err = a.stopAccount(id, false); err != nil {
			return err
		}
		_, err = a.DB.DB.Exec("DELETE FROM accounts WHERE id=?", id)
	default:
		return errors.New("unknown account operation")
	}
	if err != nil {
		return err
	}
	a.audit("account " + args[0] + " " + id)
	return a.reconcile()
}

func (a *App) node(args []string) error {
	if args[0] == "url" || args[0] == "qr" {
		return a.nodeShare(args[0], args[1:])
	}
	if args[0] == "list" {
		rows, err := a.DB.DB.Query("SELECT id,name,legacy,(SELECT COUNT(*) FROM bindings WHERE node_id=nodes.id) FROM nodes ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var id, name string
			var legacy bool
			var bindings int
			if err = rows.Scan(&id, &name, &legacy, &bindings); err != nil {
				return err
			}
			result = append(result, map[string]any{"id": id, "name": name, "legacy": legacy, "bindings": bindings})
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return output(a.Out, result)
	}
	var file, name, nodeID, accountID string
	var port int
	_, err := commandFlags("node", args[1:], func(f *flag.FlagSet) {
		f.StringVar(&file, "template", "", "node JSON file")
		f.StringVar(&name, "name", "", "logical node name")
		f.StringVar(&nodeID, "node", "", "node name or ID")
		f.StringVar(&accountID, "account", "", "account name or ID")
		f.IntVar(&port, "port", 0, "dedicated port")
	})
	if err != nil {
		return err
	}
	if args[0] == "add" {
		if file == "" || name == "" {
			return errors.New("node add requires --template and --name")
		}
		data, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		if e = config.ValidateIdentity(data); e != nil {
			return e
		}
		nodeID = randomID("n-")
		_, err = a.DB.DB.Exec("INSERT INTO nodes(id,name,template,legacy) VALUES(?,?,?,0)", nodeID, name, data)
		if err != nil {
			return err
		}
		a.audit("node created " + nodeID)
		return output(a.Out, map[string]string{"id": nodeID, "name": name, "state": "unbound"})
	}
	if accountID != "" {
		accountID, err = a.resolve("accounts", accountID)
		if err != nil {
			return err
		}
	}
	if nodeID != "" {
		nodeID, err = a.resolve("nodes", nodeID)
		if err != nil {
			return err
		}
	}
	if args[0] == "endpoints" {
		if accountID == "" {
			return errors.New("--account is required")
		}
		rows, e := a.DB.DB.Query("SELECT nodes.name,bindings.inbound FROM bindings JOIN nodes ON nodes.id=bindings.node_id WHERE account_id=?", accountID)
		if e != nil {
			return e
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var n string
			var data []byte
			if e = rows.Scan(&n, &data); e != nil {
				return e
			}
			result = append(result, map[string]any{"name": n, "configuration": json.RawMessage(data)})
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return output(a.Out, result)
	}
	if nodeID == "" {
		return errors.New("--node is required")
	}
	switch args[0] {
	case "bind":
		if accountID == "" {
			return errors.New("--account is required")
		}
		var data []byte
		var legacy bool
		if err = a.DB.DB.QueryRow("SELECT template,legacy FROM nodes WHERE id=?", nodeID).Scan(&data, &legacy); err != nil {
			return err
		}
		binding := randomID("b-")
		if legacy {
			var count int
			if err = a.DB.DB.QueryRow("SELECT COUNT(*) FROM bindings WHERE node_id=?", nodeID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("legacy identity already belongs to an account")
			}
			if port != 0 {
				return errors.New("legacy binding preserves its existing port")
			}
			if err = config.ValidateIdentity(data); err != nil {
				return err
			}
		} else {
			if port == 0 {
				port, err = a.bindingPort(20000)
				if err != nil {
					return err
				}
			}
			if err = a.checkBindingPort(port); err != nil {
				return err
			}
			data, err = config.Derive(data, accountID, binding, port)
			if err != nil {
				return err
			}
		}
		if !legacy && config.FrontendRequired(data) {
			if err = a.addFrontend(binding, nodeID, data); err != nil {
				return err
			}
		}
		_, err = a.DB.DB.Exec("INSERT INTO bindings(id,account_id,node_id,inbound) VALUES(?,?,?,?)", binding, accountID, nodeID, data)
	case "unbind":
		if accountID == "" {
			return errors.New("--account is required")
		}
		if err = a.collect(accountID); err != nil {
			return err
		}
		_, err = a.DB.DB.Exec("DELETE FROM bindings WHERE account_id=? AND node_id=?", accountID, nodeID)
	case "delete":
		rows, e := a.DB.DB.Query("SELECT account_id FROM bindings WHERE node_id=?", nodeID)
		if e != nil {
			return e
		}
		ids := []string{}
		for rows.Next() {
			var v string
			if e = rows.Scan(&v); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, v)
		}
		rows.Close()
		for _, id := range ids {
			if e = a.collect(id); e != nil {
				return e
			}
		}
		_, err = a.DB.DB.Exec("DELETE FROM nodes WHERE id=?", nodeID)
	default:
		return errors.New("unknown node operation")
	}
	if err != nil {
		return err
	}
	a.audit("node " + args[0] + " " + nodeID)
	return a.reconcile()
}

func freePort(start int) (int, error) {
	for p := start; p < 65000; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			continue
		}
		u, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", p))
		l.Close()
		if err == nil {
			u.Close()
			return p, nil
		}
	}
	return 0, errors.New("no free listener port")
}

// Reserve configured ports too, including accounts that are currently stopped.
func (a *App) checkBindingPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("invalid binding port")
	}
	rows, err := a.DB.DB.Query("SELECT inbound FROM bindings")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return err
		}
		var value struct {
			Inbounds []struct {
				Port int `json:"listen_port"`
			} `json:"inbounds"`
		}
		if err = json.Unmarshal(data, &value); err != nil {
			return err
		}
		for _, in := range value.Inbounds {
			if in.Port == port {
				return errors.New("port is already reserved by another binding")
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return errors.New("binding port is already in use")
	}
	defer l.Close()
	u, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return errors.New("binding port is already in use")
	}
	defer u.Close()
	return nil
}
func (a *App) bindingPort(start int) (int, error) {
	for port := start; port < 65000; port++ {
		if a.checkBindingPort(port) == nil {
			return port, nil
		}
	}
	return 0, errors.New("no unreserved binding port")
}

func (a *App) collect(id string) error {
	running, err := a.Manager.Running(id)
	if err != nil {
		return a.protectAccount(id, err)
	}
	if !running {
		return nil
	}
	var port int
	var generation, expected string
	if err = a.DB.DB.QueryRow("SELECT stats_port,generation,process_id FROM instances WHERE account_id=?", id).Scan(&port, &generation, &expected); err != nil {
		return a.protectAccount(id, err)
	}
	actual, err := a.Manager.ProcessID(id)
	if err != nil {
		return a.protectAccount(id, err)
	}
	if expected == "" || actual != expected {
		return a.protectAccount(id, errors.New("unexpected process generation; account protected"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counters, err := meter.Query(ctx, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return a.protectAccount(id, fmt.Errorf("statistics unavailable for %s: %w", id, err))
	}
	// Do not charge a sample to the previous process if it changed during RPC.
	after, err := a.Manager.ProcessID(id)
	if err != nil || after != actual {
		return a.protectAccount(id, errors.New("process changed during statistics query; account protected"))
	}
	status, err := a.DB.ApplySample(id, generation, counters, time.Now())
	if err != nil {
		return a.protectAccount(id, err)
	}
	if !status.Allowed {
		return a.stopAccount(id, false)
	}
	return nil
}

func (a *App) protectAccount(id string, cause error) error {
	faultErr := a.DB.SetFault(id, true)
	stopErr := a.Manager.Stop(id)
	var clearErr error
	if faultErr == nil && stopErr == nil {
		_, clearErr = a.DB.DB.Exec("UPDATE instances SET process_id='' WHERE account_id=?", id)
	}
	return errors.Join(cause, faultErr, stopErr, clearErr)
}

func (a *App) stopAccount(id string, flush bool) error {
	var flushErr error
	if flush {
		flushErr = a.collect(id)
	}
	if err := a.Manager.Stop(id); err != nil {
		return errors.Join(flushErr, err)
	}
	if flushErr != nil {
		return flushErr
	}
	_, err := a.DB.DB.Exec("UPDATE instances SET process_id='' WHERE account_id=?", id)
	return errors.Join(flushErr, err)
}
func (a *App) stopAll(fault bool) error {
	accounts, err := a.DB.Accounts()
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range accounts {
		if fault {
			if e := a.DB.SetFault(v.ID, true); e != nil {
				errs = append(errs, e)
			}
		}
		if e := a.stopAccount(v.ID, !fault); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

func (a *App) reconcile() error {
	if err := a.DB.EnsurePeriod(time.Now()); err != nil {
		return errors.Join(err, protect(a.options))
	}
	accounts, err := a.DB.Accounts()
	if err != nil {
		return errors.Join(err, protect(a.options))
	}
	var failures []error
	for _, v := range accounts {
		if e := a.reconcileAccount(v.ID); e != nil {
			failures = append(failures, fmt.Errorf("account %s: %w", v.ID, e))
		}
	}
	return errors.Join(failures...)
}

func (a *App) accountBindings(id string) ([][]byte, error) {
	rows, err := a.DB.DB.Query("SELECT inbound FROM bindings WHERE account_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := [][]byte{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		nodes = append(nodes, b)
	}
	return nodes, rows.Err()
}

func (a *App) availableStatsPort(reserved map[int]bool) (int, error) {
	port, err := freePort(30000)
	for err == nil {
		var used int
		if err = a.DB.DB.QueryRow("SELECT COUNT(*) FROM instances WHERE stats_port=?", port).Scan(&used); err != nil {
			return 0, err
		}
		if used == 0 && !reserved[port] {
			return port, nil
		}
		port, err = freePort(port + 1)
	}
	return 0, err
}

func (a *App) renderAccountConfig(nodes [][]byte, port int, domains []string) ([]byte, error) {
	base := []byte(`{"log":{"level":"warn","timestamp":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if b, e := os.ReadFile(filepath.Join(a.Root, "base.json")); e == nil {
		base = b
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	warpPort := 40000
	if p := a.setting("warp_port"); p != "" {
		var err error
		warpPort, err = strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
	}
	return config.Render(base, nodes, port, domains, warpPort)
}

func (a *App) reconcileAccount(id string) error {
	status, err := a.DB.Status(id, time.Now())
	if err != nil {
		return a.protectAccount(id, err)
	}
	if !status.Allowed || a.setting("paused") == "1" {
		return a.stopAccount(id, true)
	}
	nodes, err := a.accountBindings(id)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return a.stopAccount(id, true)
	}
	var port int
	var generation, processID, oldHash string
	err = a.DB.DB.QueryRow("SELECT stats_port,generation,process_id,config_hash FROM instances WHERE account_id=?", id).Scan(&port, &generation, &processID, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		port, err = a.availableStatsPort(nil)
		if err != nil {
			return err
		}
		generation = randomID("g-")
		_, err = a.DB.DB.Exec("INSERT INTO instances(account_id,stats_port,generation) VALUES(?,?,?)", id, port, generation)
	}
	if err != nil {
		return err
	}
	domains, err := a.domains()
	if err != nil {
		return err
	}
	data, err := a.renderAccountConfig(nodes, port, domains)
	if err != nil {
		_ = a.DB.SetFault(id, true)
		_ = a.stopAccount(id, false)
		return err
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	running, err := a.Manager.Running(id)
	if err != nil {
		return err
	}
	if running && oldHash == hash {
		return nil
	}
	if !running && processID != "" {
		_ = a.DB.SetFault(id, true)
		return errors.New("account core exited unexpectedly; recover explicitly after checking metering")
	}
	if err = a.Manager.CheckBytes(id, data); err != nil {
		return err
	}
	if running {
		if err = a.collect(id); err != nil {
			return err
		}
		status, err = a.DB.Status(id, time.Now())
		if err != nil {
			return err
		}
		if !status.Allowed {
			return nil
		}
		if err = a.stopAccount(id, false); err != nil {
			return err
		}
	}
	if _, err = a.Manager.Apply(id, data); err != nil {
		// Metering and the planned Stop already succeeded. No new process has
		// started, so a configuration failure must not become a meter fault;
		// the caller can restore the prior policy with a fresh generation.
		return fmt.Errorf("apply configuration for account %s before start: %w", id, err)
	}
	generation = randomID("g-")
	if _, err = a.DB.DB.Exec("UPDATE instances SET generation=?,process_id='',config_hash=? WHERE account_id=?", generation, hash, id); err != nil {
		return err
	}
	if err = a.Manager.Start(id); err != nil {
		_ = a.DB.SetFault(id, true)
		return err
	}
	processID, err = a.Manager.ProcessID(id)
	if err != nil || processID == "" {
		_ = a.Manager.Stop(id)
		_ = a.DB.SetFault(id, true)
		if err == nil {
			err = errors.New("core started without a process identity")
		}
		return err
	}
	_, err = a.DB.DB.Exec("UPDATE instances SET process_id=? WHERE account_id=?", processID, id)
	if err != nil {
		_ = a.Manager.Stop(id)
	}
	return err
}

func daemon(o options, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("daemon requires root")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		err := withApp(o, out, func(a *App) error {
			if e := a.DB.EnsurePeriod(time.Now()); e != nil {
				_ = a.stopAll(true)
				return e
			}
			accounts, e := a.DB.Accounts()
			if e != nil {
				return e
			}
			var errs []error
			for _, v := range accounts {
				if e = a.collect(v.ID); e != nil {
					errs = append(errs, e)
				}
			}
			if e = a.reconcile(); e != nil {
				errs = append(errs, e)
			}
			return errors.Join(errs...)
		})
		if err != nil {
			fmt.Fprintln(out, time.Now().UTC().Format(time.RFC3339), err)
			if e := protectOnDatabaseFailure(o); e != nil {
				fmt.Fprintln(out, "protection:", e)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Independently supervise core access if the accounting database is unreadable.
func protectOnDatabaseFailure(o options) error {
	s, e := store.Open(filepath.Join(o.Root, "state.db"))
	if e == nil {
		e = s.EnsurePeriod(time.Now())
		if e == nil {
			var accounts []store.Account
			accounts, e = s.Accounts()
			if e == nil {
				for _, account := range accounts {
					if _, e = s.Status(account.ID, time.Now()); e != nil {
						break
					}
				}
			}
		}
		s.Close()
	}
	if e == nil {
		return nil
	}
	return protect(o)
}
func protect(o options) error {
	m := runtimeManager.Manager{Root: o.Root, Core: o.Core, UnitDir: o.UnitDir}
	entries, err := os.ReadDir(filepath.Join(o.Root, "instances"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() && validID.MatchString(entry.Name()) {
			if e := m.Stop(entry.Name()); e != nil {
				errs = append(errs, e)
			}
		}
	}
	if s, e := store.Open(filepath.Join(o.Root, "state.db")); e == nil {
		accounts, e := s.Accounts()
		if e == nil {
			for _, v := range accounts {
				if e = s.SetFault(v.ID, true); e != nil {
					errs = append(errs, e)
					continue
				}
				if _, e = s.DB.Exec("UPDATE instances SET process_id='' WHERE account_id=?", v.ID); e != nil {
					errs = append(errs, e)
				}
			}
		} else {
			errs = append(errs, e)
		}
		s.Close()
	}
	return errors.Join(errs...)
}

const helpText = `233Boy sing-box Enhanced (开发版，GPL-3.0)
sb admin setup [--name admin] [--password-stdin] （首次配置应用管理员）
sb admin show | rename --name NEW_NAME | passwd [--password-stdin]
管理员密码用于后续面板登录；不传密码时生成随机密码，凭据保存在 root 私有文件。
sb account create --name NAME --bytes MONTHLY_BYTES
sb account create --name NAME --unlimited
sb account list | enable/disable/quota/reset/recover/delete --id NAME_OR_ID [--bytes N]
sb account quota --id NAME_OR_ID --unlimited
sb account rename --id NAME_OR_ID --name NEW_NAME
sb node add --name NAME --template NODE_JSON
sb node list | bind/unbind --node NAME_OR_ID --account NAME_OR_ID [--port PORT]
sb node endpoints --account NAME_OR_ID  （包含该账号凭据，仅本机 root 查看）
sb node delete --node NAME_OR_ID
sb node url/qr --account NAME_OR_ID --server HOST[:PORT] [--node NAME_OR_ID] [--insecure]
sb warp status | list | add/remove --domain DOMAIN | connect | disconnect | install
sb warp register --accept-terms | accept-terms （明确接受 Cloudflare 条款）
sb migrate inspect/import --legacy-root PATH [--owner ACCOUNT]
sb migrate apply [--takeover-entry PATH] | rollback （仅 SBE_DEVELOPER=1）
sb migrate run --legacy-root PATH --owner ACCOUNT [--takeover-entry PATH]
sb service status | reconcile | start | stop | restart
sb service log [--account NAME_OR_ID] [--tail N]
自然月按北京时间；上传下载合计；账号隔离进程。计量故障需 account recover。
统计为转发负载，核心异常崩溃可能丢失尚未采集尾量；轮询并非零超额硬计费。
账号订阅、Agent/API 和 Dashboard 尚未实现。`
