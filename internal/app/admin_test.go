package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oldtian123/sing-box-enhanced/internal/credential"
)

func (f cliFixture) runAdmin(input string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	err := runAdmin(options{Root: f.root, Core: f.core, UnitDir: f.unitDir}, args, &out, strings.NewReader(input))
	return out.Bytes(), err
}

func TestAdministratorSetupReusesLegacyAccountAndKeepsLedger(t *testing.T) {
	f := newCLIFixture(t, false)
	id := f.create(t, "admin", "100")
	s := f.open(t)
	if _, err := s.ApplySample(id, "legacy-process", map[string]int64{"inbound>>>legacy>>>traffic>>>uplink": 40}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	out, err := f.run("admin", "setup")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, administratorCredentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved administratorCredentials
	if err := json.Unmarshal(data, &saved); err != nil || saved.Username != "admin" || len(saved.Password) < 32 {
		t.Fatal("invalid private generated credentials")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("administrator credentials are not private")
	}
	s = f.open(t)
	administrator, err := s.Administrator()
	if err != nil || administrator.ID != id {
		t.Fatalf("legacy account was not reused: %v", err)
	}
	valid, err := credential.VerifyPassword(saved.Password, administrator.Hash)
	if err != nil || !valid {
		t.Fatal("generated password does not match hash")
	}
	status, err := s.Status(id, time.Now())
	if err != nil || !status.Account.Unlimited || status.Usage.Upload != 40 {
		t.Fatalf("setup changed ledger: %+v, %v", status, err)
	}
	s.Close()
	if bytes.Contains(out, []byte(saved.Password)) || bytes.Contains(out, []byte(administrator.Hash)) {
		t.Fatal("setup leaked password or hash")
	}
	if _, err := f.run("admin", "setup"); err == nil {
		t.Fatal("setup overwrote existing administrator")
	}
	f.mustRun(t, "admin", "rename", "--name", "管理员")
	data, err = os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.Username != "管理员" {
		t.Fatal("rename left stale generated credentials")
	}
	const replacement = "new-private-password-123"
	if out, err = f.runAdmin(replacement+"\n", "passwd", "--password-stdin"); err != nil || bytes.Contains(out, []byte(replacement)) {
		t.Fatalf("password rotation failed or leaked: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("custom password left stale initial credential file")
	}
	s = f.open(t)
	administrator, err = s.Administrator()
	valid, verifyErr := credential.VerifyPassword(replacement, administrator.Hash)
	status, statusErr := s.Status(id, time.Now())
	if err != nil || verifyErr != nil || !valid || administrator.Name != "管理员" || statusErr != nil || status.Usage.Upload != 40 {
		t.Fatal("rename/password update changed administrator identity or ledger")
	}
	var audit string
	rows, err := s.DB.Query("SELECT event FROM operations")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&audit); err != nil || strings.Contains(audit, replacement) || strings.Contains(audit, administrator.Hash) {
			t.Fatal("audit leaked credentials")
		}
	}
	if _, err := os.Stat(f.unitDir); !os.IsNotExist(err) {
		t.Fatal("administrator configuration restarted or created services")
	}
}

func TestAdministratorCustomPasswordAndInputValidation(t *testing.T) {
	f := newCLIFixture(t, false)
	if _, err := f.run("admin", "show"); err == nil {
		t.Fatal("missing administrator was reported configured")
	}
	for _, args := range [][]string{{"setup", "--name", ""}, {"setup", "--name", "bad\nname"}, {"passwd", "--password", "secret"}, {"show", "--name", "admin"}} {
		if _, err := f.runAdmin("", args...); err == nil {
			t.Fatalf("accepted invalid admin arguments: %q", args)
		}
	}
	for _, input := range []string{"short", "first-line-long\nsecond-line", strings.Repeat("x", 1025)} {
		if _, err := f.runAdmin(input, "setup", "--password-stdin"); err == nil {
			t.Fatal("accepted invalid custom password")
		}
	}
	const password = "custom-admin-password-123"
	if _, err := f.runAdmin(password+"\r\n", "setup", "--name", "operator", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, administratorCredentialsFile)); !os.IsNotExist(err) {
		t.Fatal("custom password was persisted in plaintext")
	}
	out := f.mustRun(t, "admin", "show")
	if !bytes.Contains(out, []byte("operator")) || bytes.Contains(out, []byte(password)) || bytes.Contains(out, []byte("argon2")) {
		t.Fatal("admin show failed or exposed credentials")
	}
}
