package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/oldtian123/sing-box-enhanced/internal/warp"
)

const warpJobCommand = "_warp-rule-job"

var warpJobID = regexp.MustCompile(`^warp-[A-Za-z0-9]+$`)

type warpJobResult struct {
	Error string `json:"error,omitempty"`
}

// A rule change may restart the proxy carrying its own SSH session. Keep the
// actual change in a system service with no terminal or inherited output pipe.
func runWARPJob(o options, args []string, out io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return launchWARPJob(o, args, out, executable, func(cmd *exec.Cmd) error { return cmd.Run() })
}

func warpJobArgs(args []string) ([]string, error) {
	if len(args) == 0 || (args[0] != "add" && args[0] != "remove") {
		return nil, errors.New("WARP rule job only supports add and remove")
	}
	var domain string
	if _, err := commandFlags("warp "+args[0], args[1:], func(f *flag.FlagSet) {
		f.StringVar(&domain, "domain", "", "exact destination domain")
	}); err != nil {
		return nil, err
	}
	domain, err := warp.ValidateDomain(domain)
	if err != nil {
		return nil, err
	}
	return []string{args[0], "--domain", domain}, nil
}

func privateWARPJobDir(path string, create bool) error {
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("WARP job directories must be private (0700) and not symlinks")
	}
	return nil
}

func launchWARPJob(o options, args []string, out io.Writer, executable string, run func(*exec.Cmd) error) error {
	args, err := warpJobArgs(args)
	if err != nil {
		return err
	}
	for _, path := range []string{o.Root, o.Core, o.UnitDir, executable} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "$%") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return errors.New("WARP job root, core, unit-dir and executable must be absolute paths without $, % or control characters")
		}
	}
	for _, path := range []string{o.Root, filepath.Join(o.Root, "jobs")} {
		if err := privateWARPJobDir(path, true); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(filepath.Join(o.Root, "jobs"), "warp-")
	if err != nil {
		return err
	}
	id := filepath.Base(dir)
	command := []string{
		"--quiet", "--wait", "--collect", "--service-type=exec",
		"--unit=sbe-" + id,
		"--property=User=root", "--property=UMask=0077",
		"--property=StandardInput=null", "--property=StandardOutput=null", "--property=StandardError=null",
		"--", executable, "--root", o.Root, "--core", o.Core, "--unit-dir", o.UnitDir, warpJobCommand, id,
	}
	command = append(command, args...)
	launcherLog, err := os.OpenFile(filepath.Join(dir, "launcher.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// The waiting client also writes to a private file, never to the SSH pipe.
	cmd := exec.Command("systemd-run", command...)
	cmd.Stdout, cmd.Stderr = launcherLog, launcherLog
	waitErr := errors.Join(run(cmd), launcherLog.Close())
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return fmt.Errorf("WARP rule job %s has no completion result (systemd-run: %v): %w; inspect %s", id, waitErr, err, dir)
	}
	var result warpJobResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("read WARP rule job %s result: %w", id, err)
	}
	log, err := os.Open(filepath.Join(dir, "output.log"))
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, log)
	closeErr := log.Close()
	if result.Error != "" {
		return fmt.Errorf("WARP rule job %s: %s", id, result.Error)
	}
	if waitErr != nil {
		return fmt.Errorf("waiting for WARP rule job %s: %w", id, waitErr)
	}
	return errors.Join(copyErr, closeErr)
}

func runWARPJobWorker(o options, args []string) error {
	if len(args) < 2 || !warpJobID.MatchString(args[0]) {
		return errors.New("invalid WARP rule job ID or operation")
	}
	operation, err := warpJobArgs(args[1:])
	if err != nil {
		return err
	}
	dir := filepath.Join(o.Root, "jobs", args[0])
	for _, path := range []string{o.Root, filepath.Join(o.Root, "jobs"), dir} {
		if err := privateWARPJobDir(path, false); err != nil {
			return err
		}
	}
	log, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	operationErr := withApp(o, log, func(a *App) error { return a.warp(operation) })
	operationErr = errors.Join(operationErr, log.Close())
	result := warpJobResult{}
	if operationErr != nil {
		result.Error = operationErr.Error()
	}
	data, err := json.Marshal(result)
	if err != nil {
		return errors.Join(operationErr, err)
	}
	// Rename publishes a complete result even if the caller disappeared while
	// the account instances were being restarted.
	file, err := os.CreateTemp(dir, ".result-")
	if err != nil {
		return errors.Join(operationErr, err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(operationErr, err)
	}
	return errors.Join(operationErr, os.Rename(file.Name(), filepath.Join(dir, "result.json")))
}
