package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/oldtian123/sing-box-enhanced/internal/warp"
)

// Package installation and network probes must not stall the quota meter lock.
func warpExternal(o options, operation string, out io.Writer) error {
	port := 40000
	accepted := false
	err := withApp(o, out, func(a *App) error {
		accepted = a.setting("warp_terms_accepted") == "1"
		if s := a.setting("warp_port"); s != "" {
			var e error
			port, e = strconv.Atoi(s)
			return e
		}
		return nil
	})
	if err != nil {
		return err
	}
	if operation == "install" {
		script := filepath.Join(env("SBE_SH_ROOT", filepath.Join(o.Root, "sh")), "scripts", "install-warp.sh")
		cmd := exec.Command("bash", script, strconv.Itoa(port))
		cmd.Env = append(os.Environ(), "SBE_ROOT="+o.Root)
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := warp.Client{Port: port, Binary: warpBinary(o.Root), AcceptTerms: accepted}
	switch operation {
	case "status":
		status, e := client.Status(ctx)
		_ = output(out, status)
		return e
	case "connect":
		return client.Connect(ctx)
	case "disconnect":
		return client.Disconnect(ctx)
	case "register":
		return client.Register(ctx)
	}
	return errors.New("unknown WARP external operation")
}

func warpBinary(root string) string {
	if path := os.Getenv("SBE_WARP_BIN"); path != "" {
		return path
	}
	path := filepath.Join(root, "warp", "bin", "warp-cli")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func (a *App) domains() ([]string, error) {
	var result []string
	if v := a.setting("warp_domains"); v != "" {
		if err := json.Unmarshal([]byte(v), &result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Validate every bound runtime candidate before changing the shared policy.
// Missing instance ports are planned in memory; preflight does not reserve them.
func (a *App) preflightWarp(domains []string) ([]string, error) {
	accounts, err := a.DB.Accounts()
	if err != nil {
		return nil, err
	}
	reserved := map[int]bool{}
	var healthy []string
	for _, account := range accounts {
		status, err := a.DB.Status(account.ID, time.Now())
		if err != nil {
			return nil, err
		}
		if status.Allowed {
			healthy = append(healthy, account.ID)
		}
		nodes, err := a.accountBindings(account.ID)
		if err != nil {
			return nil, err
		}
		if len(nodes) == 0 {
			continue
		}
		var port int
		err = a.DB.DB.QueryRow("SELECT stats_port FROM instances WHERE account_id=?", account.ID).Scan(&port)
		if errors.Is(err, sql.ErrNoRows) {
			if !status.Allowed {
				// Reconcile cannot start this account, so it must not consume a
				// prospective port ahead of an eligible account.
				continue
			}
			port, err = a.availableStatsPort(reserved)
		}
		if err != nil {
			return nil, err
		}
		reserved[port] = true
		data, err := a.renderAccountConfig(nodes, port, domains)
		if err == nil {
			err = a.Manager.CheckBytes(account.ID, data)
		}
		if err != nil {
			return nil, fmt.Errorf("preflight WARP configuration for account %s: %w", account.ID, err)
		}
	}
	return healthy, nil
}

func (a *App) warp(args []string) error {
	domains, err := a.domains()
	if err != nil {
		return err
	}
	port := 40000
	if s := a.setting("warp_port"); s != "" {
		port, err = strconv.Atoi(s)
		if err != nil {
			return err
		}
	}
	switch args[0] {
	case "accept-terms":
		if len(args) != 1 {
			return errors.New("warp accept-terms does not accept extra arguments")
		}
		if err := a.set("warp_terms_accepted", "1"); err != nil {
			return err
		}
		a.audit("Cloudflare WARP terms explicitly accepted by root")
		return output(a.Out, map[string]bool{"warp_terms_accepted": true})
	case "list":
		return output(a.Out, map[string]any{"domains": domains, "proxy_port": port, "scope": "all_managed_new_and_legacy_instances", "fallback": "blocked"})
	case "add", "remove":
		var domain string
		_, err = commandFlags("warp", args[1:], func(f *flag.FlagSet) { f.StringVar(&domain, "domain", "", "exact destination domain") })
		if err != nil {
			return err
		}
		domain, err = warp.ValidateDomain(domain)
		if err != nil {
			return err
		}
		found := false
		result := []string{}
		for _, v := range domains {
			if v == domain {
				found = true
				if args[0] == "remove" {
					continue
				}
			}
			result = append(result, v)
		}
		if args[0] == "add" && !found {
			result = append(result, domain)
		}
		sort.Strings(result)
		healthy, err := a.preflightWarp(result)
		if err != nil {
			return err
		}
		previous := a.setting("warp_domains")
		data, _ := json.Marshal(result)
		if err = a.set("warp_domains", string(data)); err != nil {
			return err
		}
		if err = a.reconcile(); err != nil {
			if restoreErr := a.set("warp_domains", previous); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous WARP rules: %w", restoreErr))
			}
			var failures []error
			for _, id := range healthy {
				// Reconcile respects current quotas, disabled state and StatsFault;
				// configuration rollback must never clear a metering failure.
				if restoreErr := a.reconcileAccount(id); restoreErr != nil {
					failures = append(failures, fmt.Errorf("restore account %s after WARP failure: %w", id, restoreErr))
				}
			}
			a.audit("warp domain change failed; previous rules restored")
			return errors.Join(append([]error{err}, failures...)...)
		}
		a.audit("warp domain " + args[0] + " " + domain)
		return nil
	default:
		return errors.New("unknown WARP operation")
	}
}
