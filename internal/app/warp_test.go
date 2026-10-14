package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeManager "github.com/oldtian123/sing-box-enhanced/internal/runtime"
)

func warpFixture(t *testing.T) (*App, cliFixture) {
	t.Helper()
	f := newCLIFixture(t, true)
	f.core = filepath.Join(filepath.Dir(f.root), "bin", "sing-box")
	t.Setenv("WARP_CORE_LOG", filepath.Join(filepath.Dir(f.root), "core-checks.log"))
	t.Setenv("WARP_CORE_COUNTS", filepath.Join(filepath.Dir(f.root), "checks"))
	t.Setenv("WARP_STOPPED", filepath.Join(filepath.Dir(f.root), "stopped"))
	t.Setenv("WARP_REJECT_ACCOUNT", "")
	t.Setenv("WARP_REJECT_AT", "")
	core := `#!/bin/sh
case "$3" in */alice/*) account=alice;; */bob/*) account=bob;; *) account=carol;; esac
printf '%s\n' "$account" >> "$WARP_CORE_LOG"
count=0
if [ -f "$WARP_CORE_COUNTS-$account" ]; then read -r count < "$WARP_CORE_COUNTS-$account"; fi
count=$((count + 1))
printf '%s\n' "$count" > "$WARP_CORE_COUNTS-$account"
if [ "$account" = "$WARP_REJECT_ACCOUNT" ] && { [ -z "$WARP_REJECT_AT" ] || [ "$count" = "$WARP_REJECT_AT" ]; }; then exit 17; fi
exit 0
`
	if err := os.WriteFile(f.core, []byte(core), 0700); err != nil {
		t.Fatal(err)
	}
	s := f.open(t)
	a := &App{options: options{Root: f.root, Core: f.core, UnitDir: f.unitDir}, DB: s,
		Manager: runtimeManager.Manager{Root: f.root, Core: f.core, UnitDir: f.unitDir}, Out: &bytes.Buffer{}}
	if err := a.set("warp_domains", `["old.example"]`); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"alice", "bob", "carol"} {
		if err := s.CreateAccount(id, id, 3000); err != nil {
			t.Fatal(err)
		}
		data := []byte(fmt.Sprintf(`{"inbounds":[{"type":"socks","tag":"%s","listen":"127.0.0.1","listen_port":%d,"users":[{"username":"fixture","password":"fixture"}]}]}`, id, 21001+index))
		if _, err := s.DB.Exec("INSERT INTO nodes(id,name,template) VALUES(?,?,?)", "n-"+id, "n-"+id, data); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec("INSERT INTO bindings(id,account_id,node_id,inbound) VALUES(?,?,?,?)", "b-"+id, id, "n-"+id, data); err != nil {
			t.Fatal(err)
		}
		config, err := a.renderAccountConfig([][]byte{data}, 31001+index, []string{"old.example"})
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(config)
		if _, err := s.DB.Exec("INSERT INTO instances(account_id,stats_port,generation,process_id,config_hash) VALUES(?,?,?,?,?)", id, 31001+index, "old-"+id, "old-"+id, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(f.root, "instances", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.unitDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.unitDir, runtimeManager.UnitName(id)), []byte("[Service]\nUser=root\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsurePeriod(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE usage SET upload=17,download=23"); err != nil {
		t.Fatal(err)
	}
	return a, f
}

func assertWarpLedger(t *testing.T, a *App) {
	t.Helper()
	accounts, err := a.DB.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		status, err := a.DB.Status(account.ID, time.Now())
		if err != nil || status.Usage.Upload != 17 || status.Usage.Download != 23 {
			t.Fatalf("WARP change modified account ledger: %+v, %v", status, err)
		}
	}
}

func TestWarpPreflightChecksEveryAccountBeforeSaveOrStop(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		for _, failure := range []string{"core", "render"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				a, f := warpFixture(t)
				previous := `["old.example","remove.example"]`
				if err := a.set("warp_domains", previous); err != nil {
					t.Fatal(err)
				}
				if failure == "core" {
					t.Setenv("WARP_REJECT_ACCOUNT", "bob")
				} else if err := a.set("warp_port", "31002"); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(filepath.Join(f.root, "instances", "alice", "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				domain := "new.example"
				if operation == "remove" {
					domain = "remove.example"
				}
				if err := a.warp([]string{operation, "--domain", domain}); err == nil || !strings.Contains(err.Error(), "account bob") {
					t.Fatalf("second account rejection was not reported: %v", err)
				}
				if a.setting("warp_domains") != previous {
					t.Fatal("preflight failure saved the shared WARP rules")
				}
				if data, err := os.ReadFile(f.log); !os.IsNotExist(err) {
					t.Fatalf("preflight called systemctl: %q, %v", data, err)
				}
				actual, err := os.ReadFile(filepath.Join(f.root, "instances", "alice", "config.json"))
				if err != nil || !bytes.Equal(actual, original) {
					t.Fatal("preflight replaced an earlier account's working config")
				}
				accounts, err := a.DB.Accounts()
				if err != nil {
					t.Fatal(err)
				}
				for _, account := range accounts {
					if account.StatsFault {
						t.Fatalf("preflight faulted account %s", account.ID)
					}
				}
				checks, _ := os.ReadFile(os.Getenv("WARP_CORE_LOG"))
				if !strings.HasPrefix(string(checks), "alice\n") || (failure == "core" && string(checks) != "alice\nbob\n") {
					t.Fatalf("preflight skipped the first account or the failing account: %q", checks)
				}
				assertWarpLedger(t, a)
			})
		}
	}
}

func TestWarpFailedApplyRestoresRulesWithoutClearingMeterFault(t *testing.T) {
	a, f := warpFixture(t)
	if err := a.DB.SetFault("carol", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("WARP_STOPPED")+"-sbe-account-carol.service", nil, 0600); err != nil {
		t.Fatal(err)
	}
	controller := `#!/bin/sh
printf '%s\n' "$*" >> "$APP_TEST_CTL_LOG"
case "$1" in
show)
  case "$2" in
  --property=LoadState) printf 'loaded\n';;
  --property=ActiveState) if [ -f "$WARP_STOPPED-$3" ]; then printf 'inactive\n'; else printf 'active\n'; fi;;
  --property=MainPID) printf '0\n';;
  *) exit 99;;
  esac;;
stop) : > "$WARP_STOPPED-$2";;
*) exit 99;;
esac
`
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.core), "systemctl"), []byte(controller), 0700); err != nil {
		t.Fatal(err)
	}
	// Bob passes global preflight, then rejects the candidate before its Stop.
	// Alice's separate ProcessID failure exercises a real metering protection.
	t.Setenv("WARP_REJECT_ACCOUNT", "bob")
	t.Setenv("WARP_REJECT_AT", "2")
	if err := a.warp([]string{"add", "--domain", "new.example"}); err == nil {
		t.Fatal("apply failures were hidden")
	}
	if a.setting("warp_domains") != `["old.example"]` {
		t.Fatal("apply failure did not restore the old rules")
	}
	for _, id := range []string{"alice", "carol"} {
		account, err := a.DB.Account(id)
		if err != nil || !account.StatsFault {
			t.Fatalf("rollback cleared a new or pre-existing meter fault for %s: %+v, %v", id, account, err)
		}
	}
	bob, err := a.DB.Account("bob")
	if err != nil || bob.StatsFault {
		t.Fatalf("healthy account was faulted: %+v, %v", bob, err)
	}
	calls, _ := os.ReadFile(f.log)
	if strings.Contains(string(calls), "stop sbe-account-bob.service") || strings.Contains(string(calls), "start ") {
		t.Fatalf("rollback interrupted the healthy account or started a faulted one: %s", calls)
	}
	checks, _ := os.ReadFile(os.Getenv("WARP_CORE_LOG"))
	if strings.Count(string(checks), "bob\n") != 2 {
		t.Fatalf("rollback did not recognize Bob's unchanged old config: %q", checks)
	}
	assertWarpLedger(t, a)
}

func TestWarpSaveFailureDoesNotTouchServices(t *testing.T) {
	a, f := warpFixture(t)
	if _, err := a.DB.DB.Exec(`CREATE TRIGGER reject_warp BEFORE INSERT ON settings WHEN NEW.key='warp_domains' BEGIN SELECT RAISE(ABORT,'fixture write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.warp([]string{"add", "--domain", "new.example"}); err == nil {
		t.Fatal("rule persistence failure was hidden")
	}
	if a.setting("warp_domains") != `["old.example"]` {
		t.Fatal("rejected rule write changed the old rules")
	}
	if calls, err := os.ReadFile(f.log); !os.IsNotExist(err) {
		t.Fatalf("failed rule write touched services: %q, %v", calls, err)
	}
	assertWarpLedger(t, a)
}

func TestWarpPreflightPlansMissingInstancePortsWithoutPersisting(t *testing.T) {
	a, f := warpFixture(t)
	if _, err := a.DB.DB.Exec("DELETE FROM instances"); err != nil {
		t.Fatal(err)
	}
	first, err := a.availableStatsPort(nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.availableStatsPort(map[int]bool{first: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.set("warp_port", fmt.Sprint(second)); err != nil {
		t.Fatal(err)
	}
	// Alice's prospective statistics port is safe, but Bob's would collide with
	// WARP. Reusing Alice's temporary port for every candidate would miss this.
	if err := a.warp([]string{"add", "--domain", "new.example"}); err == nil || !strings.Contains(err.Error(), "account bob") {
		t.Fatalf("planned statistics port conflict was not rejected: %v", err)
	}
	var count int
	if err := a.DB.DB.QueryRow("SELECT COUNT(*) FROM instances").Scan(&count); err != nil || count != 0 {
		t.Fatalf("preflight persisted provisional instances: %d, %v", count, err)
	}
	if a.setting("warp_domains") != `["old.example"]` {
		t.Fatal("port planning failure saved WARP rules")
	}
	if calls, err := os.ReadFile(f.log); !os.IsNotExist(err) {
		t.Fatalf("port planning touched services: %q, %v", calls, err)
	}
	assertWarpLedger(t, a)
}

func TestWarpPreflightDoesNotReservePortsForUninstantiatedDisabledAccounts(t *testing.T) {
	for _, reason := range []string{"disabled", "fault"} {
		t.Run(reason, func(t *testing.T) {
			a, f := warpFixture(t)
			if _, err := a.DB.DB.Exec("DELETE FROM instances"); err != nil {
				t.Fatal(err)
			}
			if reason == "disabled" {
				if err := a.DB.SetEnabled("alice", false); err != nil {
					t.Fatal(err)
				}
			} else if err := a.DB.SetFault("alice", true); err != nil {
				t.Fatal(err)
			}
			if err := a.DB.SetEnabled("carol", false); err != nil {
				t.Fatal(err)
			}
			first, err := a.availableStatsPort(nil)
			if err != nil {
				t.Fatal(err)
			}
			second, err := a.availableStatsPort(map[int]bool{first: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.set("warp_port", fmt.Sprint(second)); err != nil {
				t.Fatal(err)
			}
			healthy, err := a.preflightWarp([]string{"new.example"})
			if err != nil || len(healthy) != 1 || healthy[0] != "bob" {
				t.Fatalf("inactive account consumed Bob's valid prospective port: %v, %v", healthy, err)
			}
			checks, _ := os.ReadFile(os.Getenv("WARP_CORE_LOG"))
			if string(checks) != "bob\n" {
				t.Fatalf("unmaterialized inactive account received a runtime candidate: %q", checks)
			}
			var count int
			if err := a.DB.DB.QueryRow("SELECT COUNT(*) FROM instances").Scan(&count); err != nil || count != 0 {
				t.Fatalf("preflight persisted provisional instances: %d, %v", count, err)
			}
			if calls, err := os.ReadFile(f.log); !os.IsNotExist(err) {
				t.Fatalf("preflight touched services: %q, %v", calls, err)
			}
			assertWarpLedger(t, a)
		})
	}
}

func TestReconcileThirdCheckApplyFailureIsNotAMeterFault(t *testing.T) {
	a, f := warpFixture(t)
	if _, err := a.DB.DB.Exec("UPDATE instances SET process_id='' WHERE account_id='bob'"); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(f.root, "instances", "bob", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_REJECT_ACCOUNT", "bob")
	t.Setenv("WARP_REJECT_AT", "3")
	if _, err := a.preflightWarp([]string{"new.example"}); err != nil {
		t.Fatal(err)
	}
	if err := a.set("warp_domains", `["new.example"]`); err != nil {
		t.Fatal(err)
	}
	// The stopped, clean process identity models the state after a successful
	// planned Stop. Both outer checks pass; Manager.Apply's own check fails.
	if err := a.reconcileAccount("bob"); err == nil || !strings.Contains(err.Error(), "before start") {
		t.Fatalf("actual Apply validation failure was not reported: %v", err)
	}
	account, err := a.DB.Account("bob")
	if err != nil || account.StatsFault {
		t.Fatalf("configuration failure became a meter fault: %+v, %v", account, err)
	}
	actual, err := os.ReadFile(filepath.Join(f.root, "instances", "bob", "config.json"))
	if err != nil || !bytes.Equal(original, actual) {
		t.Fatal("failed Apply replaced the old runtime configuration")
	}
	calls, _ := os.ReadFile(f.log)
	if strings.Contains(string(calls), "start ") {
		t.Fatalf("failed Apply started a core: %s", calls)
	}
	checks, _ := os.ReadFile(os.Getenv("WARP_CORE_LOG"))
	if strings.Count(string(checks), "bob\n") != 3 {
		t.Fatalf("test did not reach Manager.Apply's third check: %q", checks)
	}
	assertWarpLedger(t, a)
}
