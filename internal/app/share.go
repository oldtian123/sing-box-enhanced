package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/oldtian123/sing-box-enhanced/internal/runtime"
	"github.com/oldtian123/sing-box-enhanced/internal/share"
)

func (a *App) nodeShare(operation string, args []string) error {
	var account, node, server string
	var insecure bool
	_, err := commandFlags("node "+operation, args, func(f *flag.FlagSet) {
		f.StringVar(&account, "account", "", "account name or ID")
		f.StringVar(&node, "node", "", "optional node name or ID")
		f.StringVar(&server, "server", "", "public server address; explicit port for a frontend")
		f.BoolVar(&insecure, "insecure", false, "explicitly allow an untrusted TLS certificate in the client link")
	})
	if err != nil {
		return err
	}
	if account == "" || server == "" {
		return errors.New("node share requires --account and --server")
	}
	account, err = a.resolve("accounts", account)
	if err != nil {
		return err
	}
	if node != "" {
		node, err = a.resolve("nodes", node)
		if err != nil {
			return err
		}
	}
	rows, err := a.DB.DB.Query("SELECT nodes.name,bindings.inbound FROM bindings JOIN nodes ON nodes.id=bindings.node_id WHERE account_id=? AND (?='' OR node_id=?) ORDER BY nodes.name", account, node, node)
	if err != nil {
		return err
	}
	defer rows.Close()
	result := []map[string]string{}
	for rows.Next() {
		var name string
		var data []byte
		if err := rows.Scan(&name, &data); err != nil {
			return err
		}
		uri, err := share.GenerateWithOptions(data, server, name, insecure)
		if err != nil {
			return fmt.Errorf("node %s: %w", name, err)
		}
		result = append(result, map[string]string{"name": name, "url": uri})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if operation != "qr" {
		return output(a.Out, result)
	}
	binary, err := exec.LookPath("qrencode")
	if err != nil {
		return errors.New("qrencode is not installed; use node url to view the links")
	}
	for _, item := range result {
		fmt.Fprintln(a.Out, item["name"])
		command := exec.Command(binary, "-t", "ANSIUTF8")
		command.Stdin = strings.NewReader(item["url"])
		command.Stdout, command.Stderr = a.Out, io.Discard
		if err := command.Run(); err != nil {
			return fmt.Errorf("qrencode failed: %w", err)
		}
	}
	return nil
}

func (a *App) logs(args []string) error {
	var account string
	var tail int
	_, err := commandFlags("service log", args, func(f *flag.FlagSet) {
		f.StringVar(&account, "account", "", "account name or ID; defaults to the quota daemon")
		f.IntVar(&tail, "tail", 100, "number of recent lines")
	})
	if err != nil {
		return err
	}
	if tail < 1 || tail > 10000 {
		return errors.New("--tail must be between 1 and 10000")
	}
	unit := "sing-box-enhanced-meter.service"
	if account != "" {
		account, err = a.resolve("accounts", account)
		if err != nil {
			return err
		}
		unit = runtime.UnitName(account)
	}
	command := exec.Command("journalctl", "--no-pager", "--unit", unit, "--lines", fmt.Sprint(tail))
	command.Stdout, command.Stderr = a.Out, io.Discard
	return command.Run()
}
