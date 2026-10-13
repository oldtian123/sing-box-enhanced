// Package runtime manages only account-owned sing-box instances.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var accountIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Manager writes root-owned configurations and systemd units. Apply does not
// start an account: the caller must first check its persisted access state.
type Manager struct {
	Root    string
	Core    string
	UnitDir string
}

func UnitName(accountID string) string {
	return "sbe-account-" + accountID + ".service"
}

func safePath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\r\n\x00")
}

func (m Manager) validate(accountID string) error {
	if !accountIdentifier.MatchString(accountID) {
		return errors.New("account ID must contain 1-64 letters, digits, underscores or hyphens")
	}
	if !safePath(m.Root) || !safePath(m.Core) || !safePath(m.UnitDir) {
		return errors.New("runtime root, core, and systemd unit directory must be absolute paths without control characters")
	}
	if filepath.Clean(m.Root) == string(filepath.Separator) || filepath.Clean(m.UnitDir) == string(filepath.Separator) {
		return errors.New("runtime and unit directories cannot be the filesystem root")
	}
	return nil
}

func (m Manager) instanceDir(accountID string) string {
	return filepath.Join(m.Root, "instances", accountID)
}

// Check validates a configuration with the actual installed core.
func (m Manager) Check(configPath string) error {
	if !safePath(m.Core) || !safePath(configPath) {
		return errors.New("core and configuration paths must be absolute without control characters")
	}
	output, err := exec.Command(m.Core, "check", "-c", configPath).CombinedOutput()
	if err != nil {
		// Core diagnostics can contain private values; callers should keep this
		// error on their local administrative channel rather than public logs.
		return fmt.Errorf("sing-box configuration check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m Manager) unit(accountID string) []byte {
	configPath := filepath.Join(m.instanceDir(accountID), "config.json")
	return []byte("[Unit]\nDescription=sing-box enhanced account " + accountID + "\nBindsTo=sing-box-enhanced-meter.service\nAfter=network-online.target sing-box-enhanced-meter.service\n\n[Service]\nType=simple\nUser=root\nUMask=0077\nExecStart=" + unitQuote(m.Core) + " run -c " + unitQuote(configPath) + "\nRestart=no\nKillMode=control-group\nTimeoutStopSec=10\nSendSIGKILL=yes\n\n")
}

// systemd command lines are not shell commands. Percent specifiers and quoting
// must still be escaped so an absolute filename remains a literal filename.
func unitQuote(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "$", "$$")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}

func directory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime directory %q must be a real directory", path)
	}
	return os.Chmod(path, mode)
}

func existingFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("managed file %q must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sbe-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func restore(path string, data []byte, exists bool) error {
	if exists {
		return atomicWrite(path, data)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func systemctl(args ...string) ([]byte, error) {
	output, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// Apply validates a private candidate, retains the prior configuration, replaces
// config and unit atomically, and reloads systemd. Failure restores both files.
// It does not start, restart, enable, or change any other service.
func (m Manager) Apply(accountID string, config []byte) (string, error) {
	if err := m.validate(accountID); err != nil {
		return "", err
	}
	for _, path := range []string{m.Root, filepath.Join(m.Root, "instances"), m.instanceDir(accountID)} {
		if err := directory(path, 0700); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(m.UnitDir, 0755); err != nil {
		return "", err
	}
	unitDirInfo, err := os.Lstat(m.UnitDir)
	if err != nil || !unitDirInfo.IsDir() || unitDirInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("systemd unit directory must be a real directory")
	}
	dir := m.instanceDir(accountID)
	candidate, err := os.CreateTemp(dir, ".candidate-*.json")
	if err != nil {
		return "", err
	}
	candidatePath := candidate.Name()
	defer os.Remove(candidatePath)
	if err := candidate.Chmod(0600); err != nil {
		candidate.Close()
		return "", err
	}
	if _, err := candidate.Write(config); err != nil {
		candidate.Close()
		return "", err
	}
	if err := candidate.Sync(); err != nil {
		candidate.Close()
		return "", err
	}
	if err := candidate.Close(); err != nil {
		return "", err
	}
	if err := m.Check(candidatePath); err != nil {
		return "", err
	}
	configPath := filepath.Join(dir, "config.json")
	unitPath := filepath.Join(m.UnitDir, UnitName(accountID))
	oldConfig, configExists, err := existingFile(configPath)
	if err != nil {
		return "", err
	}
	oldUnit, unitExists, err := existingFile(unitPath)
	if err != nil {
		return "", err
	}
	if configExists {
		if err := atomicWrite(filepath.Join(dir, "config.json.previous"), oldConfig); err != nil {
			return "", err
		}
	}
	rollback := func(cause error) error {
		configErr := restore(configPath, oldConfig, configExists)
		unitErr := restore(unitPath, oldUnit, unitExists)
		_, reloadErr := systemctl("daemon-reload")
		return errors.Join(cause, configErr, unitErr, reloadErr)
	}
	if err := os.Rename(candidatePath, configPath); err != nil {
		return "", err
	}
	if err := syncDirectory(dir); err != nil {
		return "", rollback(err)
	}
	if err := atomicWrite(unitPath, m.unit(accountID)); err != nil {
		return "", rollback(err)
	}
	if _, err := systemctl("daemon-reload"); err != nil {
		return "", rollback(err)
	}
	digest := sha256.Sum256(config)
	return hex.EncodeToString(digest[:]), nil
}

func (m Manager) Start(accountID string) error {
	if err := m.validate(accountID); err != nil {
		return err
	}
	_, err := systemctl("start", UnitName(accountID))
	return err
}

func (m Manager) Stop(accountID string) error {
	if err := m.validate(accountID); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(m.UnitDir, UnitName(accountID)))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("account unit file must be a regular file")
	}
	output, err := systemctl("show", "--property=LoadState", "--value", UnitName(accountID))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) == "not-found" {
		return nil
	}
	_, err = systemctl("stop", UnitName(accountID))
	return err
}

func (m Manager) Running(accountID string) (bool, error) {
	if err := m.validate(accountID); err != nil {
		return false, err
	}
	info, err := os.Lstat(filepath.Join(m.UnitDir, UnitName(accountID)))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("account unit file must be a regular file")
	}
	output, err := systemctl("show", "--property=ActiveState", "--value", UnitName(accountID))
	if err != nil {
		return false, err
	}
	state := strings.TrimSpace(string(output))
	switch state {
	case "active", "reloading", "activating", "deactivating", "refreshing":
		return true, nil
	case "inactive", "failed", "maintenance":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected account service active state %q", state)
	}
}

// ProcessID includes kernel boot identity, PID, and start ticks. A restart or
// reboot therefore never reuses the old counter checkpoint identity.
func (m Manager) ProcessID(accountID string) (string, error) {
	if err := m.validate(accountID); err != nil {
		return "", err
	}
	output, err := systemctl("show", "--property=MainPID", "--value", UnitName(accountID))
	if err != nil {
		return "", err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 0 {
		return "", errors.New("account process is not running")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", fmt.Errorf("read account process identity: %w", err)
	}
	start, err := processStartTicks(string(data))
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read kernel boot identity: %w", err)
	}
	return strings.TrimSpace(string(boot)) + ":" + strconv.Itoa(pid) + ":" + start, nil
}

func processStartTicks(stat string) (string, error) {
	// The comm field can contain spaces and parentheses. Everything after the
	// final ')' begins at field 3; starttime is field 22, hence offset 19.
	close := strings.LastIndex(stat, ")")
	if close < 0 {
		return "", errors.New("invalid process stat identity")
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) <= 19 {
		return "", errors.New("incomplete process stat identity")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", errors.New("invalid process start time")
	}
	return fields[19], nil
}
