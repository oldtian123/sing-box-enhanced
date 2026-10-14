package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeStatusCommand(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SBE_STATUS_LOG\"\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SBE_STATUS_LOG", log)
	return log
}

func TestUnitStatesBatchReadOnlyAndPreserveStates(t *testing.T) {
	log := fakeStatusCommand(t, `cat <<'EOF'
Id=sing-box-enhanced-meter.service
LoadState=loaded
ActiveState=active

Id=warp-svc.service
LoadState=loaded
ActiveState=activating

Id=sbe-account-alice.service
LoadState=loaded
ActiveState=deactivating

Id=sbe-account-missing.service
LoadState=not-found
ActiveState=inactive
EOF
`)
	units := []string{"sing-box-enhanced-meter.service", "warp-svc.service", UnitName("alice"), UnitName("missing"), UnitName("alice")}
	states, err := (Manager{}).UnitStates(units)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"sing-box-enhanced-meter.service": "active",
		"warp-svc.service":                "activating",
		UnitName("alice"):                 "deactivating",
		UnitName("missing"):               "not-found",
	}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	wantCall := "show --property=Id --property=LoadState --property=ActiveState --no-pager -- sing-box-enhanced-meter.service warp-svc.service sbe-account-alice.service sbe-account-missing.service\n"
	if string(calls) != wantCall {
		t.Fatalf("status must make one read-only, deduplicated query: %q", calls)
	}
}

func TestUnitStatesUnavailableIsUnknown(t *testing.T) {
	fakeStatusCommand(t, "printf 'Id=warp-svc.service\\nLoadState=loaded\\nActiveState=active\\n'\nexit 1\n")
	states, err := (Manager{}).UnitStates([]string{"warp-svc.service"})
	if err == nil || states["warp-svc.service"] != "unknown" {
		t.Fatalf("failed command falsely returned a known state: %v, %v", states, err)
	}
}

func TestUnitStatesIncompleteResultIsUnknown(t *testing.T) {
	fakeStatusCommand(t, "printf 'Id=warp-svc.service\\nLoadState=loaded\\nActiveState=inactive\\n'\n")
	states, err := (Manager{}).UnitStates([]string{"warp-svc.service", UnitName("alice")})
	if err == nil || states["warp-svc.service"] != "inactive" || states[UnitName("alice")] != "unknown" {
		t.Fatalf("incomplete result hid a missing unit: %v, %v", states, err)
	}
}

func TestUnitStatesDoesNotCollapseActualStates(t *testing.T) {
	fakeStatusCommand(t, "printf 'Id=warp-svc.service\\nLoadState=loaded\\nActiveState=%s\\n' \"$SBE_STATUS_STATE\"\n")
	for _, state := range []string{"active", "inactive", "failed", "reloading", "activating", "deactivating", "maintenance", "refreshing"} {
		t.Setenv("SBE_STATUS_STATE", state)
		states, err := (Manager{}).UnitStates([]string{"warp-svc.service"})
		if err != nil || states["warp-svc.service"] != state {
			t.Fatalf("state %q was not preserved: %v, %v", state, states, err)
		}
	}
	t.Setenv("SBE_STATUS_STATE", "unexpected")
	states, err := (Manager{}).UnitStates([]string{"warp-svc.service"})
	if err == nil || states["warp-svc.service"] != "unknown" {
		t.Fatalf("unknown state accepted as a known state: %v, %v", states, err)
	}
}

func TestUnitStatesRejectInvalidUnitsWithoutCommand(t *testing.T) {
	log := fakeStatusCommand(t, "exit 1\n")
	for _, unit := range []string{"", "ssh.service", "--all", "sbe-account-.service", "sbe-account-../alice.service", "sbe-account-alice.service\nwarp-svc.service", UnitName(strings.Repeat("a", 65)), "sbe-account-alice;reboot.service"} {
		if _, err := (Manager{}).UnitStates([]string{"warp-svc.service", unit}); err == nil {
			t.Fatalf("unsafe unit accepted %q", unit)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("invalid units reached systemctl: %v", err)
	}
	states, err := (Manager{}).UnitStates(nil)
	if err != nil || len(states) != 0 {
		t.Fatalf("empty query = %v, %v", states, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("empty query reached systemctl: %v", err)
	}
}

func TestUnitStatesTimeoutIsBounded(t *testing.T) {
	fakeStatusCommand(t, "exec sleep 10\n")
	start := time.Now()
	states, err := (Manager{}).UnitStates([]string{"warp-svc.service"})
	if !errors.Is(err, context.DeadlineExceeded) || states["warp-svc.service"] != "unknown" {
		t.Fatalf("timeout = %v, %v", states, err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("query exceeded its deadline: %v", elapsed)
	}
}
