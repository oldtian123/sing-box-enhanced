package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeManager(t *testing.T) (Manager, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	core := filepath.Join(bin, "sing-box")
	if err := os.WriteFile(core, []byte("#!/bin/sh\ncase \"$(cat \"$3\")\" in *INVALID*) exit 1;; esac\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctl := filepath.Join(bin, "systemctl")
	if err := os.WriteFile(ctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SBE_TEST_LOG\"\nif [ \"$1\" = daemon-reload ] && [ -f \"$SBE_TEST_FAIL\" ]; then rm \"$SBE_TEST_FAIL\"; exit 1; fi\nif [ \"$1\" = stop ] && [ \"$SBE_TEST_STOP_FAIL\" = 1 ]; then exit 1; fi\nif [ \"$1\" = show ]; then case \"$2\" in --property=ActiveState) printf '%s\\n' \"$SBE_TEST_STATE\";; --property=MainPID) printf '%s\\n' 0;; --property=LoadState) if [ \"$SBE_TEST_LOAD_FAIL\" = 1 ]; then exit 1; fi; printf '%s\\n' \"$SBE_TEST_LOAD\";; esac; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "systemctl.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SBE_TEST_LOG", log)
	t.Setenv("SBE_TEST_FAIL", filepath.Join(dir, "fail-reload"))
	t.Setenv("SBE_TEST_STATE", "active")
	t.Setenv("SBE_TEST_LOAD", "loaded")
	t.Setenv("SBE_TEST_LOAD_FAIL", "0")
	t.Setenv("SBE_TEST_STOP_FAIL", "0")
	return Manager{Root: filepath.Join(dir, "data"), Core: core, UnitDir: filepath.Join(dir, "units")}, log
}

func TestApplyPrivateConfigurationAndNoAutomaticStart(t *testing.T) {
	m, log := fakeManager(t)
	hash, err := m.Apply("alice", []byte(`{"secret":"keep-private"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 {
		t.Fatal("missing content hash")
	}
	config := filepath.Join(m.Root, "instances", "alice", "config.json")
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration does not have private permissions")
	}
	info, err = os.Stat(filepath.Dir(config))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("account directory does not have private permissions")
	}
	unit, err := os.ReadFile(filepath.Join(m.UnitDir, UnitName("alice")))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"User=root", "Restart=no", "KillMode=control-group", "BindsTo=sing-box-enhanced-meter.service", config} {
		if !strings.Contains(string(unit), required) {
			t.Fatalf("missing unit property %q", required)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "daemon-reload\n" {
		t.Fatalf("Apply started or modified a service: %q", calls)
	}
}

func TestCheckBytesPrivateCandidatePreservesInstalledFiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		config  string
		wantErr bool
	}{
		{"valid", `{"version":"candidate"}`, false},
		{"invalid", "INVALID", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, log := fakeManager(t)
			dir := m.instanceDir("alice")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(m.UnitDir, 0755); err != nil {
				t.Fatal(err)
			}
			installed := map[string]string{
				filepath.Join(dir, "config.json"):           `{"version":"current"}`,
				filepath.Join(dir, "config.json.previous"):  `{"version":"previous"}`,
				filepath.Join(m.UnitDir, UnitName("alice")): "unchanged unit\n",
			}
			for path, content := range installed {
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			seen := filepath.Join(t.TempDir(), "candidate-path")
			t.Setenv("SBE_TEST_CANDIDATE", seen)
			// Inspect the candidate while it exists, including its private mode.
			script := "#!/bin/sh\ncase \"$(LC_ALL=C ls -ld \"$3\")\" in -rw-------*) ;; *) exit 2;; esac\nprintf '%s\\n' \"$3\" > \"$SBE_TEST_CANDIDATE\"\ncase \"$(cat \"$3\")\" in *INVALID*) exit 1;; esac\n"
			if err := os.WriteFile(m.Core, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := m.CheckBytes("alice", []byte(test.config)); (err != nil) != test.wantErr {
				t.Fatalf("CheckBytes error = %v, want error %v", err, test.wantErr)
			}
			for path, content := range installed {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != content {
					t.Fatalf("preflight modified installed file %s: %q, %v", path, data, err)
				}
			}
			for _, path := range []string{m.Root, filepath.Join(m.Root, "instances"), dir} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("candidate directory is not private: %s, %v", path, err)
				}
			}
			candidate, err := os.ReadFile(seen)
			if err != nil || filepath.Dir(strings.TrimSpace(string(candidate))) != dir {
				t.Fatalf("core did not inspect an account-scoped candidate: %q, %v", candidate, err)
			}
			if _, err := os.Stat(strings.TrimSpace(string(candidate))); !os.IsNotExist(err) {
				t.Fatalf("candidate remains after preflight: %v", err)
			}
			if paths, err := filepath.Glob(filepath.Join(dir, ".candidate-*.json")); err != nil || len(paths) != 0 {
				t.Fatalf("candidate files remain after preflight: %v, %v", paths, err)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("preflight unexpectedly called systemctl: %v", err)
			}
		})
	}
}

func TestCheckBytesNewAccountDoesNotInstallConfigurationOrUnit(t *testing.T) {
	m, log := fakeManager(t)
	if err := m.CheckBytes("../unsafe", []byte(`{}`)); err == nil {
		t.Fatal("unsafe account ID accepted")
	}
	if _, err := os.Stat(m.Root); !os.IsNotExist(err) {
		t.Fatalf("invalid input created runtime files: %v", err)
	}
	if err := m.CheckBytes("new-account", []byte(`{"version":"candidate"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(m.instanceDir("new-account"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("new account preflight left files: %v, %v", entries, err)
	}
	if _, err := os.Stat(m.UnitDir); !os.IsNotExist(err) {
		t.Fatalf("preflight created the systemd unit directory: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("preflight unexpectedly called systemctl: %v", err)
	}
}

func TestApplyValidationAndReloadFailuresPreservePriorConfig(t *testing.T) {
	m, _ := fakeManager(t)
	old := []byte(`{"version":"old"}`)
	if _, err := m.Apply("alice", old); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(m.Root, "instances", "alice", "config.json")
	if _, err := m.Apply("alice", []byte("INVALID")); err == nil {
		t.Fatal("invalid configuration applied")
	}
	data, _ := os.ReadFile(config)
	if string(data) != string(old) {
		t.Fatal("validation failure replaced the working configuration")
	}
	if err := os.WriteFile(os.Getenv("SBE_TEST_FAIL"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply("alice", []byte(`{"version":"new"}`)); err == nil {
		t.Fatal("reload failure was not reported")
	}
	data, _ = os.ReadFile(config)
	if string(data) != string(old) {
		t.Fatal("reload failure did not restore the working configuration")
	}
	backup, _ := os.ReadFile(config + ".previous")
	if string(backup) != string(old) {
		t.Fatal("prior configuration was not retained")
	}
}

func TestServiceOperationsAreAccountScoped(t *testing.T) {
	m, log := fakeManager(t)
	if _, err := m.Apply("alice", []byte(`{"version":"initial"}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop("alice"); err != nil {
		t.Fatal(err)
	}
	active, err := m.Running("alice")
	if err != nil || !active {
		t.Fatal("active state not detected")
	}
	t.Setenv("SBE_TEST_STATE", "failed")
	active, err = m.Running("alice")
	if err != nil || active {
		t.Fatal("failed state reported running")
	}
	if _, err := m.ProcessID("alice"); err == nil {
		t.Fatal("missing process accepted as an identity")
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "start sbe-account-alice.service\n") || !strings.Contains(string(data), "stop sbe-account-alice.service\n") || strings.Contains(string(data), "sing-box.service") {
		t.Fatal("operation was not scoped to the account instance")
	}
	for _, unsafe := range []string{"../alice", "alice;reboot", "alice\nother.service", ""} {
		if err := m.Start(unsafe); err == nil {
			t.Fatalf("unsafe account ID accepted %q", unsafe)
		}
	}
}

func TestRunningBeforeUnitCreationIsFalse(t *testing.T) {
	m, log := fakeManager(t)
	active, err := m.Running("new-account")
	if err != nil || active {
		t.Fatal("a new account without a unit should not be running")
	}
	if data, _ := os.ReadFile(log); len(data) > 0 {
		t.Fatal("missing unit should not cause a systemctl query")
	}
	if _, err := m.Apply("new-account", []byte(`{"version":"initial"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBE_TEST_STATE", "")
	if _, err := m.Running("new-account"); err == nil {
		t.Fatal("unknown runtime status was silently treated as stopped")
	}
}

func TestStopAbsentUnitSucceedsWithoutHidingRealFailures(t *testing.T) {
	m, log := fakeManager(t)
	if err := m.Stop("alice"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(log); len(data) > 0 {
		t.Fatal("missing unit should not cause a systemctl query")
	}
	if _, err := m.Apply("alice", []byte(`{"version":"initial"}`)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBE_TEST_LOAD", "not-found")
	if err := m.Stop("alice"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "stop ") {
		t.Fatal("missing unit was unnecessarily stopped")
	}
	t.Setenv("SBE_TEST_LOAD", "loaded")
	t.Setenv("SBE_TEST_STOP_FAIL", "1")
	if err := m.Stop("alice"); err == nil {
		t.Fatal("actual stop failure was hidden")
	}
	t.Setenv("SBE_TEST_LOAD_FAIL", "1")
	if err := m.Stop("alice"); err == nil {
		t.Fatal("load state query failure was hidden")
	}
}

func TestSystemdPathQuoting(t *testing.T) {
	got := unitQuote(`/absolute/a b/%x/$var/"name"`)
	if got != `"/absolute/a b/%%x/$$var/\"name\""` {
		t.Fatalf("unexpected systemd quoting %q", got)
	}
	if safePath("relative/path") || safePath("/tmp/path\nExecStart=bad") {
		t.Fatal("unsafe path accepted")
	}
}

func TestProcessIdentityParsesSpacesAndParentheses(t *testing.T) {
	fields := []string{"S", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "123456"}
	start, err := processStartTicks("99 (name with ) parentheses) " + strings.Join(fields, " "))
	if err != nil || start != "123456" {
		t.Fatalf("incorrect process identity %q: %v", start, err)
	}
	if _, err := processStartTicks("99 (name) S 1"); err == nil {
		t.Fatal("incomplete identity accepted")
	}
}
