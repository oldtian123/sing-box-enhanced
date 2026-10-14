// Package share exports an individual authenticated node, never a subscription.
package share

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var dnsPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// Generate preserves certificate verification. A local certificate filename
// alone cannot establish whether the client's trust store accepts it.
func Generate(template []byte, server, label string) (string, error) {
	return GenerateWithOptions(template, server, label, false)
}

// GenerateWithOptions exports one identity. allowInsecure is an explicit client
// choice and is never inferred from a certificate path or server IP address.
// For a Caddy frontend, server must include the known public listening port.
func GenerateWithOptions(template []byte, server, label string, allowInsecure bool) (string, error) {
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(template))
	d.UseNumber()
	if err := d.Decode(&root); err != nil || root == nil {
		return "", errors.New("node must be a JSON object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return "", errors.New("node must contain exactly one JSON object")
	}
	items, ok := root["inbounds"].([]any)
	if !ok || len(items) != 1 {
		return "", errors.New("single-node sharing requires exactly one inbound")
	}
	in, ok := items[0].(map[string]any)
	if !ok {
		return "", errors.New("inbound must be a JSON object")
	}
	protocol := text(in["type"])
	host, overridePort, err := endpoint(server)
	if err != nil {
		return "", err
	}
	port, err := number(in["listen_port"])
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("inbound has no valid listening port")
	}
	if overridePort != 0 {
		port = overridePort
	}
	transport, err := transportInfo(in)
	if err != nil {
		return "", err
	}
	tls, tlsObject := in["tls"].(map[string]any)
	if in["tls"] != nil && !tlsObject {
		return "", errors.New("TLS configuration must be an object")
	}
	tlsEnabled, _ := tls["enabled"].(bool)
	if tls["enabled"] != nil {
		if _, ok := tls["enabled"].(bool); !ok {
			return "", errors.New("TLS enabled must be a boolean")
		}
	}
	reality, _ := tls["reality"].(map[string]any)
	realityEnabled, _ := reality["enabled"].(bool)
	if realityEnabled && !tlsEnabled {
		return "", errors.New("REALITY requires TLS to be enabled")
	}
	sni := text(tls["server_name"])
	frontend := false
	if listen := net.ParseIP(text(in["listen"])); listen != nil && listen.IsLoopback() {
		if transport.kind != "ws" && transport.kind != "http" && transport.kind != "httpupgrade" {
			return "", errors.New("loopback node has no supported HTTP frontend mapping to a public endpoint")
		}
		frontend = true
		if overridePort == 0 {
			return "", errors.New("loopback/Caddy node requires an explicit public frontend port in server host:port")
		}
		frontendHost := transport.host
		if frontendHost == "" {
			frontendHost = sni
		}
		if !validDNS(frontendHost) {
			return "", errors.New("loopback/Caddy node has no unambiguous public Host or TLS server_name")
		}
		host = frontendHost
		tlsEnabled = true
	}
	if sni == "" {
		sni = transport.host
	}
	if sni == "" {
		sni = host
	}
	if tlsEnabled && !validHost(sni) {
		return "", errors.New("TLS server name is not a valid hostname or IP")
	}
	if !tlsEnabled && !frontend && sni != "" && transport.host != "" {
		// SNI is emitted only for an actual TLS layer.
		sni = ""
	}
	alpn, err := stringList(tls["alpn"], "TLS ALPN")
	if err != nil {
		return "", err
	}
	query := url.Values{}
	if tlsEnabled {
		query.Set("sni", sni)
		if len(alpn) > 0 {
			query.Set("alpn", strings.Join(alpn, ","))
		}
		if allowInsecure {
			query.Set("insecure", "1")
			query.Set("allowInsecure", "1")
		}
	}
	authority := net.JoinHostPort(host, strconv.Itoa(port))
	var user *url.Userinfo
	switch protocol {
	case "vmess", "vless", "trojan", "hysteria2", "anytls", "tuic", "socks":
		identity, err := singleUser(in)
		if err != nil {
			return "", err
		}
		if protocol == "vmess" || protocol == "vless" || protocol == "tuic" {
			if !uuidPattern.MatchString(text(identity["uuid"])) {
				return "", errors.New("node identity has no valid UUID")
			}
		}
		if protocol == "vmess" {
			if realityEnabled {
				return "", errors.New("VMess REALITY cannot be represented by the supported sharing format")
			}
			return vmess(identity, host, port, label, transport, tlsEnabled, sni, alpn, allowInsecure)
		}
		switch protocol {
		case "vless":
			user = url.User(text(identity["uuid"]))
			query.Set("encryption", "none")
			if flow := text(identity["flow"]); flow != "" {
				if flow != "xtls-rprx-vision" {
					return "", fmt.Errorf("unsupported VLESS flow %q", flow)
				}
				if !tlsEnabled || transport.kind != "tcp" {
					return "", errors.New("VLESS Vision sharing requires TCP with TLS or REALITY")
				}
				query.Set("flow", flow)
			}
			if realityEnabled {
				if text(tls["server_name"]) == "" || (transport.kind != "tcp" && transport.kind != "http") {
					return "", errors.New("REALITY requires an explicit server_name and TCP or HTTP/2 transport")
				}
				public, err := realityPublicKey(root)
				if err != nil {
					return "", err
				}
				ids, err := stringList(reality["short_id"], "REALITY short IDs")
				if err != nil || len(ids) == 0 {
					return "", errors.New("REALITY requires at least one explicit short ID, including an explicit empty ID")
				}
				if len(ids[0]) > 16 || len(ids[0])%2 != 0 {
					return "", errors.New("REALITY short ID must contain at most 16 hexadecimal characters")
				}
				if _, err := hex.DecodeString(ids[0]); err != nil {
					return "", errors.New("REALITY short ID must be hexadecimal")
				}
				query.Set("security", "reality")
				query.Set("pbk", public)
				query.Set("sid", ids[0])
				query.Set("fp", "chrome")
				query.Del("insecure")
				query.Del("allowInsecure")
			} else if tlsEnabled {
				query.Set("security", "tls")
			} else {
				query.Set("security", "none")
			}
			if err := addTransport(query, transport); err != nil {
				return "", err
			}
		case "trojan":
			if !tlsEnabled || realityEnabled {
				return "", errors.New("Trojan sharing requires an ordinary TLS or Caddy frontend")
			}
			user, err = passwordUser(identity)
			if err != nil {
				return "", err
			}
			query.Set("security", "tls")
			if err := addTransport(query, transport); err != nil {
				return "", err
			}
		case "hysteria2", "anytls", "tuic":
			if transport.kind != "tcp" || !tlsEnabled || realityEnabled {
				return "", fmt.Errorf("%s sharing requires its native transport with ordinary TLS", protocol)
			}
			if protocol == "tuic" {
				if text(identity["password"]) == "" {
					return "", errors.New("TUIC identity has no password")
				}
				user = url.UserPassword(text(identity["uuid"]), text(identity["password"]))
				if congestion := text(in["congestion_control"]); congestion != "" {
					query.Set("congestion_control", congestion)
				}
			} else {
				user, err = passwordUser(identity)
				if err != nil {
					return "", err
				}
			}
			if protocol == "tuic" || protocol == "hysteria2" {
				if len(alpn) == 0 {
					query.Set("alpn", "h3")
				}
			}
			if protocol == "hysteria2" && in["obfs"] != nil {
				obfs, ok := in["obfs"].(map[string]any)
				if !ok || text(obfs["type"]) != "salamander" || text(obfs["password"]) == "" {
					return "", errors.New("unsupported or incomplete Hysteria2 obfuscation")
				}
				query.Set("obfs", "salamander")
				query.Set("obfs-password", text(obfs["password"]))
			}
		case "socks":
			if tlsEnabled || transport.kind != "tcp" {
				return "", errors.New("SOCKS TLS or additional transport cannot be represented by the supported sharing format")
			}
			username, pass := text(identity["username"]), text(identity["password"])
			if username == "" || pass == "" || strings.Contains(username, ":") {
				return "", errors.New("SOCKS requires a nonempty username without a colon and a nonempty password")
			}
			user = url.User(base64.StdEncoding.EncodeToString([]byte(username + ":" + pass)))
		}
	case "shadowsocks":
		if tlsEnabled || transport.kind != "tcp" {
			return "", errors.New("Shadowsocks plugin/TLS transports cannot be represented by the supported sharing format")
		}
		method, pass, err := shadowsocksPassword(in)
		if err != nil {
			return "", err
		}
		user = url.User(base64.RawURLEncoding.EncodeToString([]byte(method + ":" + pass)))
		protocol = "ss"
	default:
		return "", fmt.Errorf("unsupported node protocol %q", protocol)
	}
	return (&url.URL{Scheme: protocol, User: user, Host: authority, RawQuery: query.Encode(), Fragment: label}).String(), nil
}

func text(value any) string {
	v, _ := value.(string)
	return v
}

func number(value any) (int, error) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("expected integer")
	}
	i, err := strconv.Atoi(string(n))
	return i, err
}

func validDNS(host string) bool {
	if len(host) == 0 || len(host) > 253 || !dnsPattern.MatchString(host) || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func validHost(host string) bool {
	return net.ParseIP(host) != nil || validDNS(host)
}

func endpoint(server string) (string, int, error) {
	if server == "" || strings.ContainsAny(server, "\r\n\x00 \t/?#@") {
		return "", 0, errors.New("server must be an explicit hostname or IP, optionally with a public port")
	}
	if host, port, err := net.SplitHostPort(server); err == nil {
		p, err := strconv.Atoi(port)
		if !validHost(host) || err != nil || p < 1 || p > 65535 {
			return "", 0, errors.New("server host or public port is invalid")
		}
		return host, p, nil
	}
	host := server
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if !validHost(host) {
		return "", 0, errors.New("server host is invalid; bracket an IPv6 address when specifying its port")
	}
	return host, 0, nil
}

func stringList(value any, field string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	var result []string
	for _, item := range items {
		v, ok := item.(string)
		if !ok || strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("%s must contain strings without control characters", field)
		}
		result = append(result, v)
	}
	return result, nil
}

func singleUser(in map[string]any) (map[string]any, error) {
	users, ok := in["users"].([]any)
	if !ok || len(users) != 1 {
		return nil, errors.New("single-node sharing requires exactly one explicit authentication identity")
	}
	user, ok := users[0].(map[string]any)
	if !ok {
		return nil, errors.New("authentication identity must be an object")
	}
	return user, nil
}

func passwordUser(identity map[string]any) (*url.Userinfo, error) {
	password := text(identity["password"])
	if password == "" {
		return nil, errors.New("authentication identity has no password")
	}
	return url.User(password), nil
}

type transport struct {
	kind string
	path string
	host string
}

func transportInfo(in map[string]any) (transport, error) {
	if in["transport"] == nil {
		return transport{kind: "tcp"}, nil
	}
	value, ok := in["transport"].(map[string]any)
	if !ok {
		return transport{}, errors.New("transport must be an object")
	}
	for field := range value {
		switch field {
		case "type", "path", "host", "headers", "max_early_data", "early_data_header_name", "idle_timeout", "ping_timeout", "method":
		default:
			return transport{}, fmt.Errorf("transport field %q cannot be represented in the supported sharing format", field)
		}
	}
	result := transport{kind: text(value["type"]), path: text(value["path"])}
	switch result.kind {
	case "ws", "http", "httpupgrade", "quic":
	default:
		return transport{}, fmt.Errorf("unsupported sharing transport %q", result.kind)
	}
	if result.path != "" && (!strings.HasPrefix(result.path, "/") || strings.ContainsAny(result.path, "\r\n\x00")) {
		return transport{}, errors.New("transport path is invalid")
	}
	if value["max_early_data"] != nil {
		amount, err := number(value["max_early_data"])
		if err != nil || amount != 0 {
			return transport{}, errors.New("nonzero early data requires client-specific configuration rather than this sharing format")
		}
	}
	if headers, ok := value["headers"].(map[string]any); ok {
		for name, header := range headers {
			if !strings.EqualFold(name, "host") {
				return transport{}, errors.New("custom transport headers cannot be represented in the supported sharing format")
			}
			if host := text(header); host != "" {
				result.host = host
			} else {
				values, err := stringList(header, "transport Host")
				if err != nil || len(values) != 1 {
					return transport{}, errors.New("transport Host requires exactly one explicit hostname")
				}
				result.host = values[0]
			}
		}
	} else if value["headers"] != nil {
		return transport{}, errors.New("transport headers must be an object")
	}
	if value["host"] != nil {
		hosts, err := stringList(value["host"], "transport hosts")
		if err != nil || len(hosts) != 1 || (result.host != "" && result.host != hosts[0]) {
			return transport{}, errors.New("transport hosts are ambiguous or unsupported")
		}
		result.host = hosts[0]
	}
	if result.host != "" && !validHost(result.host) {
		return transport{}, errors.New("transport Host is not a valid hostname or IP")
	}
	if method := text(value["method"]); method != "" {
		return transport{}, errors.New("custom transport HTTP method cannot be represented in the supported sharing format")
	}
	return result, nil
}

func addTransport(query url.Values, t transport) error {
	if t.kind == "quic" {
		return errors.New("QUIC sharing is supported only for the upstream VMess format")
	}
	kind := t.kind
	if kind == "http" {
		kind = "h2"
	}
	query.Set("type", kind)
	if t.path != "" {
		query.Set("path", t.path)
	}
	if t.host != "" {
		query.Set("host", t.host)
	}
	return nil
}

func vmess(user map[string]any, host string, port int, label string, t transport, tls bool, sni string, alpn []string, insecure bool) (string, error) {
	if t.kind == "quic" && !tls {
		return "", errors.New("VMess QUIC sharing requires explicit TLS")
	}
	netType, headerType := t.kind, "none"
	if t.kind == "http" {
		if t.host == "" && !tls {
			netType, headerType = "tcp", "http"
		} else {
			netType = "h2"
		}
	}
	aid := 0
	if user["alter_id"] != nil {
		var err error
		aid, err = number(user["alter_id"])
		if err != nil || aid < 0 {
			return "", errors.New("VMess alter_id must be a nonnegative integer")
		}
	}
	v := map[string]any{"v": "2", "ps": label, "add": host, "port": strconv.Itoa(port), "id": text(user["uuid"]), "aid": strconv.Itoa(aid), "net": netType, "type": headerType}
	if t.host != "" {
		v["host"] = t.host
	}
	if t.path != "" {
		v["path"] = t.path
	}
	if tls {
		v["tls"] = "tls"
		v["sni"] = sni
		if len(alpn) > 0 {
			v["alpn"] = strings.Join(alpn, ",")
		} else if t.kind == "quic" {
			v["alpn"] = "h3"
		}
		if insecure {
			v["allowInsecure"] = true
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(data), nil
}

func realityPublicKey(root map[string]any) (string, error) {
	items, ok := root["outbounds"].([]any)
	if !ok {
		return "", errors.New("REALITY public key metadata is missing; private keys are never exported")
	}
	public := ""
	for _, item := range items {
		out, ok := item.(map[string]any)
		if !ok || !strings.HasPrefix(text(out["tag"]), "public_key_") {
			continue
		}
		key := strings.TrimPrefix(text(out["tag"]), "public_key_")
		decoded, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil || len(decoded) != 32 {
			return "", errors.New("REALITY public key metadata is invalid")
		}
		if public != "" && public != key {
			return "", errors.New("REALITY public key metadata is ambiguous")
		}
		public = key
	}
	if public == "" {
		return "", errors.New("REALITY public key metadata is missing; private keys are never exported")
	}
	return public, nil
}

func shadowsocksPassword(in map[string]any) (string, string, error) {
	method, pass := text(in["method"]), text(in["password"])
	size := 0
	switch method {
	case "2022-blake3-aes-128-gcm":
		size = 16
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		size = 32
	case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305":
	default:
		return "", "", fmt.Errorf("unsupported Shadowsocks method %q", method)
	}
	if pass == "" {
		return "", "", errors.New("Shadowsocks password is missing")
	}
	if size != 0 {
		if data, err := base64.StdEncoding.DecodeString(pass); err != nil || len(data) != size {
			return "", "", errors.New("Shadowsocks 2022 server key has the wrong encoding or length")
		}
	}
	if in["users"] != nil {
		users, ok := in["users"].([]any)
		if !ok {
			return "", "", errors.New("Shadowsocks users must be an array")
		}
		if len(users) > 0 {
			if size == 0 || method == "2022-blake3-chacha20-poly1305" {
				return "", "", errors.New("this Shadowsocks method has no supported multi-user sharing format")
			}
			user, err := singleUser(in)
			if err != nil {
				return "", "", err
			}
			key := text(user["password"])
			if data, err := base64.StdEncoding.DecodeString(key); err != nil || len(data) != size {
				return "", "", errors.New("Shadowsocks 2022 user key has the wrong encoding or length")
			}
			pass += ":" + key
		}
	}
	return method, pass, nil
}
