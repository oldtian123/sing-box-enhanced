package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// UnitStates reads actual systemd states for the human-facing status pages.
// Unlike Running, it preserves transitional states and never changes services.
// An unavailable or invalid result remains unknown and is accompanied by an error.
func (m Manager) UnitStates(units []string) (map[string]string, error) {
	states := make(map[string]string, len(units))
	requested := make([]string, 0, len(units))
	for _, unit := range units {
		if !statusUnitAllowed(unit) {
			return states, fmt.Errorf("service status query does not allow unit %q", unit)
		}
		if _, exists := states[unit]; !exists {
			states[unit] = "unknown"
			requested = append(requested, unit)
		}
	}
	if len(requested) == 0 {
		return states, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := append([]string{"show", "--property=Id", "--property=LoadState", "--property=ActiveState", "--no-pager", "--"}, requested...)
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	// A child retaining the output pipe cannot extend the query indefinitely.
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return states, fmt.Errorf("query service states: %w", err)
	}

	seen := make(map[string]bool, len(requested))
	var problems []error
	for _, block := range strings.Split(strings.TrimSpace(string(output)), "\n\n") {
		fields := make(map[string]string, 3)
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				fields[key] = value
			}
		}
		unit := fields["Id"]
		if _, exists := states[unit]; !exists || seen[unit] {
			if exists {
				states[unit] = "unknown"
			}
			problems = append(problems, fmt.Errorf("unexpected service status record %q", unit))
			continue
		}
		seen[unit] = true
		if fields["LoadState"] == "not-found" {
			states[unit] = "not-found"
			continue
		}
		if fields["LoadState"] == "" {
			problems = append(problems, fmt.Errorf("missing load state for %s", unit))
			continue
		}
		switch state := fields["ActiveState"]; state {
		case "active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance", "refreshing":
			states[unit] = state
		default:
			problems = append(problems, fmt.Errorf("unknown active state %q for %s", state, unit))
		}
	}
	for _, unit := range requested {
		if !seen[unit] {
			problems = append(problems, fmt.Errorf("missing service status for %s", unit))
		}
	}
	return states, errors.Join(problems...)
}

func statusUnitAllowed(unit string) bool {
	if unit == "sing-box-enhanced-meter.service" || unit == "warp-svc.service" {
		return true
	}
	const prefix, suffix = "sbe-account-", ".service"
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, suffix) {
		return false
	}
	return accountIdentifier.MatchString(strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix))
}
