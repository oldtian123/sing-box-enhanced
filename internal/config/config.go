// Package config derives authenticated bindings without discarding protocol fields.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func object(data []byte) (map[string]any, error) {
	var v map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("configuration must contain exactly one JSON object")
	}
	if v == nil {
		return nil, errors.New("configuration must be a JSON object")
	}
	return v, nil
}

func array(v any, field string) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	return a, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Node templates are fragments, not complete server configurations. Until
// their root-level semantics can be merged safely, reject every other field
// instead of silently dropping routing, DNS, or management configuration.
func validateNodeFields(node map[string]any) error {
	var unsupported []string
	for field := range node {
		if field != "inbounds" && field != "outbounds" {
			unsupported = append(unsupported, field)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return fmt.Errorf("node template has unsupported root fields %s; only inbounds and outbounds may be merged, put reviewed global settings in the base configuration", strings.Join(unsupported, ", "))
	}
	return nil
}

func password(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func uuid() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func ssKeySize(method string) (int, error) {
	switch method {
	case "2022-blake3-aes-128-gcm":
		return 16, nil
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return 32, nil
	case "aes-128-gcm", "aes-192-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305":
		return 32, nil
	default:
		return 0, fmt.Errorf("unsupported authenticated Shadowsocks method %q", method)
	}
}

// Derive creates one account-specific binding and retains TLS, transport, and
// unknown fields. Reverse-proxy mappings and dynamic certificate issuance must
// be handled explicitly before these configurations can be duplicated safely.
func Derive(template []byte, accountID, bindingID string, port int) ([]byte, error) {
	if !identifier.MatchString(accountID) || !identifier.MatchString(bindingID) {
		return nil, errors.New("account and binding IDs must contain 1-64 letters, digits, underscores or hyphens")
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("binding port must be between 1 and 65535")
	}
	v, err := object(template)
	if err != nil {
		return nil, err
	}
	if err := validateNodeFields(v); err != nil {
		return nil, err
	}
	inbounds, err := array(v["inbounds"], "inbounds")
	if err != nil || len(inbounds) != 1 {
		return nil, errors.New("a new binding requires exactly one inbound template")
	}
	in, ok := inbounds[0].(map[string]any)
	if !ok {
		return nil, errors.New("inbound must be an object")
	}
	if err := validateInbound(in); err != nil {
		return nil, err
	}
	if tls, ok := in["tls"].(map[string]any); ok {
		provider, _ := tls["certificate_provider"].(map[string]any)
		if tls["acme"] != nil || str(provider["type"]) == "acme" {
			return nil, errors.New("dynamic ACME cannot be copied across account processes safely; materialize and share certificate files first")
		}
	}
	if FrontendRequired(template) {
		transport := in["transport"].(map[string]any)
		path := str(transport["path"])
		if path == "" {
			return nil, errors.New("loopback transport without a path requires an explicit account-specific Caddy host mapping")
		}
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00 \t*{}") {
			return nil, errors.New("loopback transport path is unsafe for Caddy frontend mapping")
		}
		transport["path"] = "/sbe/" + bindingID + path
	}
	in["tag"] = "sbe-" + bindingID
	in["listen_port"] = port
	name := accountID + "-" + bindingID
	users, _ := array(in["users"], "users")
	user := map[string]any{}
	if len(users) > 0 {
		for k, value := range users[0].(map[string]any) {
			user[k] = value
		}
	}
	user["name"] = name
	switch str(in["type"]) {
	case "vmess", "vless":
		user["uuid"], err = uuid()
	case "trojan", "hysteria2", "anytls":
		user["password"], err = password(32)
	case "tuic":
		user["uuid"], err = uuid()
		if err == nil {
			user["password"], err = password(32)
		}
	case "socks":
		delete(user, "name")
		user["username"] = name
		user["password"], err = password(32)
	case "shadowsocks":
		size, _ := ssKeySize(str(in["method"]))
		in["password"], err = password(size)
		if len(users) > 0 {
			if !strings.HasPrefix(str(in["method"]), "2022-") {
				return nil, errors.New("multi-user Shadowsocks requires an explicitly supported 2022 method")
			}
			if err == nil {
				user["password"], err = password(size)
			}
		} else {
			delete(in, "users")
		}
	}
	if err != nil {
		return nil, err
	}
	if str(in["type"]) != "shadowsocks" || len(users) > 0 {
		in["users"] = []any{user}
	}
	return json.MarshalIndent(v, "", "  ")
}

// FrontendRequired identifies loopback transport bindings that need a separate
// reverse-proxy mapping. Derive does not install or modify Caddy itself.
func FrontendRequired(template []byte) bool {
	v, err := object(template)
	if err != nil {
		return false
	}
	ins, err := array(v["inbounds"], "inbounds")
	if err != nil || len(ins) != 1 {
		return false
	}
	in, ok := ins[0].(map[string]any)
	if !ok {
		return false
	}
	ip := net.ParseIP(str(in["listen"]))
	_, hasTransport := in["transport"].(map[string]any)
	return ip != nil && ip.IsLoopback() && hasTransport
}

// ValidateIdentity checks that every inbound has explicit authentication.
// It never changes legacy credentials or guesses account ownership.
func ValidateIdentity(node []byte) error {
	v, err := object(node)
	if err != nil {
		return err
	}
	if err := validateNodeFields(v); err != nil {
		return err
	}
	ins, err := array(v["inbounds"], "inbounds")
	if err != nil {
		return err
	}
	if len(ins) == 0 {
		return errors.New("configuration has no authenticated inbound")
	}
	for i, item := range ins {
		in, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("inbound %d must be an object", i)
		}
		if err := validateInbound(in); err != nil {
			return fmt.Errorf("inbound %d: %w", i, err)
		}
	}
	return nil
}

func validateInbound(in map[string]any) error {
	protocol := str(in["type"])
	users, err := array(in["users"], "users")
	if err != nil {
		return err
	}
	var required []string
	switch protocol {
	case "vmess", "vless":
		required = []string{"uuid"}
	case "trojan", "hysteria2", "anytls":
		required = []string{"password"}
	case "tuic":
		required = []string{"uuid", "password"}
	case "socks":
		required = []string{"username", "password"}
	case "shadowsocks":
		size, err := ssKeySize(str(in["method"]))
		if err != nil {
			return err
		}
		if str(in["password"]) == "" {
			return errors.New("Shadowsocks requires a server password")
		}
		if strings.HasPrefix(str(in["method"]), "2022-") {
			if err := validateSSKey(str(in["password"]), size); err != nil {
				return err
			}
		}
		if len(users) == 0 {
			return nil
		}
		if !strings.HasPrefix(str(in["method"]), "2022-") {
			return errors.New("multi-user Shadowsocks requires an explicitly supported 2022 method")
		}
		required = []string{"password"}
	default:
		return fmt.Errorf("protocol %q has no supported account authentication", protocol)
	}
	if len(users) == 0 {
		return fmt.Errorf("%s requires explicit authenticated users", protocol)
	}
	for i, item := range users {
		user, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("user %d must be an object", i)
		}
		for _, field := range required {
			if str(user[field]) == "" {
				return fmt.Errorf("user %d is missing %s", i, field)
			}
		}
		if protocol == "shadowsocks" {
			size, _ := ssKeySize(str(in["method"]))
			if err := validateSSKey(str(user["password"]), size); err != nil {
				return fmt.Errorf("user %d: %w", i, err)
			}
		}
	}
	return nil
}

func validateSSKey(value string, size int) error {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) != size {
		return fmt.Errorf("Shadowsocks 2022 key must be base64 encoding of %d bytes", size)
	}
	return nil
}

// Render replaces base inbounds with the account's bindings. Exact duplicate
// outbounds are reused; the same tag with a different definition is rejected.
func Render(base []byte, nodes [][]byte, statsPort int, warpDomains []string, warpPort int) ([]byte, error) {
	if statsPort < 1 || statsPort > 65535 {
		return nil, errors.New("statistics port must be between 1 and 65535")
	}
	v, err := object(base)
	if err != nil {
		return nil, err
	}
	outs, err := array(v["outbounds"], "outbounds")
	if err != nil {
		return nil, err
	}
	tags := map[string]map[string]any{}
	var mergedOut []any
	addOutbound := func(item any) error {
		out, ok := item.(map[string]any)
		if !ok {
			return errors.New("outbound must be an object")
		}
		tag := str(out["tag"])
		if tag != "" {
			if old, exists := tags[tag]; exists {
				if reflect.DeepEqual(old, out) {
					return nil
				}
				return fmt.Errorf("conflicting outbound tag %q", tag)
			}
			tags[tag] = out
		}
		mergedOut = append(mergedOut, out)
		return nil
	}
	for _, out := range outs {
		if err := addOutbound(out); err != nil {
			return nil, err
		}
	}
	var mergedIn []any
	var inboundTags []string
	inTags := map[string]bool{}
	for i, node := range nodes {
		n, err := object(node)
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		if err := validateNodeFields(n); err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
		ins, err := array(n["inbounds"], "inbounds")
		if err != nil {
			return nil, err
		}
		if len(ins) == 0 {
			return nil, fmt.Errorf("node %d has no inbound", i)
		}
		for _, item := range ins {
			in, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("inbound must be an object")
			}
			if err := validateInbound(in); err != nil {
				return nil, err
			}
			tag := str(in["tag"])
			if tag == "" {
				return nil, errors.New("all managed inbounds require unique non-empty tags")
			}
			if inTags[tag] {
				return nil, fmt.Errorf("duplicate inbound tag %q", tag)
			}
			inTags[tag] = true
			inboundTags = append(inboundTags, tag)
			mergedIn = append(mergedIn, in)
		}
		outs, err := array(n["outbounds"], "outbounds")
		if err != nil {
			return nil, err
		}
		for _, out := range outs {
			if err := addOutbound(out); err != nil {
				return nil, err
			}
		}
	}
	if len(mergedIn) == 0 {
		return nil, errors.New("an account process requires at least one authenticated inbound")
	}
	v["inbounds"] = mergedIn
	v["outbounds"] = mergedOut
	experimental, ok := v["experimental"].(map[string]any)
	if v["experimental"] != nil && !ok {
		return nil, errors.New("experimental must be an object")
	}
	if experimental == nil {
		experimental = map[string]any{}
	}
	if clash, ok := experimental["clash_api"].(map[string]any); ok {
		if listen := str(clash["external_controller"]); listen != "" && !loopbackAddress(listen) {
			return nil, errors.New("existing Clash management API is externally accessible; disable it explicitly before adoption")
		}
	}
	if api, ok := experimental["v2ray_api"].(map[string]any); ok {
		if listen := str(api["listen"]); listen != "" && !loopbackAddress(listen) {
			return nil, errors.New("existing V2Ray management API is externally accessible; disable it explicitly before adoption")
		}
	}
	experimental["v2ray_api"] = map[string]any{
		"listen": fmt.Sprintf("127.0.0.1:%d", statsPort),
		"stats":  map[string]any{"enabled": true, "inbounds": inboundTags},
	}
	v["experimental"] = experimental
	services, err := array(v["services"], "services")
	if err != nil {
		return nil, err
	}
	if services != nil {
		for _, item := range services {
			service, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("service must be an object")
			}
			if str(service["type"]) == "api" || str(service["type"]) == "ssm-api" {
				return nil, errors.New("existing management API service requires explicit access review before adoption")
			}
		}
	}
	if len(warpDomains) > 0 {
		if warpPort < 1 || warpPort > 65535 || warpPort == statsPort {
			return nil, errors.New("WARP requires a valid local proxy port distinct from the statistics port")
		}
		domains, err := domains(warpDomains)
		if err != nil {
			return nil, err
		}
		if _, exists := tags["sbe-warp"]; exists {
			return nil, errors.New("outbound tag sbe-warp is reserved for the managed WARP proxy")
		}
		v["outbounds"] = append(mergedOut, map[string]any{"type": "socks", "tag": "sbe-warp", "server": "127.0.0.1", "server_port": warpPort, "version": "5"})
		route, ok := v["route"].(map[string]any)
		if v["route"] != nil && !ok {
			return nil, errors.New("route must be an object")
		}
		if route == nil {
			route = map[string]any{}
		}
		rules, err := array(route["rules"], "route.rules")
		if err != nil {
			return nil, err
		}
		route["rules"] = append([]any{
			map[string]any{"inbound": inboundTags, "action": "sniff"},
			map[string]any{"domain": domains, "outbound": "sbe-warp"},
		}, rules...)
		v["route"] = route
	}
	return json.MarshalIndent(v, "", "  ")
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func domains(input []string) ([]string, error) {
	seen := map[string]bool{}
	var result []string
	for _, value := range input {
		domain := strings.ToLower(strings.TrimSuffix(value, "."))
		if len(domain) == 0 || len(domain) > 253 || net.ParseIP(domain) != nil {
			return nil, fmt.Errorf("invalid exact WARP domain %q", value)
		}
		for _, label := range strings.Split(domain, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, fmt.Errorf("invalid exact WARP domain %q", value)
			}
			for _, c := range label {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					return nil, fmt.Errorf("invalid exact WARP domain %q; use ASCII or punycode without wildcards", value)
				}
			}
		}
		if !seen[domain] {
			seen[domain] = true
			result = append(result, domain)
		}
	}
	return result, nil
}
