package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimeManager "github.com/oldtian123/sing-box-enhanced/internal/runtime"
	"github.com/oldtian123/sing-box-enhanced/internal/store"
)

type cliFixture struct {
	root    string
	core    string
	unitDir string
	log     string
}

func newCLIFixture(t *testing.T, fakeSystemd bool) cliFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	f := cliFixture{root: filepath.Join(dir, "state"), core: filepath.Join(bin, "absent-core"), unitDir: filepath.Join(dir, "units"), log: filepath.Join(dir, "systemctl.log")}
	// This PATH contains no host utilities, so tests cannot touch real services.
	t.Setenv("PATH", bin)
	if fakeSystemd {
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$APP_TEST_CTL_LOG\"\ncase \"$1\" in\nshow) printf 'inactive\\n';;\nstop) exit 0;;\n*) printf 'unexpected service operation\\n' >&2; exit 99;;\nesac\n"
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("APP_TEST_CTL_LOG", f.log)
	}
	return f
}

func (f cliFixture) run(args ...string) ([]byte, error) {
	var output bytes.Buffer
	all := []string{"--root", f.root, "--core", f.core, "--unit-dir", f.unitDir}
	err := Run(append(all, args...), &output)
	return output.Bytes(), err
}

func (f cliFixture) mustRun(t *testing.T, args ...string) []byte {
	t.Helper()
	output, err := f.run(args...)
	if err != nil {
		t.Fatalf("CLI %v: %v", args, err)
	}
	return output
}

func (f cliFixture) create(t *testing.T, name string, quota string) string {
	t.Helper()
	var account map[string]string
	if err := json.Unmarshal(f.mustRun(t, "account", "create", "--name", name, "--bytes", quota), &account); err != nil {
		t.Fatal(err)
	}
	if account["id"] == "" || account["name"] != name {
		t.Fatalf("invalid account create result: %v", account)
	}
	return account["id"]
}

func (f cliFixture) open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(f.root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUnboundAccountCLIRequiresNoSystemd(t *testing.T) {
	for _, operation := range []string{"quota", "reset", "disable", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newCLIFixture(t, false)
			id := f.create(t, "alice", "1000")
			var statuses []store.AccountStatus
			if err := json.Unmarshal(f.mustRun(t, "account", "list"), &statuses); err != nil || len(statuses) != 1 || statuses[0].Account.ID != id {
				t.Fatalf("account was not persisted/listed: %+v, %v", statuses, err)
			}
			args := []string{"account", operation, "--id", id}
			if operation == "quota" {
				args = append(args, "--bytes", "500")
			}
			f.mustRun(t, args...)
			// Accounts without materialized instances must remain database-only.
			if _, err := os.Stat(f.unitDir); !os.IsNotExist(err) {
				t.Fatalf("unbound account created service files: %v", err)
			}
		})
	}
}

func TestAccountCLIPrivatePersistenceAndReset(t *testing.T) {
	f := newCLIFixture(t, true)
	id := f.create(t, "alice", "1000")
	s := f.open(t)
	if _, err := s.ApplySample(id, "fixture-process", map[string]int64{"inbound>>>fixture>>>traffic>>>uplink": 200}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f.mustRun(t, "account", "quota", "--id", "alice", "--bytes", "300")
	f.mustRun(t, "account", "reset", "--id", id)
	f.mustRun(t, "account", "disable", "--id", id)
	f.mustRun(t, "account", "reset", "--id", id)
	s = f.open(t)
	status, err := s.Status(id, time.Now())
	if err != nil || status.Account.QuotaBytes != 300 || status.Usage.Upload != 0 || status.Allowed || status.Reason != "disabled" {
		t.Fatalf("CLI quota/reset/disable state incorrect: %+v, %v", status, err)
	}
	var checkpoint int64
	if err := s.DB.QueryRow("SELECT value FROM samples WHERE account_id = ?", id).Scan(&checkpoint); err != nil || checkpoint != 200 {
		t.Fatalf("CLI reset removed cumulative checkpoint: %d, %v", checkpoint, err)
	}
	for path, mode := range map[string]os.FileMode{f.root: 0700, filepath.Join(f.root, "state.db"): 0600, filepath.Join(f.root, "manage.lock"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private CLI state permissions incorrect: %s, %v", path, err)
		}
	}
}

func nodeFixture(t *testing.T, f cliFixture) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.root), "node.json")
	data := []byte(`{"inbounds":[{"type":"vless","tag":"original","listen":"0.0.0.0","listen_port":22000,"users":[{"name":"original-user","uuid":"11111111-1111-4111-8111-111111111111","flow":"xtls-rprx-vision"}],"tls":{"enabled":true,"certificate_path":"/private/cert.pem","key_path":"/private/key.pem"}}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type bindingFixture struct {
	Inbounds []struct {
		Tag        string `json:"tag"`
		ListenPort int    `json:"listen_port"`
		Users      []struct {
			Name string `json:"name"`
			UUID string `json:"uuid"`
			Flow string `json:"flow"`
		} `json:"users"`
		TLS struct {
			CertificatePath string `json:"certificate_path"`
			KeyPath         string `json:"key_path"`
		} `json:"tls"`
	} `json:"inbounds"`
}

func binding(t *testing.T, s *store.Store, account string) bindingFixture {
	t.Helper()
	var data []byte
	if err := s.DB.QueryRow("SELECT inbound FROM bindings WHERE account_id = ?", account).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var result bindingFixture
	if err := json.Unmarshal(data, &result); err != nil || len(result.Inbounds) != 1 || len(result.Inbounds[0].Users) != 1 {
		t.Fatalf("invalid stored binding: %s, %v", data, err)
	}
	return result
}

func fixturePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 16; attempt++ {
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatalf("local fixture socket unavailable: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		packet, err := net.ListenPacket("udp", "0.0.0.0:"+strconv.Itoa(port))
		listener.Close()
		if err == nil {
			packet.Close()
			return port
		}
	}
	t.Fatal("no TCP/UDP fixture port available")
	return 0
}

func TestNodeCLIAddAndMultiAccountBinding(t *testing.T) {
	f := newCLIFixture(t, true)
	alice := f.create(t, "alice", "1000")
	bob := f.create(t, "bob", "1000")
	f.mustRun(t, "account", "disable", "--id", alice)
	f.mustRun(t, "account", "disable", "--id", bob)
	firstPort, secondPort := fixturePort(t), fixturePort(t)
	for firstPort == secondPort {
		secondPort = fixturePort(t)
	}
	path := nodeFixture(t, f)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.mustRun(t, "node", "add", "--name", "shared", "--template", path)
	f.mustRun(t, "node", "bind", "--node", "shared", "--account", alice, "--port", strconv.Itoa(firstPort))
	f.mustRun(t, "node", "bind", "--node", "shared", "--account", bob, "--port", strconv.Itoa(secondPort))
	s := f.open(t)
	var template []byte
	if err := s.DB.QueryRow("SELECT template FROM nodes WHERE name = 'shared'").Scan(&template); err != nil || !bytes.Equal(template, original) {
		t.Fatalf("node add/bind modified the original credentials: %s, %v", template, err)
	}
	a, b := binding(t, s, alice), binding(t, s, bob)
	ain, bin := a.Inbounds[0], b.Inbounds[0]
	if ain.ListenPort != firstPort || bin.ListenPort != secondPort || ain.Tag == bin.Tag || ain.Users[0].UUID == bin.Users[0].UUID || ain.Users[0].UUID == "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("accounts share binding identity or endpoint: %+v, %+v", ain, bin)
	}
	for _, inbound := range []bindingFixture{a, b} {
		in := inbound.Inbounds[0]
		if in.Users[0].Flow != "xtls-rprx-vision" || in.TLS.CertificatePath != "/private/cert.pem" || in.TLS.KeyPath != "/private/key.pem" {
			t.Fatalf("binding discarded protocol/TLS fields: %+v", in)
		}
	}
	if _, err := f.run("node", "bind", "--node", "shared", "--account", alice, "--port", strconv.Itoa(fixturePort(t))); err == nil {
		t.Fatal("same account/node accepted a second binding")
	}
	if _, err := os.Stat(filepath.Join(f.root, "instances")); !os.IsNotExist(err) {
		t.Fatalf("disabled fixture accounts started or materialized a core: %v", err)
	}
}

func TestNodeCLIBindRejectsPortAlreadyReservedByDisabledAccount(t *testing.T) {
	f := newCLIFixture(t, true)
	alice := f.create(t, "alice", "1000")
	bob := f.create(t, "bob", "1000")
	f.mustRun(t, "account", "disable", "--id", alice)
	f.mustRun(t, "account", "disable", "--id", bob)
	port := strconv.Itoa(fixturePort(t))
	f.mustRun(t, "node", "add", "--name", "shared", "--template", nodeFixture(t, f))
	f.mustRun(t, "node", "bind", "--node", "shared", "--account", alice, "--port", port)
	if _, err := f.run("node", "bind", "--node", "shared", "--account", bob, "--port", port); err == nil {
		t.Fatal("same listener port was persisted for two independent account processes")
	}
	s := f.open(t)
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM bindings WHERE account_id = ?", bob).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed port reservation left a binding persisted: %d, %v", count, err)
	}
}

func TestLegacyNodeCLIKeepsCredentialsAndSingleOwner(t *testing.T) {
	f := newCLIFixture(t, true)
	alice := f.create(t, "alice", "1000")
	bob := f.create(t, "bob", "1000")
	f.mustRun(t, "account", "disable", "--id", alice)
	f.mustRun(t, "account", "disable", "--id", bob)
	path := nodeFixture(t, f)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := f.open(t)
	if _, err := s.DB.Exec("INSERT INTO nodes(id,name,template,legacy) VALUES ('old-one', 'legacy', ?, 1)", original); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f.mustRun(t, "node", "bind", "--node", "legacy", "--account", alice)
	if _, err := f.run("node", "bind", "--node", "legacy", "--account", bob); err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("legacy identity accepted multiple owners: %v", err)
	}
	s = f.open(t)
	var inbound []byte
	if err := s.DB.QueryRow("SELECT inbound FROM bindings WHERE account_id = ?", alice).Scan(&inbound); err != nil || !bytes.Equal(inbound, original) {
		t.Fatalf("legacy binding changed its credentials or port: %s, %v", inbound, err)
	}
}

func materializedAccountingFixture(t *testing.T) (*App, cliFixture, string) {
	t.Helper()
	f := newCLIFixture(t, true)
	id := f.create(t, "alice", "1000")
	s := f.open(t)
	if err := s.EnsurePeriod(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO instances(account_id, stats_port, generation, process_id) VALUES (?, 31001, 'fixture-generation', 'fixture-process')", id); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(f.root, "instances", id), f.unitDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.unitDir, runtimeManager.UnitName(id)), []byte("[Service]\nUser=root\n"), 0600); err != nil {
		t.Fatal(err)
	}
	controller := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$APP_TEST_CTL_LOG\"\ncase \"$1\" in\nshow) case \"$2\" in --property=LoadState) printf 'loaded\\n';; --property=ActiveState) printf 'active\\n';; *) exit 99;; esac;;\nstop) exit 0;;\n*) exit 99;;\nesac\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.root), "bin", "systemctl"), []byte(controller), 0700); err != nil {
		t.Fatal(err)
	}
	o := options{Root: f.root, Core: f.core, UnitDir: f.unitDir}
	a := &App{options: o, DB: s, Manager: runtimeManager.Manager{Root: f.root, Core: f.core, UnitDir: f.unitDir}, Out: &bytes.Buffer{}}
	return a, f, id
}

func assertAccountStopAttempt(t *testing.T, f cliFixture, id string) {
	t.Helper()
	log, err := os.ReadFile(f.log)
	if err != nil || !strings.Contains(string(log), "stop "+runtimeManager.UnitName(id)+"\n") {
		t.Fatalf("account protection did not stop the materialized service: %q, %v", log, err)
	}
}

func TestDaemonDatabaseProtectionDoesNotTreatOpenAsHealthy(t *testing.T) {
	a, f, id := materializedAccountingFixture(t)
	// A valid SQLite file can open while its accounting schema is unusable.
	// The filesystem instance remains even when FK cascades delete DB rows.
	if _, err := a.DB.DB.Exec("DROP TABLE accounts"); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(f.root, "state.db"))
	if err != nil {
		t.Fatalf("fixture should reproduce Open success: %v", err)
	}
	reopened.Close()
	if err := a.reconcile(); err == nil {
		t.Fatal("invalid accounting schema was not detected")
	}
	// This is the fallback invoked by daemon after its accounting turn fails.
	_ = protectOnDatabaseFailure(a.options)
	assertAccountStopAttempt(t, f, id)
}

func TestReconcileStatusFailureProtectsAccount(t *testing.T) {
	a, f, id := materializedAccountingFixture(t)
	// Non-STRICT SQLite affinity can retain a nonnumeric value that passes the
	// nonnegative CHECK but fails scanning into the Go traffic ledger.
	if _, err := a.DB.DB.Exec("UPDATE usage SET upload = 'corrupt-int' WHERE account_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileAccount(id); err == nil {
		t.Fatal("invalid traffic ledger was not detected")
	}
	assertAccountStopAttempt(t, f, id)
}

func TestEnsurePeriodWriteFailureStillStopsKnownAccounts(t *testing.T) {
	a, f, id := materializedAccountingFixture(t)
	if _, err := a.DB.DB.Exec("CREATE TRIGGER reject_usage_insert BEFORE INSERT ON usage BEGIN SELECT RAISE(ABORT, 'fixture ledger write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcile(); err == nil {
		t.Fatal("accounting write failure was not detected")
	}
	assertAccountStopAttempt(t, f, id)
	account, err := a.DB.Account(id)
	if err != nil || !account.StatsFault {
		t.Fatalf("period write fault was not persisted: %+v, %v", account, err)
	}
}

func TestProtectionRetainsProcessIdentityWhenFaultCannotPersist(t *testing.T) {
	for _, scope := range []string{"global", "account"} {
		t.Run(scope, func(t *testing.T) {
			a, f, id := materializedAccountingFixture(t)
			if _, err := a.DB.DB.Exec("CREATE TRIGGER reject_fault BEFORE UPDATE OF stats_fault ON accounts WHEN NEW.stats_fault = 1 BEGIN SELECT RAISE(ABORT, 'fixture fault flag write failure'); END"); err != nil {
				t.Fatal(err)
			}
			var err error
			if scope == "global" {
				err = protect(a.options)
			} else {
				err = a.protectAccount(id, errors.New("fixture accounting failure"))
			}
			if err == nil {
				t.Fatal("fault flag persistence failure was not reported")
			}
			assertAccountStopAttempt(t, f, id)
			var processID string
			if err := a.DB.DB.QueryRow("SELECT process_id FROM instances WHERE account_id = ?", id).Scan(&processID); err != nil || processID != "fixture-process" {
				t.Fatalf("failed fault persistence erased the unexpected-exit guard: %q, %v", processID, err)
			}
		})
	}
}

func TestAccountCLICreateUnlimitedAndRejectConflictingLimits(t *testing.T) {
	t.Run("unlimited", func(t *testing.T) {
		f := newCLIFixture(t, false)
		var created map[string]string
		if err := json.Unmarshal(f.mustRun(t, "account", "create", "--name", "admin", "--unlimited"), &created); err != nil || created["id"] == "" {
			t.Fatalf("invalid unlimited create result: %v, %v", created, err)
		}
		s := f.open(t)
		status, err := s.ApplySample(created["id"], "fixture", map[string]int64{"inbound>>>fixture>>>traffic>>>uplink": 10000}, time.Now())
		if err != nil || !status.Allowed || !status.Unlimited || !status.Account.Unlimited || status.Usage.Upload != 10000 {
			t.Fatalf("CLI unlimited account did not count without quota enforcement: %+v, %v", status, err)
		}
	})
	for _, bytes := range []string{"0", "1000", "-1"} {
		t.Run("conflict-bytes-"+bytes, func(t *testing.T) {
			f := newCLIFixture(t, false)
			if _, err := f.run("account", "create", "--name", "admin", "--unlimited", "--bytes", bytes); err == nil {
				t.Fatal("create accepted both --bytes and --unlimited")
			}
			s := f.open(t)
			accounts, err := s.Accounts()
			if err != nil || len(accounts) != 0 {
				t.Fatalf("rejected create persisted an account: %+v, %v", accounts, err)
			}
		})
	}
}

func TestAccountCLIUnlimitedQuotaKeepsUsageAndProtection(t *testing.T) {
	f := newCLIFixture(t, false)
	id := f.create(t, "alice", "100")
	s := f.open(t)
	if _, err := s.ApplySample(id, "fixture", map[string]int64{"inbound>>>fixture>>>traffic>>>uplink": 500}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	f.mustRun(t, "account", "quota", "--id", id, "--unlimited")
	var statuses []store.AccountStatus
	if err := json.Unmarshal(f.mustRun(t, "account", "list"), &statuses); err != nil || len(statuses) != 1 || !statuses[0].Allowed || !statuses[0].Unlimited || statuses[0].Usage.Upload != 500 {
		t.Fatalf("CLI did not switch to unlimited while retaining usage: %+v, %v", statuses, err)
	}
	f.mustRun(t, "account", "quota", "--id", id, "--bytes", "400")
	if err := json.Unmarshal(f.mustRun(t, "account", "list"), &statuses); err != nil || len(statuses) != 1 || statuses[0].Allowed || statuses[0].Unlimited || statuses[0].Account.Unlimited || statuses[0].Reason != "quota_exceeded" || statuses[0].Usage.Upload != 500 {
		t.Fatalf("CLI fixed quota failed to enforce retained usage: %+v, %v", statuses, err)
	}
	f.mustRun(t, "account", "disable", "--id", id)
	f.mustRun(t, "account", "quota", "--id", id, "--unlimited")
	if err := json.Unmarshal(f.mustRun(t, "account", "list"), &statuses); err != nil || len(statuses) != 1 || statuses[0].Allowed || !statuses[0].Unlimited || statuses[0].Reason != "disabled" {
		t.Fatalf("CLI unlimited bypassed manual disable: %+v, %v", statuses, err)
	}
	f.mustRun(t, "account", "enable", "--id", id)
	s = f.open(t)
	if err := s.SetFault(id, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	f.mustRun(t, "account", "quota", "--id", id, "--unlimited")
	if err := json.Unmarshal(f.mustRun(t, "account", "list"), &statuses); err != nil || len(statuses) != 1 || statuses[0].Allowed || !statuses[0].Unlimited || statuses[0].Reason != "stats_fault" {
		t.Fatalf("CLI unlimited bypassed accounting fault: %+v, %v", statuses, err)
	}
}

func TestAccountCLIQuotaRejectsConflictingLimits(t *testing.T) {
	for _, value := range []string{"0", "1000", "-1"} {
		t.Run("bytes-"+value, func(t *testing.T) {
			f := newCLIFixture(t, false)
			id := f.create(t, "alice", "100")
			if _, err := f.run("account", "quota", "--id", id, "--unlimited", "--bytes", value); err == nil {
				t.Fatal("quota accepted both --bytes and --unlimited")
			}
			s := f.open(t)
			account, err := s.Account(id)
			if err != nil || account.Unlimited || account.QuotaBytes != 100 {
				t.Fatalf("rejected quota changed account limits: %+v, %v", account, err)
			}
		})
	}
}
