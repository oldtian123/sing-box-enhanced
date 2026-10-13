// Package caddy maintains only explicitly marked account frontend mappings.
package caddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var safeTag = regexp.MustCompile(`^sbe-[A-Za-z0-9_-]{1,64}$`)
var safeHost = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type Mapping struct {
	Host      string `json:"host"`
	Path      string `json:"path"`
	Backend   string `json:"backend"`
	Tag       string `json:"tag"`
	Transport string `json:"transport"`
	Snippet   string `json:"snippet"`
}

func inbound(data []byte) (map[string]any, error) {
	var value struct {
		Inbounds []map[string]any `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &value); err != nil || len(value.Inbounds) != 1 {
		return nil, errors.New("Caddy mapping requires exactly one inbound")
	}
	return value.Inbounds[0], nil
}

func text(value any) string {
	v, _ := value.(string)
	return v
}

func headerHost(transport map[string]any) (string, error) {
	if headers, ok := transport["headers"].(map[string]any); ok {
		for key, value := range headers {
			if strings.EqualFold(key, "host") {
				if host := text(value); host != "" {
					return host, nil
				}
				if values, ok := value.([]any); ok && len(values) == 1 && text(values[0]) != "" {
					return text(values[0]), nil
				}
				return "", errors.New("frontend requires one explicit Host value")
			}
		}
	}
	if values, ok := transport["host"].([]any); ok && len(values) == 1 && text(values[0]) != "" {
		return text(values[0]), nil
	}
	return "", errors.New("frontend template has no explicit Host value")
}

func validHost(host string) bool {
	if len(host) == 0 || len(host) > 253 || !safeHost.MatchString(host) || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func validPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.ContainsAny(path, "\r\n\x00 \t*{}#\"`\\")
}

// Plan derives the frontend from the original Host and the binding's actual
// path/port. It preserves any URI path prefix required by the protocol.
func Plan(template, binding []byte) (Mapping, error) {
	original, err := inbound(template)
	if err != nil {
		return Mapping{}, err
	}
	derived, err := inbound(binding)
	if err != nil {
		return Mapping{}, err
	}
	originalTransport, ok := original["transport"].(map[string]any)
	if !ok {
		return Mapping{}, errors.New("frontend template has no transport")
	}
	transport, ok := derived["transport"].(map[string]any)
	if !ok {
		return Mapping{}, errors.New("binding has no transport")
	}
	protocol := text(transport["type"])
	if protocol != "ws" && protocol != "http" && protocol != "httpupgrade" {
		return Mapping{}, fmt.Errorf("unsupported Caddy frontend transport %q", protocol)
	}
	host, err := headerHost(originalTransport)
	if err != nil || !validHost(host) {
		return Mapping{}, errors.New("Caddy frontend requires one safe DNS hostname without a port")
	}
	path := text(transport["path"])
	tag := text(derived["tag"])
	if !validPath(path) || !safeTag.MatchString(tag) {
		return Mapping{}, errors.New("binding path or tag is unsafe for Caddyfile")
	}
	ip := net.ParseIP(text(derived["listen"]))
	if ip == nil || !ip.IsLoopback() {
		return Mapping{}, errors.New("Caddy backend must listen on an explicit loopback IP")
	}
	port, ok := derived["listen_port"].(float64)
	if !ok || port < 1 || port > 65535 || port != float64(int(port)) {
		return Mapping{}, errors.New("Caddy backend port is invalid")
	}
	backend := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
	tls, _ := derived["tls"].(map[string]any)
	tlsEnabled, _ := tls["enabled"].(bool)
	if tlsEnabled {
		backend = "https://" + backend
	} else if protocol == "http" {
		backend = "h2c://" + backend
	}
	mapping := Mapping{Host: strings.ToLower(host), Path: path, Backend: backend, Tag: tag, Transport: protocol}
	mapping.Snippet, err = renderSnippet(mapping)
	return mapping, err
}

func renderSnippet(mapping Mapping) (string, error) {
	if mapping.Transport != "ws" && mapping.Transport != "http" && mapping.Transport != "httpupgrade" {
		return "", errors.New("unsupported frontend transport")
	}
	backend := strings.TrimPrefix(strings.TrimPrefix(mapping.Backend, "https://"), "h2c://")
	host, port, err := net.SplitHostPort(backend)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || ip == nil || !ip.IsLoopback() {
		return "", errors.New("frontend backend must be a loopback address and valid port")
	}
	tlsEnabled := strings.HasPrefix(mapping.Backend, "https://")
	var directive strings.Builder
	fmt.Fprintf(&directive, "    reverse_proxy %s %s", mapping.Path, mapping.Backend)
	if tlsEnabled {
		// The upstream's self-signed certificate is used only across the
		// explicitly verified loopback backend; this never applies to public
		// TLS destinations. HTTP transport requires HTTP/2 to the core.
		directive.WriteString(" {\n        transport http {\n")
		if mapping.Transport == "http" {
			directive.WriteString("            versions 2\n")
		}
		directive.WriteString("            tls_insecure_skip_verify\n        }\n    }")
	}
	return "    # BEGIN sing-box-enhanced " + mapping.Tag + "\n" + directive.String() + "\n    # END sing-box-enhanced " + mapping.Tag + "\n", nil
}

type Manager struct {
	Binary    string
	MainFile  string
	BackupDir string
}

func safeAbsolute(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\r\n\x00")
}

func (m Manager) validate(siteFile string) error {
	if !safeAbsolute(m.Binary) || !safeAbsolute(m.MainFile) || !safeAbsolute(m.BackupDir) || !safeAbsolute(siteFile) {
		return errors.New("Caddy binary, main config, backup directory, and site file require absolute paths")
	}
	if filepath.Clean(m.BackupDir) == "/" {
		return errors.New("Caddy backup directory cannot be the filesystem root")
	}
	return nil
}

func (m Manager) check(path string, full bool) error {
	args := []string{"adapt", "--config", path, "--adapter", "caddyfile", "--validate"}
	if full {
		args = []string{"validate", "--config", path, "--adapter", "caddyfile"}
	}
	if output, err := exec.Command(m.Binary, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("Caddy configuration validation failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Apply changes only the block marked with mapping.Tag. The common management
// lock must be held by its caller. It validates the site and complete config,
// retains a private backup, and rolls back on failure. Service activation is a
// separate operation because the upstream disables Caddy's administration API.
func (m Manager) Apply(siteFile string, mapping Mapping) (string, error) {
	if !validHost(mapping.Host) || !safeTag.MatchString(mapping.Tag) || !validPath(mapping.Path) {
		return "", errors.New("invalid frontend mapping")
	}
	snippet, err := renderSnippet(mapping)
	if err != nil || mapping.Snippet != snippet {
		return "", errors.New("mapping requires a generated owned fragment")
	}
	return m.edit(siteFile, func(current []byte) ([]byte, error) {
		without, _, err := remove(current, mapping.Tag)
		if err != nil {
			return nil, err
		}
		offset, err := siteOpening(without, mapping.Host)
		if err != nil {
			return nil, err
		}
		prefix, suffix := string(without[:offset]), string(without[offset:])
		if strings.HasPrefix(suffix, "\r\n") {
			prefix += "\r\n"
			suffix = suffix[2:]
		} else if strings.HasPrefix(suffix, "\n") {
			prefix += "\n"
			suffix = suffix[1:]
		} else {
			prefix += "\n"
		}
		return []byte(prefix + mapping.Snippet + suffix), nil
	})
}

func (m Manager) Remove(siteFile, tag string) (string, error) {
	if !safeTag.MatchString(tag) {
		return "", errors.New("invalid owned frontend tag")
	}
	return m.edit(siteFile, func(current []byte) ([]byte, error) {
		result, _, err := remove(current, tag)
		return result, err
	})
}

func (m Manager) edit(siteFile string, edit func([]byte) ([]byte, error)) (string, error) {
	if err := m.validate(siteFile); err != nil {
		return "", err
	}
	info, err := os.Lstat(siteFile)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("existing Caddy site file must be a regular file")
	}
	previous, err := os.ReadFile(siteFile)
	if err != nil {
		return "", err
	}
	if err := m.imported(siteFile); err != nil {
		return "", err
	}
	candidate, err := edit(previous)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(candidate)
	if string(candidate) == string(previous) {
		return hex.EncodeToString(digest[:]), nil
	}
	f, err := os.CreateTemp(filepath.Dir(siteFile), ".sbe-check-*.caddyfile")
	if err != nil {
		return "", err
	}
	checkPath := f.Name()
	defer os.Remove(checkPath)
	if _, err := f.Write(candidate); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := m.check(checkPath, false); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.BackupDir, 0700); err != nil {
		return "", err
	}
	backupInfo, err := os.Lstat(m.BackupDir)
	if err != nil || !backupInfo.IsDir() || backupInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Caddy backup directory must be a real directory")
	}
	if err := os.Chmod(m.BackupDir, 0700); err != nil {
		return "", err
	}
	oldDigest := sha256.Sum256(previous)
	backup := filepath.Join(m.BackupDir, filepath.Base(siteFile)+"-"+hex.EncodeToString(oldDigest[:])+".bak")
	if err := atomicWrite(backup, previous); err != nil {
		return "", err
	}
	if err := atomicWrite(siteFile, candidate); err != nil {
		return "", err
	}
	if err := m.check(m.MainFile, true); err != nil {
		return "", errors.Join(err, atomicWrite(siteFile, previous))
	}
	return hex.EncodeToString(digest[:]), nil
}

func (m Manager) imported(siteFile string) error {
	data, err := os.ReadFile(m.MainFile)
	if err != nil {
		return err
	}
	tokens, err := tokenize(data)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].value != "import" {
			continue
		}
		pattern := tokens[i+1].value
		if strings.HasPrefix(pattern, "\"") {
			if decoded, err := strconv.Unquote(pattern); err == nil {
				pattern = decoded
			}
		} else {
			pattern = strings.Trim(pattern, "`")
		}
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(filepath.Dir(m.MainFile), pattern)
		}
		if match, err := filepath.Match(pattern, siteFile); err == nil && match {
			return nil
		}
	}
	return errors.New("main Caddyfile does not directly import the selected site file; explicit import discovery is required")
}

func atomicWrite(path string, value []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sbe-write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(value); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func remove(current []byte, tag string) ([]byte, bool, error) {
	begin := "    # BEGIN sing-box-enhanced " + tag + "\n"
	end := "    # END sing-box-enhanced " + tag + "\n"
	value := string(current)
	start := strings.Index(value, begin)
	if start < 0 {
		if strings.Contains(value, "# END sing-box-enhanced "+tag) {
			return nil, false, errors.New("orphaned owned Caddy fragment end marker")
		}
		return current, false, nil
	}
	if start > 0 && value[start-1] != '\n' {
		return nil, false, errors.New("owned Caddy marker must begin on its own line")
	}
	finish := strings.Index(value[start+len(begin):], end)
	if finish < 0 {
		return nil, false, errors.New("incomplete owned Caddy fragment")
	}
	finish += start + len(begin) + len(end)
	if strings.Contains(value[finish:], begin) {
		return nil, false, errors.New("duplicate owned Caddy fragment")
	}
	return []byte(value[:start] + value[finish:]), true, nil
}

type token struct {
	value string
	end   int
}

// Caddyfile block braces are standalone tokens. Placeholder braces, quoted
// strings, and comments must not be counted as site block delimiters.
func tokenize(data []byte) ([]token, error) {
	var result []token
	for i := 0; i < len(data); {
		if strings.ContainsRune(" \t\r\n", rune(data[i])) {
			i++
			continue
		}
		if data[i] == '#' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			continue
		}
		start := i
		if data[i] == '"' || data[i] == '`' {
			quote := data[i]
			i++
			closed := false
			for i < len(data) {
				if quote == '"' && data[i] == '\\' {
					i += 2
					continue
				}
				if data[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, errors.New("unclosed quoted Caddyfile token")
			}
		} else {
			for i < len(data) && !strings.ContainsRune(" \t\r\n", rune(data[i])) {
				i++
			}
		}
		result = append(result, token{string(data[start:i]), i})
	}
	return result, nil
}

func siteOpening(data []byte, host string) (int, error) {
	tokens, err := tokenize(data)
	if err != nil {
		return 0, err
	}
	depth := 0
	var header []string
	offset := 0
	for _, item := range tokens {
		switch item.value {
		case "{":
			if depth == 0 {
				matches := false
				for _, value := range header {
					value = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "http://"), ",")
					if hostname, _, err := net.SplitHostPort(value); err == nil {
						value = hostname
					}
					if strings.EqualFold(value, host) {
						matches = true
					}
				}
				if matches {
					if offset != 0 {
						return 0, errors.New("multiple matching Caddy site blocks require explicit selection")
					}
					offset = item.end
				}
				header = nil
			}
			depth++
		case "}":
			depth--
			if depth < 0 {
				return 0, errors.New("unbalanced Caddyfile site blocks")
			}
		default:
			if depth == 0 {
				header = append(header, item.value)
			}
		}
	}
	if depth != 0 || offset == 0 {
		return 0, errors.New("Caddyfile must contain one identifiable site block for the frontend hostname")
	}
	return offset, nil
}
