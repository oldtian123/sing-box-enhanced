package app

import (
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/oldtian123/sing-box-enhanced/internal/caddy"
)

// Frontends share the management lock. A failed mapping never creates a usable
// binding. Quota enforcement always stops its loopback backend process too.
func (a *App) addFrontend(bindingID, nodeID string, data []byte) error {
	var template []byte
	if err := a.DB.DB.QueryRow("SELECT template FROM nodes WHERE id=?", nodeID).Scan(&template); err != nil {
		return err
	}
	mapping, err := caddy.Plan(template, data)
	if err != nil {
		return err
	}
	binary, err := exec.LookPath(env("SBE_CADDY_BIN", "caddy"))
	if err != nil {
		return errors.New("this transport requires an existing Caddy installation and site")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	site := filepath.Join(env("SBE_CADDY_SITES", "/etc/caddy/233boy"), mapping.Host+".conf")
	manager := caddy.Manager{Binary: binary, MainFile: env("SBE_CADDY_MAIN", "/etc/caddy/Caddyfile"), BackupDir: filepath.Join(a.Root, "backups", "caddy")}
	if _, err = manager.Apply(site, mapping); err != nil {
		return err
	}
	if err = systemUnit("restart", "caddy.service"); err != nil {
		_, restoreErr := manager.Remove(site, mapping.Tag)
		return errors.Join(err, restoreErr)
	}
	a.audit("frontend installed " + bindingID)
	return nil
}
