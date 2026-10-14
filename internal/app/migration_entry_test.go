package app

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldtian123/sing-box-enhanced/internal/store"
)

func migrationEntryFixture(t *testing.T) (*App, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "state ' $literal `literal`")
	legacy := filepath.Join(dir, "legacy ' installation")
	backup := filepath.Join(root, "backups", "migration-test")
	s, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a := &App{options: options{Root: root, Core: filepath.Join(root, "bin", "core ' $literal"), UnitDir: filepath.Join(dir, "units ' $literal")}, DB: s, Out: io.Discard}
	for key, value := range map[string]string{"legacy_root": legacy, "migration_backup": backup} {
		if err := a.set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	legacyScript := filepath.Join(legacy, "sh", "sing-box.sh")
	if err := os.MkdirAll(filepath.Dir(legacyScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyScript, []byte("#!/bin/bash\nprintf 'legacy-script\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return a, legacy, backup
}

func TestTakeoverEntryBacksUpAndGuardsAllLegacyEntryPoints(t *testing.T) {
	a, legacy, backup := migrationEntryFixture(t)
	originalPath := filepath.Join(legacy, "sh", "sing-box.sh")
	original := []byte("#!/bin/bash\nprintf 'original-script\\n'\n")
	if err := os.MkdirAll(filepath.Dir(originalPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originalPath, original, 0700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(filepath.Dir(legacy), "sb")
	// Preserve the stored symlink target, including its relative spelling.
	target, err := filepath.Rel(filepath.Dir(entry), originalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, entry); err != nil {
		t.Fatal(err)
	}
	secondEntry := filepath.Join(filepath.Dir(legacy), "sing-box")
	if err := os.Symlink(target, secondEntry); err != nil {
		t.Fatal(err)
	}
	if err := a.takeoverEntry(entry); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(entry)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 {
		t.Fatalf("entry was not replaced with an executable private wrapper: %v", err)
	}
	for path, wanted := range map[string][]byte{
		filepath.Join(backup, "legacy-script.original"): original,
		filepath.Join(backup, "sb-entry.original"):      original,
		filepath.Join(backup, "sb-entry.symlink"):       []byte(target),
	} {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, wanted) {
			t.Fatalf("original script or entry bytes were not backed up: %s: %q, %v", path, data, err)
		}
	}
	for _, name := range []string{"legacy-script.original", "sb-entry.original", "sb-entry.symlink"} {
		info, err := os.Stat(filepath.Join(backup, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("entry backup is not private: %s, %v", name, err)
		}
	}
	wrapper, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	if guarded, err := os.ReadFile(originalPath); err != nil || !bytes.Equal(guarded, wrapper) {
		t.Fatalf("direct legacy script remains an unguarded entry: %q, %v", guarded, err)
	}
	if targetNow, err := os.Readlink(secondEntry); err != nil || targetNow != target {
		t.Fatalf("secondary legacy symlink was unnecessarily rewritten: %q, %v", targetNow, err)
	}
	for _, path := range []string{entry, originalPath, secondEntry} {
		if output, err := exec.Command("/bin/bash", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("wrapper is invalid Bash: %s: %s, %v", path, output, err)
		}
	}
	launcher := filepath.Join(a.Root, "sh", "scripts", "sb-enhanced.sh")
	if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	// NUL framing proves that shell metacharacters and empty arguments survive.
	recorder := []byte("#!/bin/bash\nprintf '%s\\0' \"$SBE_ROOT\" \"$SBE_CORE\" \"$SBE_BIN\" \"$SBE_SH_ROOT\" \"$SBE_UNIT_DIR\" \"$@\"\n")
	if err := os.WriteFile(launcher, recorder, 0700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"space in argument", "quote'\"", "$HOME", "`uname`", "$(printf injected)", "", "; false"}
	wanted := []string{a.Root, a.Core, filepath.Join(a.Root, "bin", "sbe"), filepath.Join(a.Root, "sh"), a.UnitDir}
	wanted = append(wanted, arguments...)
	for _, path := range []string{entry, originalPath, secondEntry} {
		cmd := exec.Command("/bin/bash", append([]string{path}, arguments...)...)
		cmd.Env = append(os.Environ(), "PATH=/bin:/usr/bin")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("wrapper execution failed: %s: %q, %v", path, output, err)
		}
		parts := bytes.Split(output, []byte{0})
		if len(parts) != len(wanted)+1 || len(parts[len(parts)-1]) != 0 {
			t.Fatalf("wrapper did not preserve argument framing: %s: %q", path, output)
		}
		for i, value := range wanted {
			if string(parts[i]) != value {
				t.Fatalf("wrapper changed field %d: got %q, want %q", i, parts[i], value)
			}
		}
	}
}

func TestTakeoverEntryRejectsUnrelatedTargetsWithoutChanges(t *testing.T) {
	for _, insideLegacy := range []bool{false, true} {
		name := "outside-legacy"
		if insideLegacy {
			name = "inside-legacy-unrelated-script"
		}
		t.Run(name, func(t *testing.T) {
			a, legacy, backup := migrationEntryFixture(t)
			target := filepath.Join(filepath.Dir(legacy), "unrelated-command")
			if insideLegacy {
				target = filepath.Join(legacy, "sh", "other-command.sh")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("#!/bin/bash\nprintf 'unrelated-command\\n'\n")
			if err := os.WriteFile(target, original, 0700); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(filepath.Dir(legacy), "sb")
			if err := os.Symlink(target, entry); err != nil {
				t.Fatal(err)
			}
			if err := a.takeoverEntry(entry); err == nil {
				t.Fatal("unrelated entry was accepted for replacement")
			}
			if got, err := os.Readlink(entry); err != nil || got != target {
				t.Fatalf("rejected entry symlink was changed: %q, %v", got, err)
			}
			if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
				t.Fatalf("rejected target bytes were changed: %q, %v", got, err)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("rejected entry created or modified a backup: %v", err)
			}
		})
	}
}

func TestOneStepMigrationRequiresOwnerBeforeDiscoveryOrServiceOperations(t *testing.T) {
	f := newCLIFixture(t, false)
	t.Setenv("SBE_DEVELOPER", "1")
	// Neither this installation nor systemctl exists. The owner validation must
	// reject the command first, before discovery, import, or service operations.
	_, err := f.run("migrate", "run", "--legacy-root", filepath.Join(f.root, "missing-legacy"), "--takeover-entry", filepath.Join(f.root, "missing-entry"))
	if err == nil || !strings.Contains(err.Error(), "explicit --owner") {
		t.Fatalf("one-step migration did not require an owner first: %v", err)
	}
	s := f.open(t)
	for _, table := range []string{"nodes", "bindings", "instances", "settings"} {
		var count int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("owner-less migration modified %s: %d, %v", table, count, err)
		}
	}
	for _, path := range []string{f.unitDir, filepath.Join(f.root, "backups"), filepath.Join(f.root, "base.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owner-less migration touched configuration or services: %s, %v", path, err)
		}
	}
}
