package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oldtian123/sing-box-enhanced/internal/credential"
)

const administratorCredentialsFile = "admin.initial-credentials.json"

type administratorCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func validAdministratorName(name string) bool {
	if !utf8.ValidString(name) || name == "" || strings.TrimSpace(name) != name || utf8.RuneCountInString(name) > 64 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Read interactive input before taking the accounting lock.
func runAdmin(o options, args []string, out io.Writer, in io.Reader) error {
	if len(args) == 0 {
		return errors.New("missing admin subcommand")
	}
	operation := args[0]
	name := ""
	passwordStdin := false
	if operation == "setup" {
		name = "admin"
	}
	if _, err := commandFlags("admin "+operation, args[1:], func(f *flag.FlagSet) {
		if operation == "setup" || operation == "rename" {
			f.StringVar(&name, "name", name, "application administrator name")
		}
		if operation == "setup" || operation == "passwd" {
			f.BoolVar(&passwordStdin, "password-stdin", false, "read password from standard input")
		}
	}); err != nil {
		return err
	}
	if operation != "setup" && operation != "passwd" && operation != "rename" && operation != "show" {
		return errors.New("unknown admin operation")
	}
	if (operation == "setup" || operation == "rename") && !validAdministratorName(name) {
		return errors.New("administrator name must contain 1 to 64 characters without surrounding whitespace or control characters")
	}
	password := ""
	var err error
	if operation == "setup" || operation == "passwd" {
		if passwordStdin {
			data, readErr := io.ReadAll(io.LimitReader(in, 1027))
			if readErr != nil {
				return errors.New("failed to read administrator password")
			}
			password = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
			if strings.ContainsAny(password, "\r\n") {
				return errors.New("administrator password must be a single line")
			}
		} else {
			password, err = credential.GeneratePassword()
			if err != nil {
				return err
			}
		}
	}
	return withApp(o, out, func(a *App) error {
		administrator, lookupErr := a.DB.Administrator()
		if operation == "setup" {
			if lookupErr == nil {
				return errors.New("application administrator already exists; use admin rename or admin passwd")
			}
			if !errors.Is(lookupErr, sql.ErrNoRows) {
				return lookupErr
			}
		} else if lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return errors.New("application administrator is not configured; run admin setup")
			}
			return lookupErr
		}
		path := filepath.Join(a.Root, administratorCredentialsFile)
		switch operation {
		case "show":
			return output(a.Out, map[string]string{"id": administrator.ID, "name": administrator.Name})
		case "rename":
			// Keep the one-time private credential file usable until it is removed.
			data, readErr := os.ReadFile(path)
			var saved administratorCredentials
			if readErr == nil {
				if err := json.Unmarshal(data, &saved); err != nil {
					return errors.New("invalid private administrator credential file")
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
			if err := a.DB.RenameAdministrator(name); err != nil {
				return err
			}
			if readErr == nil {
				saved.Username = name
				data, _ = json.MarshalIndent(saved, "", "  ")
				if err := writePrivate(path, append(data, '\n')); err != nil {
					return err
				}
			}
			a.audit("administrator renamed " + administrator.ID)
			return output(a.Out, map[string]string{"id": administrator.ID, "name": name})
		case "setup", "passwd":
			hash, err := credential.HashPassword(password)
			if err != nil {
				return err
			}
			id := administrator.ID
			if operation == "setup" {
				id, err = a.DB.CreateAdministrator(randomID("a-"), name, hash)
			} else {
				name = administrator.Name
				err = a.DB.SetAdministratorPassword(hash)
			}
			if err != nil {
				return err
			}
			result := map[string]any{"id": id, "name": name, "password_generated": !passwordStdin}
			if passwordStdin {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("password updated, but removing old private credential file failed: %w", err)
				}
			} else {
				data, _ := json.MarshalIndent(administratorCredentials{Username: name, Password: password}, "", "  ")
				if err := writePrivate(path, append(data, '\n')); err != nil {
					return fmt.Errorf("administrator password saved, but private credential file failed; rotate with admin passwd: %w", err)
				}
				result["credentials_file"] = path
			}
			a.audit("administrator password updated " + id)
			return output(a.Out, result)
		}
		return errors.New("unknown admin operation")
	})
}
