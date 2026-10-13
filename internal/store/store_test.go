package store

import (
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	uplink   = "inbound>>>first>>>traffic>>>uplink"
	downlink = "inbound>>>first>>>traffic>>>downlink"
)

var sampleTime = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func addAccount(t *testing.T, s *Store, id string, quota int64) {
	t.Helper()
	if err := s.CreateAccount(id, id, quota); err != nil {
		t.Fatal(err)
	}
}

func apply(t *testing.T, s *Store, id, generation string, counters map[string]int64, now time.Time) AccountStatus {
	t.Helper()
	status, err := s.ApplySample(id, generation, counters, now)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestSampleIdempotenceAndPersistence(t *testing.T) {
	s, path := testStore(t)
	addAccount(t, s, "a", 1000)
	counters := map[string]int64{uplink: 80, downlink: 120}
	apply(t, s, "a", "process-1", counters, sampleTime)
	status := apply(t, s, "a", "process-1", counters, sampleTime)
	if status.Usage.Upload != 80 || status.Usage.Download != 120 || !status.Allowed {
		t.Fatalf("repeated sample was charged twice: %+v", status)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status = apply(t, reopened, "a", "process-1", map[string]int64{uplink: 90, downlink: 140}, sampleTime)
	if status.Usage.Upload != 90 || status.Usage.Download != 140 {
		t.Fatalf("checkpoint was not restored: %+v", status.Usage)
	}
}

func TestCounterFaultDoesNotPartiallyCommit(t *testing.T) {
	for _, bad := range []int64{-1, 49} {
		t.Run(stringTestName(bad), func(t *testing.T) {
			s, _ := testStore(t)
			addAccount(t, s, "a", 1000)
			apply(t, s, "a", "p", map[string]int64{uplink: 50, downlink: 60}, sampleTime)
			status, err := s.ApplySample("a", "p", map[string]int64{uplink: bad, downlink: 100}, sampleTime)
			if !errors.Is(err, ErrInvalidSample) || status.Allowed || !status.Account.StatsFault {
				t.Fatalf("bad sample did not fail closed: %+v, %v", status, err)
			}
			if status.Usage.Upload != 50 || status.Usage.Download != 60 {
				t.Fatalf("fault modified ledger: %+v", status.Usage)
			}
			var previous int64
			if err := s.DB.QueryRow("SELECT value FROM samples WHERE name = ?", downlink).Scan(&previous); err != nil || previous != 60 {
				t.Fatalf("fault modified checkpoint: %d, %v", previous, err)
			}
			status = apply(t, s, "a", "p", map[string]int64{uplink: 70, downlink: 80}, sampleTime)
			if status.Allowed || !status.Account.StatsFault || status.Usage.Upload != 70 || status.Usage.Download != 80 {
				t.Fatalf("successful sample cleared fault or lost watermarks: %+v", status)
			}
		})
	}
}

func stringTestName(value int64) string {
	if value < 0 {
		return "negative"
	}
	return "backwards"
}

func TestMultipleBindingsShareQuota(t *testing.T) {
	s, _ := testStore(t)
	addAccount(t, s, "a", 100)
	addAccount(t, s, "b", 100)
	for _, id := range []string{"one", "two"} {
		if _, err := s.DB.Exec("INSERT INTO nodes(id, name, template) VALUES (?, ?, ?)", id, id, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec("INSERT INTO bindings(id, account_id, node_id, inbound) VALUES (?, 'a', ?, ?)", "a-"+id, id, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("INSERT INTO bindings(id, account_id, node_id, inbound) VALUES ('b-one', 'b', 'one', ?)", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	status := apply(t, s, "a", "p", map[string]int64{
		uplink: 20, downlink: 30,
		"inbound>>>second>>>traffic>>>uplink":   10,
		"inbound>>>second>>>traffic>>>downlink": 40,
	}, sampleTime)
	if status.Allowed || status.Reason != "quota_exceeded" || status.Usage.Upload != 30 || status.Usage.Download != 70 {
		t.Fatalf("shared quota incorrect: %+v", status)
	}
	other, err := s.Status("b", sampleTime)
	if err != nil || !other.Allowed || other.Usage.Upload != 0 || other.Usage.Download != 0 {
		t.Fatalf("other account affected: %+v, %v", other, err)
	}
	if _, err := s.DB.Exec("DELETE FROM bindings WHERE id = 'a-one'"); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status("a", sampleTime)
	if err != nil || status.Allowed || status.Usage.Upload+status.Usage.Download != 100 {
		t.Fatalf("unbind erased quota usage: %+v, %v", status, err)
	}
}

func TestBeijingMonthResetKeepsCheckpointsAndDisableReasons(t *testing.T) {
	s, _ := testStore(t)
	for _, id := range []string{"quota", "disabled", "fault"} {
		addAccount(t, s, id, 100)
	}
	last := time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC)
	next := last.Add(time.Second)
	if Period(last) != "2026-10" || Period(next) != "2026-11" {
		t.Fatal("month boundary did not use Beijing time")
	}
	apply(t, s, "quota", "p", map[string]int64{uplink: 100}, last)
	if err := s.SetEnabled("disabled", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFault("fault", true); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePeriod(next); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsurePeriod(next); err != nil {
		t.Fatal(err)
	}
	status := apply(t, s, "quota", "p", map[string]int64{uplink: 110}, next)
	if !status.Allowed || status.Usage.Upload != 10 || status.Usage.Period != "2026-11" {
		t.Fatalf("month reset erased checkpoint or carried usage: %+v", status)
	}
	var historic int64
	if err := s.DB.QueryRow("SELECT upload FROM usage WHERE account_id = 'quota' AND period = '2026-10'").Scan(&historic); err != nil || historic != 100 {
		t.Fatalf("historic usage was lost: %d, %v", historic, err)
	}
	for id, reason := range map[string]string{"disabled": "disabled", "fault": "stats_fault"} {
		status, err := s.Status(id, next)
		if err != nil || status.Allowed || status.Reason != reason {
			t.Fatalf("month reset cleared %s: %+v, %v", reason, status, err)
		}
	}
	if err := s.EnsurePeriod(last); !errors.Is(err, ErrClockRollback) {
		t.Fatalf("clock reversal reopened old period: %v", err)
	}
	if _, err := s.Status("quota", last); !errors.Is(err, ErrClockRollback) {
		t.Fatalf("Status allowed clock reversal: %v", err)
	}
	if _, err := s.ApplySample("quota", "p", map[string]int64{uplink: 120}, last); !errors.Is(err, ErrClockRollback) {
		t.Fatalf("sample allowed clock reversal: %v", err)
	}
	status = apply(t, s, "quota", "p", map[string]int64{uplink: 120}, next)
	if status.Usage.Upload != 20 {
		t.Fatalf("clock reversal committed sample checkpoint: %+v", status)
	}
}

func TestManualResetAndGeneration(t *testing.T) {
	s, _ := testStore(t)
	addAccount(t, s, "a", 100)
	apply(t, s, "a", "p1", map[string]int64{uplink: 100}, sampleTime)
	if err := s.ResetUsage("a", sampleTime); err != nil {
		t.Fatal(err)
	}
	status := apply(t, s, "a", "p1", map[string]int64{uplink: 100}, sampleTime)
	if !status.Allowed || status.Usage.Upload != 0 {
		t.Fatalf("reset recharged previously measured traffic: %+v", status)
	}
	status = apply(t, s, "a", "p1", map[string]int64{uplink: 110}, sampleTime)
	status = apply(t, s, "a", "p2", map[string]int64{uplink: 15}, sampleTime)
	if status.Usage.Upload != 25 {
		t.Fatalf("new process generation used an old watermark: %+v", status)
	}
	if err := s.SetEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetUsage("a", sampleTime); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status("a", sampleTime)
	if err != nil || status.Allowed || status.Reason != "disabled" {
		t.Fatalf("manual reset enabled disabled account: %+v, %v", status, err)
	}
}

func TestQuotaChangesAndValidation(t *testing.T) {
	s, _ := testStore(t)
	addAccount(t, s, "a", 100)
	apply(t, s, "a", "p", map[string]int64{uplink: 50}, sampleTime)
	for _, quota := range []int64{50, 0} {
		if err := s.SetQuota("a", quota); err != nil {
			t.Fatal(err)
		}
		status, err := s.Status("a", sampleTime)
		if err != nil || status.Allowed || status.Reason != "quota_exceeded" || status.Usage.Upload != 50 {
			t.Fatalf("quota change bypassed limit: %+v, %v", status, err)
		}
	}
	if err := s.SetQuota("a", 51); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status("a", sampleTime)
	if err != nil || !status.Allowed {
		t.Fatalf("raised quota did not allow access: %+v, %v", status, err)
	}
	addAccount(t, s, "zero", 0)
	status, err = s.Status("zero", sampleTime)
	if err != nil || status.Allowed {
		t.Fatalf("zero quota account was allowed: %+v, %v", status, err)
	}
	if err := s.CreateAccount("negative", "negative", -1); err == nil {
		t.Fatal("negative initial quota accepted")
	}
	if err := s.SetQuota("a", -1); err == nil {
		t.Fatal("negative quota accepted")
	}
	if err := s.SetEnabled("missing", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown account update succeeded: %v", err)
	}
	if err := s.ResetUsage("missing", sampleTime); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown account reset succeeded: %v", err)
	}
}

func TestOverflowFailsClosed(t *testing.T) {
	s, _ := testStore(t)
	addAccount(t, s, "a", math.MaxInt64)
	status, err := s.ApplySample("a", "p", map[string]int64{uplink: math.MaxInt64, downlink: 1}, sampleTime)
	if !errors.Is(err, ErrInvalidSample) || status.Allowed || !status.Account.StatsFault || status.Usage.Upload != 0 {
		t.Fatalf("overflow did not preserve ledger and fail closed: %+v, %v", status, err)
	}
}

func TestStoragePermissionsPragmasAndFutureSchema(t *testing.T) {
	s, path := testStore(t)
	for target, wanted := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != wanted {
			t.Fatalf("private storage permissions incorrect: %s, %v", target, err)
		}
	}
	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("WAL not enabled: %s, %v", mode, err)
	}
	for pragma, wanted := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 5000, "user_version": schemaVersion} {
		var value int
		if err := s.DB.QueryRow("PRAGMA " + pragma).Scan(&value); err != nil || value != wanted {
			t.Fatalf("%s incorrect: %d, %v", pragma, value, err)
		}
	}
	// Every replacement connection must enforce foreign keys too.
	s.DB.SetMaxIdleConns(0)
	for i := 0; i < 2; i++ {
		if _, err := s.DB.Exec("INSERT INTO instances(account_id, stats_port) VALUES ('missing', 50001)"); err == nil {
			t.Fatal("replacement connection did not enforce foreign keys")
		}
	}
	if _, err := s.DB.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if newer, err := Open(path); err == nil {
		newer.Close()
		t.Fatal("future database version was accepted")
	}
}

func TestOpenRejectsPublicDirectoryAndSymlink(t *testing.T) {
	dir := t.TempDir()
	public := filepath.Join(dir, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(public, "state.db")); err == nil {
		s.Close()
		t.Fatal("public state directory accepted")
	}
	info, err := os.Stat(public)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("existing public directory permissions were changed")
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(filepath.Join(dir, "target.db"), link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(link); err == nil {
		s.Close()
		t.Fatal("database symlink accepted")
	}
}

func TestUnlimitedAccountStillCountsAndResetsMonthly(t *testing.T) {
	s, path := testStore(t)
	if err := s.CreateUnlimitedAccount("admin", "admin"); err != nil {
		t.Fatal(err)
	}
	status := apply(t, s, "admin", "p", map[string]int64{uplink: 500, downlink: 500000}, sampleTime)
	if !status.Allowed || !status.Unlimited || !status.Account.Unlimited || status.Account.QuotaBytes != 0 || status.Usage.Upload != 500 || status.Usage.Download != 500000 {
		t.Fatalf("unlimited account was limited or not counted: %+v", status)
	}
	accounts, err := s.Accounts()
	if err != nil || len(accounts) != 1 || !accounts[0].Unlimited {
		t.Fatalf("unlimited flag was not exposed in accounts: %+v, %v", accounts, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err = reopened.Status("admin", sampleTime)
	if err != nil || !status.Allowed || !status.Unlimited || !status.Account.Unlimited {
		t.Fatalf("unlimited account did not persist: %+v, %v", status, err)
	}
	next := time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)
	status = apply(t, reopened, "admin", "p", map[string]int64{uplink: 510, downlink: 500020}, next)
	if !status.Allowed || !status.Unlimited || status.Usage.Period != "2026-11" || status.Usage.Upload != 10 || status.Usage.Download != 20 {
		t.Fatalf("unlimited monthly reset lost checkpoints: %+v", status)
	}
	var historicUpload, historicDownload int64
	if err := reopened.DB.QueryRow("SELECT upload, download FROM usage WHERE account_id = 'admin' AND period = '2026-10'").Scan(&historicUpload, &historicDownload); err != nil || historicUpload != 500 || historicDownload != 500000 {
		t.Fatalf("unlimited history was not preserved: %d/%d, %v", historicUpload, historicDownload, err)
	}
}

func TestSetUnlimitedAndReturnToFixedQuota(t *testing.T) {
	s, _ := testStore(t)
	addAccount(t, s, "a", 50)
	status := apply(t, s, "a", "p", map[string]int64{uplink: 60}, sampleTime)
	if status.Allowed {
		t.Fatal("fixed quota did not limit account")
	}
	if err := s.SetUnlimited("a"); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status("a", sampleTime)
	if err != nil || !status.Allowed || !status.Unlimited || status.Account.QuotaBytes != 50 || status.Usage.Upload != 60 {
		t.Fatalf("switching to unlimited erased quota/usage or stayed limited: %+v, %v", status, err)
	}
	if err := s.SetQuota("a", 55); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status("a", sampleTime)
	if err != nil || status.Allowed || status.Unlimited || status.Account.Unlimited || status.Reason != "quota_exceeded" || status.Usage.Upload != 60 {
		t.Fatalf("SetQuota did not restore fixed enforcement: %+v, %v", status, err)
	}
	if err := s.SetUnlimited("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuota("a", -1); err == nil {
		t.Fatal("negative quota accepted")
	}
	status, err = s.Status("a", sampleTime)
	if err != nil || !status.Unlimited {
		t.Fatalf("rejected quota changed unlimited flag: %+v, %v", status, err)
	}
	if err := s.SetQuota("a", 0); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status("a", sampleTime)
	if err != nil || status.Allowed || status.Unlimited || status.Reason != "quota_exceeded" {
		t.Fatalf("zero fixed quota became unlimited: %+v, %v", status, err)
	}
}

func TestUnlimitedDoesNotBypassDisabledOrFaultState(t *testing.T) {
	s, _ := testStore(t)
	if err := s.CreateUnlimitedAccount("admin", "admin"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, "admin", "p", map[string]int64{uplink: 100}, sampleTime)
	if err := s.SetEnabled("admin", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetUsage("admin", sampleTime); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status("admin", sampleTime)
	if err != nil || status.Allowed || !status.Unlimited || status.Reason != "disabled" {
		t.Fatalf("unlimited reset bypassed manual disable: %+v, %v", status, err)
	}
	if err := s.SetEnabled("admin", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFault("admin", true); err != nil {
		t.Fatal(err)
	}
	status, err = s.Status("admin", sampleTime.AddDate(0, 1, 0))
	if err != nil || status.Allowed || !status.Unlimited || status.Reason != "stats_fault" {
		t.Fatalf("unlimited monthly reset bypassed accounting fault: %+v, %v", status, err)
	}
	if err := s.SetFault("admin", false); err != nil {
		t.Fatal(err)
	}
	status, err = s.ApplySample("admin", "p", map[string]int64{uplink: 99}, sampleTime.AddDate(0, 1, 0))
	if !errors.Is(err, ErrInvalidSample) || status.Allowed || !status.Unlimited || !status.Account.StatsFault {
		t.Fatalf("unlimited account bypassed counter regression protection: %+v, %v", status, err)
	}
}

func TestSchemaV1MigrationPreservesFixedAccountsAndLedgers(t *testing.T) {
	s, path := testStore(t)
	addAccount(t, s, "a", 1024)
	apply(t, s, "a", "old-generation", map[string]int64{uplink: 100, downlink: 200}, sampleTime)
	if err := s.SetEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFault("a", true); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO usage(account_id, period, upload, download) VALUES ('a', '2026-09', 700, 800)",
		"INSERT INTO nodes(id, name, template, legacy) VALUES ('old', 'legacy', x'7b7d', 1)",
		"INSERT INTO bindings(id, account_id, node_id, inbound) VALUES ('binding', 'a', 'old', x'7b7d')",
		"INSERT INTO instances(account_id, stats_port, generation, process_id, config_hash) VALUES ('a', 31111, 'old-generation', 'old-process', 'old-hash')",
		"INSERT INTO settings(key, value) VALUES ('custom_setting', 'preserved')",
		// Remove the v2-only column to create an actual v1 schema fixture.
		"ALTER TABLE accounts DROP COLUMN unlimited",
		"PRAGMA user_version = 1",
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var operations int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM operations").Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	status, err := migrated.Status("a", sampleTime)
	if err != nil || status.Unlimited || status.Account.Unlimited || status.Account.QuotaBytes != 1024 || status.Account.Enabled || !status.Account.StatsFault || status.Usage.Upload != 100 || status.Usage.Download != 200 {
		t.Fatalf("migration changed account enforcement or ledger: %+v, %v", status, err)
	}
	for query, wanted := range map[string]int{
		"PRAGMA user_version": schemaVersion,
		"SELECT COUNT(*) FROM usage WHERE account_id = 'a' AND period = '2026-09' AND upload = 700 AND download = 800":                                                               1,
		"SELECT COUNT(*) FROM samples WHERE account_id = 'a' AND generation = 'old-generation'":                                                                                      2,
		"SELECT COUNT(*) FROM nodes WHERE id = 'old' AND legacy = 1":                                                                                                                 1,
		"SELECT COUNT(*) FROM bindings WHERE id = 'binding' AND account_id = 'a' AND node_id = 'old'":                                                                                1,
		"SELECT COUNT(*) FROM instances WHERE account_id = 'a' AND stats_port = 31111 AND process_id = 'old-process' AND generation = 'old-generation' AND config_hash = 'old-hash'": 1,
		"SELECT COUNT(*) FROM settings WHERE key = 'custom_setting' AND value = 'preserved'":                                                                                         1,
		"SELECT COUNT(*) FROM operations": operations,
	} {
		var got int
		if err := migrated.DB.QueryRow(query).Scan(&got); err != nil || got != wanted {
			t.Fatalf("migration did not preserve stored data: %s: %d, want %d, %v", query, got, wanted, err)
		}
	}
	if err := migrated.SetUnlimited("a"); err != nil {
		t.Fatal(err)
	}
	status, err = migrated.Status("a", sampleTime)
	if err != nil || status.Allowed || !status.Unlimited || status.Reason != "disabled" || !status.Account.StatsFault {
		t.Fatalf("unlimited migration bypassed prior protection: %+v, %v", status, err)
	}
}
