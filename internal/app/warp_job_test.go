package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWARPJobDetachedWorkerAndOptions(t *testing.T) {
	f := newCLIFixture(t, true)
	o := options{Root: f.root, Core: f.core, UnitDir: f.unitDir}
	id := f.create(t, "faulted", "300000000000")
	s := f.open(t)
	if err := s.SetFault(id, true); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var jobDir string
	err := launchWARPJob(o, []string{"add", "--domain", "example.com"}, &out, "/private/path with spaces/sbe", func(cmd *exec.Cmd) error {
		if cmd.Stdin != nil || cmd.Stdout == nil || cmd.Stderr != cmd.Stdout || cmd.Stdout == &out {
			t.Fatal("systemd client is attached to the caller's terminal or output")
		}
		for _, value := range []string{"--wait", "--collect", "--service-type=exec", "--property=StandardInput=null", "--property=StandardOutput=null", "--property=StandardError=null", "--property=UMask=0077", "--property=User=root"} {
			if !containsString(cmd.Args, value) {
				t.Errorf("missing detached-service argument %q: %q", value, cmd.Args)
			}
		}
		sep := indexString(cmd.Args, "--")
		if sep < 0 {
			t.Fatal("missing argument separator")
		}
		want := []string{"/private/path with spaces/sbe", "--root", o.Root, "--core", o.Core, "--unit-dir", o.UnitDir, warpJobCommand}
		if !reflect.DeepEqual(cmd.Args[sep+1:sep+1+len(want)], want) {
			t.Fatalf("worker lost explicit global options: %q", cmd.Args)
		}
		workerArgs := cmd.Args[sep+1+len(want):]
		jobDir = filepath.Join(o.Root, "jobs", workerArgs[0])
		// This enters the internal worker directly and therefore must not launch
		// another systemd client or inherit the parent's output writer.
		return Run(append([]string{"--root", o.Root, "--core", o.Core, "--unit-dir", o.UnitDir, warpJobCommand}, workerArgs...), &out)
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(id, time.Now())
	if err != nil || !status.Account.StatsFault || status.Account.QuotaBytes != 300000000000 {
		t.Fatalf("worker changed account protection or quota: %+v, %v", status, err)
	}
	var domains string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='warp_domains'").Scan(&domains); err != nil || domains != `["example.com"]` {
		t.Fatalf("worker did not apply the rule: %q, %v", domains, err)
	}
	for _, path := range []string{o.Root, filepath.Join(o.Root, "jobs"), jobDir, filepath.Join(jobDir, "output.log"), filepath.Join(jobDir, "launcher.log"), filepath.Join(jobDir, "result.json")} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("job artifact is not private: %s, %v", path, err)
		}
	}
}

func TestWARPJobWorkerFailureIsPersistedAndReturned(t *testing.T) {
	f := newCLIFixture(t, false)
	o := options{Root: f.root, Core: f.core, UnitDir: f.unitDir}
	if err := os.Mkdir(o.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Root, "state.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	var dir string
	err := launchWARPJob(o, []string{"remove", "--domain", "example.com"}, &bytes.Buffer{}, "/sbe", func(cmd *exec.Cmd) error {
		i := indexString(cmd.Args, warpJobCommand)
		dir = filepath.Join(o.Root, "jobs", cmd.Args[i+1])
		return runWARPJobWorker(o, cmd.Args[i+1:])
	})
	if err == nil || !strings.Contains(err.Error(), "WARP rule job") {
		t.Fatalf("worker failure was hidden: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "result.json"))
	var result warpJobResult
	if readErr != nil || json.Unmarshal(data, &result) != nil || result.Error == "" || !strings.Contains(err.Error(), result.Error) {
		t.Fatalf("worker error was not privately persisted and propagated: %s, %v, %v", data, readErr, err)
	}
}

func TestWARPJobLaunchFailureAndUnsafeWorkerPaths(t *testing.T) {
	f := newCLIFixture(t, false)
	o := options{Root: f.root, Core: f.core, UnitDir: f.unitDir}
	err := launchWARPJob(o, []string{"add", "--domain", "example.com"}, &bytes.Buffer{}, "/sbe", func(cmd *exec.Cmd) error {
		return errors.New("fake systemd launch failure")
	})
	if err == nil || !strings.Contains(err.Error(), "fake systemd launch failure") || !strings.Contains(err.Error(), "no completion result") {
		t.Fatalf("launch failure was hidden: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Root, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("failed bootstrap ran an unsafe inline fallback: %v", err)
	}
	for _, id := range []string{"../outside", "/tmp/outside", "warp-../outside"} {
		if err := runWARPJobWorker(o, []string{id, "add", "--domain", "example.com"}); err == nil {
			t.Errorf("unsafe worker job ID accepted: %q", id)
		}
	}
	called := false
	if err := launchWARPJob(o, []string{"add", "--domain", "example.com", "extra"}, &bytes.Buffer{}, "/sbe", func(cmd *exec.Cmd) error { called = true; return nil }); err == nil || called {
		t.Fatal("invalid rule arguments reached systemd")
	}
	for _, path := range []string{"relative/sbe", "/tmp/$SBE/sbe", "/tmp/%n/sbe", "/tmp/sbe\nother", "/tmp/sbe\x00other"} {
		for _, field := range []string{"root", "core", "unit-dir", "executable"} {
			called = false
			changed, executable := o, "/sbe"
			switch field {
			case "root":
				changed.Root = path
			case "core":
				changed.Core = path
			case "unit-dir":
				changed.UnitDir = path
			case "executable":
				executable = path
			}
			if err := launchWARPJob(changed, []string{"add", "--domain", "example.com"}, &bytes.Buffer{}, executable, func(cmd *exec.Cmd) error { called = true; return nil }); err == nil || called {
				t.Errorf("unsafe %s path reached systemd: %q", field, path)
			}
		}
	}
}

func TestWARPJobWorkerRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	o := options{Root: filepath.Join(dir, "state")}
	if err := os.MkdirAll(filepath.Join(o.Root, "jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(o.Root, "jobs", "warp-safe")); err != nil {
		t.Fatal(err)
	}
	if err := runWARPJobWorker(o, []string{"warp-safe", "add", "--domain", "example.com"}); err == nil {
		t.Fatal("worker followed a symlink outside its private job directory")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("worker wrote outside its job directory: %v, %v", entries, err)
	}
}

func indexString(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func containsString(values []string, target string) bool { return indexString(values, target) >= 0 }
