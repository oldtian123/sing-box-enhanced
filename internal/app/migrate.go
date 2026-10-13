package app

import (
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/oldtian123/sing-box-enhanced/internal/config"
)

type legacyNode struct {
	Path, Name, ID string
	Data           []byte
	Authenticated  bool
}
type legacyPlan struct {
	Root, Unit string
	Base       []byte
	Nodes      []legacyNode
}

var unitIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`)

func discoverLegacy(root, unit string) (legacyPlan, error) {
	p := legacyPlan{Root: root, Unit: unit}
	if !unitIdentifier.MatchString(unit) {
		return p, errors.New("invalid legacy service unit")
	}
	if root == "" {
		out, err := exec.Command("systemctl", "show", "--property=ExecStart", "--value", unit).Output()
		if err != nil {
			return p, err
		}
		re := regexp.MustCompile(`(?:^|\s)-c\s+([^\s;]+)`)
		m := re.FindStringSubmatch(string(out))
		if len(m) != 2 {
			return p, errors.New("cannot discover base configuration; provide --legacy-root")
		}
		root = filepath.Dir(m[1])
		p.Root = root
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
		return p, errors.New("legacy root must be an absolute non-root directory")
	}
	var err error
	p.Base, err = os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		return p, err
	}
	if !json.Valid(p.Base) {
		return p, errors.New("invalid legacy base JSON")
	}
	// Inbounds in config.json are identities too; do not silently discard them.
	var base map[string]json.RawMessage
	if err = json.Unmarshal(p.Base, &base); err != nil {
		return p, err
	}
	var baseInbounds []json.RawMessage
	if err = json.Unmarshal(base["inbounds"], &baseInbounds); len(base["inbounds"]) > 0 && err != nil {
		return p, err
	}
	for i, in := range baseInbounds {
		value := map[string]json.RawMessage{"inbounds": json.RawMessage("[" + string(in) + "]")}
		data, e := json.Marshal(value)
		if e != nil {
			return p, e
		}
		sum := sha256.Sum256([]byte(filepath.Join(root, "config.json") + fmt.Sprint(i)))
		p.Nodes = append(p.Nodes, legacyNode{Path: filepath.Join(root, "config.json"), Name: fmt.Sprintf("legacy-base-%d", i+1), ID: "old-" + hex.EncodeToString(sum[:8]), Data: data, Authenticated: config.ValidateIdentity(data) == nil})
	}
	files, err := filepath.Glob(filepath.Join(root, "conf", "*.json"))
	if err != nil {
		return p, err
	}
	for _, path := range files {
		data, e := os.ReadFile(path)
		if e != nil {
			return p, e
		}
		if !json.Valid(data) {
			return p, fmt.Errorf("invalid legacy JSON: %s", filepath.Base(path))
		}
		sum := sha256.Sum256([]byte(path))
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		p.Nodes = append(p.Nodes, legacyNode{Path: path, Name: name, ID: "old-" + hex.EncodeToString(sum[:8]), Data: data, Authenticated: config.ValidateIdentity(data) == nil})
	}
	if len(p.Nodes) == 0 {
		return p, errors.New("no legacy nodes found")
	}
	return p, nil
}

func (a *App) migrate(args []string) error {
	if os.Getenv("SBE_DEVELOPER") != "1" {
		return errors.New("migration is available only with SBE_DEVELOPER=1")
	}
	var root, owner, unit, entry string
	_, err := commandFlags("migrate", args[1:], func(f *flag.FlagSet) {
		f.StringVar(&root, "legacy-root", env("SBE_LEGACY_ROOT", ""), "legacy installation directory")
		f.StringVar(&owner, "owner", "", "explicit account owner for imported identities")
		f.StringVar(&unit, "legacy-unit", "sing-box.service", "legacy systemd service")
		f.StringVar(&entry, "takeover-entry", "", "optional legacy sb entry to replace after successful takeover")
	})
	if err != nil {
		return err
	}
	if args[0] == "run" {
		if owner == "" {
			return errors.New("one-step migration requires an explicit --owner")
		}
		out := a.Out
		a.Out = io.Discard
		err = a.migrate(append([]string{"import"}, args[1:]...))
		a.Out = out
		if err != nil {
			return err
		}
		return a.applyMigration(entry)
	}
	switch args[0] {
	case "inspect", "import":
		p, e := discoverLegacy(root, unit)
		if e != nil {
			return e
		}
		summary := []map[string]any{}
		for _, n := range p.Nodes {
			summary = append(summary, map[string]any{"id": n.ID, "name": n.Name, "authenticated": n.Authenticated})
		}
		if args[0] == "inspect" {
			return output(a.Out, map[string]any{"legacy_root": p.Root, "legacy_unit": p.Unit, "nodes": summary, "changes_applied": false})
		}
		if a.setting("migration_active") == "1" {
			return errors.New("migration already active")
		}
		if a.setting("migration_imported") == "1" {
			return errors.New("legacy configuration is already imported; bind its existing identities before apply")
		}
		if owner != "" {
			owner, err = a.resolve("accounts", owner)
			if err != nil {
				return err
			}
		}
		backup := filepath.Join(a.Root, "backups", "migration-"+time.Now().UTC().Format("20060102T150405.000000000"))
		if err = os.MkdirAll(backup, 0700); err != nil {
			return err
		}
		if err = writePrivate(filepath.Join(backup, "config.json"), p.Base); err != nil {
			return err
		}
		for _, n := range p.Nodes {
			if err = writePrivate(filepath.Join(backup, n.ID+".json"), n.Data); err != nil {
				return err
			}
		}
		if err = writePrivate(filepath.Join(a.Root, "base.json"), p.Base); err != nil {
			return err
		}
		tx, e := a.DB.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, n := range p.Nodes {
			if _, e = tx.Exec("INSERT INTO nodes(id,name,template,legacy) VALUES(?,?,?,1)", n.ID, n.Name, n.Data); e != nil {
				return e
			}
			if owner != "" && n.Authenticated {
				if _, e = tx.Exec("INSERT INTO bindings(id,account_id,node_id,inbound) VALUES(?,?,?,?)", randomID("b-"), owner, n.ID, n.Data); e != nil {
					return e
				}
			}
		}
		for k, v := range map[string]string{"legacy_root": p.Root, "legacy_unit": p.Unit, "migration_backup": backup, "migration_imported": "1", "paused": "1"} {
			if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); e != nil {
				return e
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		a.audit("legacy configuration imported; service unchanged")
		return output(a.Out, map[string]any{"nodes": summary, "owner": owner, "backup": backup, "legacy_service_unchanged": true, "unowned_will_be_disabled_on_apply": true})
	case "apply":
		return a.applyMigration(entry)
	case "rollback":
		return a.rollbackMigration()
	default:
		return errors.New("unknown migration operation")
	}
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".sbe-write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(data); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func systemUnit(action, unit string) error {
	if !unitIdentifier.MatchString(unit) {
		return errors.New("invalid service name")
	}
	out, e := exec.Command("systemctl", action, unit).CombinedOutput()
	if e != nil {
		return fmt.Errorf("legacy systemctl %s failed: %w: %s", action, e, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *App) prepareMigration() error {
	accounts, err := a.DB.Accounts()
	if err != nil {
		return err
	}
	base, err := os.ReadFile(filepath.Join(a.Root, "base.json"))
	if err != nil {
		return err
	}
	domains, err := a.domains()
	if err != nil {
		return err
	}
	for _, v := range accounts {
		rows, e := a.DB.DB.Query("SELECT inbound FROM bindings WHERE account_id=?", v.ID)
		if e != nil {
			return e
		}
		nodes := [][]byte{}
		for rows.Next() {
			var b []byte
			if e = rows.Scan(&b); e != nil {
				rows.Close()
				return e
			}
			nodes = append(nodes, b)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(nodes) == 0 {
			continue
		}
		port, e := freePort(30000)
		if e != nil {
			return e
		}
		var used int
		for {
			if e = a.DB.DB.QueryRow("SELECT COUNT(*) FROM instances WHERE stats_port=?", port).Scan(&used); e != nil {
				return e
			}
			if used == 0 {
				break
			}
			port, e = freePort(port + 1)
			if e != nil {
				return e
			}
		}
		var oldPort int
		if e = a.DB.DB.QueryRow("SELECT stats_port FROM instances WHERE account_id=?", v.ID).Scan(&oldPort); e == nil {
			port = oldPort
		}
		data, e := config.Render(base, nodes, port, domains, 40000)
		if e != nil {
			return e
		}
		hash, e := a.Manager.Apply(v.ID, data)
		if e != nil {
			return e
		}
		_, e = a.DB.DB.Exec("INSERT INTO instances(account_id,stats_port,generation,config_hash) VALUES(?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET config_hash=excluded.config_hash", v.ID, port, randomID("g-"), hash)
		if e != nil {
			return e
		}
	}
	return nil
}

func (a *App) applyMigration(entry string) error {
	if a.setting("migration_imported") != "1" {
		return errors.New("inspect and import legacy configuration first")
	}
	if a.setting("migration_active") == "1" {
		return errors.New("migration already applied")
	}
	if entry != "" {
		if err := a.checkTakeoverEntry(entry); err != nil {
			return err
		}
	}
	if err := a.prepareMigration(); err != nil {
		return err
	}
	unit := a.setting("legacy_unit")
	if !unitIdentifier.MatchString(unit) {
		return errors.New("invalid legacy unit")
	}
	if err := systemUnit("stop", unit); err != nil {
		return err
	}
	if err := systemUnit("mask", unit); err != nil {
		return errors.Join(err, a.rollbackMigration())
	}
	if err := a.set("paused", "0"); err != nil {
		return errors.Join(err, a.rollbackMigration())
	}
	if err := a.reconcile(); err != nil {
		_ = a.set("paused", "1")
		_ = a.stopAll(false)
		return errors.Join(err, a.rollbackMigration())
	}
	if entry != "" {
		if err := a.takeoverEntry(entry); err != nil {
			_ = a.set("paused", "1")
			_ = a.stopAll(false)
			return err
		}
	}
	if err := a.set("migration_active", "1"); err != nil {
		_ = a.set("paused", "1")
		_ = a.stopAll(false)
		return err
	}
	if err := writePrivate(filepath.Join(a.Root, "managed.marker"), []byte("managed-v1\n")); err != nil {
		return err
	}
	a.audit("legacy migration applied; original unit masked")
	return output(a.Out, map[string]any{"migration_active": true, "legacy_unit": unit, "unowned_identities": "disabled", "entry_replaced": entry, "backup": a.setting("migration_backup")})
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (a *App) checkTakeoverEntry(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return errors.New("takeover entry must be an absolute file path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	legacy := a.setting("legacy_root")
	script := filepath.Join(legacy, "sh", "sing-box.sh")
	canonicalScript, err := filepath.EvalSymlinks(script)
	if err != nil {
		return errors.New("recognized legacy script is missing")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Only the recognized old entry may be replaced; unrelated commands stay intact.
	if resolved != canonicalScript && !strings.Contains(string(data), script) {
		return errors.New("entry does not point to the recognized legacy script")
	}
	return nil
}
func (a *App) takeoverEntry(path string) error {
	if err := a.checkTakeoverEntry(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backup := a.setting("migration_backup")
	legacyScript, err := filepath.EvalSymlinks(filepath.Join(a.setting("legacy_root"), "sh", "sing-box.sh"))
	if err != nil {
		return err
	}
	originalScript, err := os.ReadFile(legacyScript)
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(backup, "legacy-script.original"), originalScript); err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(backup, "sb-entry.original"), data); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(path)
		if e != nil {
			return e
		}
		if e = writePrivate(filepath.Join(backup, "sb-entry.symlink"), []byte(target)); e != nil {
			return e
		}
	}
	content := "#!/bin/bash\n"
	for _, pair := range [][2]string{{"SBE_ROOT", a.Root}, {"SBE_CORE", a.Core}, {"SBE_BIN", filepath.Join(a.Root, "bin", "sbe")}, {"SBE_SH_ROOT", filepath.Join(a.Root, "sh")}, {"SBE_UNIT_DIR", a.UnitDir}} {
		content += "export " + pair[0] + "=" + shellQuote(pair[1]) + "\n"
	}
	content += "exec bash " + shellQuote(filepath.Join(a.Root, "sh", "scripts", "sb-enhanced.sh")) + " \"$@\"\n"
	// Upstream aliases and its second command symlink share this script target.
	// Guard that target too, so those entry points cannot launch an unmetered core.
	if err = writePrivate(legacyScript, []byte(content)); err != nil {
		return err
	}
	if err = os.Chmod(legacyScript, 0700); err != nil {
		return err
	}
	if err = writePrivate(path, []byte(content)); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func (a *App) rollbackMigration() error {
	if a.setting("migration_imported") != "1" {
		return errors.New("no migration to roll back")
	}
	if err := a.set("paused", "1"); err != nil {
		return err
	}
	if err := a.stopAll(false); err != nil {
		return err
	}
	var unowned int
	if err := a.DB.DB.QueryRow("SELECT COUNT(*) FROM nodes WHERE legacy=1 AND NOT EXISTS(SELECT 1 FROM bindings WHERE node_id=nodes.id)").Scan(&unowned); err != nil {
		return err
	}
	accounts, err := a.DB.Accounts()
	if err != nil {
		return err
	}
	safe := unowned == 0
	for _, v := range accounts {
		s, e := a.DB.Status(v.ID, time.Now())
		if e != nil {
			return e
		}
		if !s.Allowed {
			safe = false
		}
	}
	if !safe {
		a.audit("migration rollback kept legacy stopped to prevent quota bypass")
		return errors.New("legacy service remains stopped: unowned or restricted identities would bypass quota; configuration backup is preserved")
	}
	// Restoring an unmetered service after successful managed use is unsafe even
	// for accounts currently below quota. Explicit release back to legacy is not
	// part of automated rollback; failed cutovers may restore their prior state.
	if a.setting("migration_active") == "1" {
		return errors.New("active migration rollback preserves the traffic ledger and leaves legacy stopped; use configuration rollback within enhanced management")
	}
	unit := a.setting("legacy_unit")
	if err = systemUnit("unmask", unit); err != nil {
		return err
	}
	if err = systemUnit("start", unit); err != nil {
		return err
	}
	a.audit("failed cutover restored prior legacy service")
	return output(a.Out, map[string]any{"legacy_restored": true, "traffic_ledger_preserved": true})
}
