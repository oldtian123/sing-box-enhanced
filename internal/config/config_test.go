package config

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDeriveRetainsTLSAndTransport(t *testing.T) {
	template := []byte(`{"inbounds":[{"type":"vless","tag":"old","listen":"::","listen_port":443,"users":[{"uuid":"old","flow":"xtls-rprx-vision"}],"tls":{"enabled":true,"server_name":"example.com","reality":{"enabled":true,"private_key":"keep","short_id":["abc"]}},"transport":{"type":"http","path":"/old"},"future_field":{"counter":9007199254740993}}],"outbounds":[{"type":"direct","tag":"public_key_keep"}]}`)
	derived, err := Derive(template, "alice", "bind-1", 24443)
	if err != nil {
		t.Fatal(err)
	}
	v := decode(t, derived)
	in := v["inbounds"].([]any)[0].(map[string]any)
	u := in["users"].([]any)[0].(map[string]any)
	if in["tag"] != "sbe-bind-1" || in["listen_port"] != float64(24443) || u["uuid"] == "old" || u["flow"] != "xtls-rprx-vision" {
		t.Fatalf("binding derivation lost fields: %s", derived)
	}
	if len(u["uuid"].(string)) != 36 || !strings.Contains(string(derived), "9007199254740993") || !strings.Contains(string(derived), "public_key_keep") || !strings.Contains(string(derived), `"private_key": "keep"`) {
		t.Fatalf("unknown or credential fields were not preserved: %s", derived)
	}
	if strings.Contains(string(template), "sbe-bind-1") {
		t.Fatal("template was mutated")
	}
}

func TestDeriveAllUpstreamProtocols(t *testing.T) {
	for _, protocol := range []string{"vmess", "vless", "trojan", "hysteria2", "anytls", "tuic", "socks"} {
		t.Run(protocol, func(t *testing.T) {
			template := []byte(`{"inbounds":[{"type":"` + protocol + `","listen_port":1000,"users":[{"uuid":"old","password":"old","username":"old"}],"tls":{"certificate_path":"/cert","key_path":"/key"}}]}`)
			a, err := Derive(template, "account", "a", 1001)
			if err != nil {
				t.Fatal(err)
			}
			b, err := Derive(template, "account", "b", 1002)
			if err != nil {
				t.Fatal(err)
			}
			if string(a) == string(b) {
				t.Fatal("separate bindings share credentials")
			}
			if err := ValidateIdentity(a); err != nil {
				t.Fatal(err)
			}
			if protocol == "socks" && strings.Contains(string(a), `"name"`) {
				t.Fatal("SOCKS user has an unsupported name field")
			}
		})
	}
}

func TestDeriveShadowsocksKeySizes(t *testing.T) {
	for method, size := range map[string]int{"2022-blake3-aes-128-gcm": 16, "2022-blake3-aes-256-gcm": 32, "2022-blake3-chacha20-poly1305": 32, "aes-128-gcm": 32, "aes-256-gcm": 32, "chacha20-ietf-poly1305": 32, "xchacha20-ietf-poly1305": 32} {
		t.Run(method, func(t *testing.T) {
			old := "old"
			if strings.HasPrefix(method, "2022-") {
				old = base64.StdEncoding.EncodeToString(make([]byte, size))
			}
			data, err := Derive([]byte(`{"inbounds":[{"type":"shadowsocks","method":"`+method+`","password":"`+old+`","listen_port":1000}]}`), "account", "bind", 2000)
			if err != nil {
				t.Fatal(err)
			}
			in := decode(t, data)["inbounds"].([]any)[0].(map[string]any)
			key, err := base64.StdEncoding.DecodeString(in["password"].(string))
			if err != nil || len(key) != size {
				t.Fatalf("invalid generated key for %s", method)
			}
		})
	}
}

func TestDeriveCaddyPathAndACMEGuard(t *testing.T) {
	template := []byte(`{"inbounds":[{"type":"vmess","listen":"127.0.0.1","listen_port":1000,"users":[{"uuid":"old"}],"transport":{"type":"ws","path":"/old","headers":{"host":"example.com"}}}]}`)
	if !FrontendRequired(template) {
		t.Fatal("Caddy frontend not detected")
	}
	data, err := Derive(template, "a", "b", 1001)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"path": "/sbe/b/old"`) || !strings.Contains(string(data), "example.com") {
		t.Fatalf("frontend path or host not retained: %s", data)
	}
	for _, tls := range []string{`"acme":{"domain":["example.com"]}`, `"certificate_provider":{"type":"acme","domain":["example.com"]}`} {
		_, err := Derive([]byte(`{"inbounds":[{"type":"anytls","users":[{"password":"old"}],"tls":{`+tls+`}}]}`), "a", "b", 1001)
		if err == nil || !strings.Contains(err.Error(), "ACME") {
			t.Fatal("dynamic ACME was silently copied")
		}
	}
}

func TestRejectAnonymousAndMalformedIdentities(t *testing.T) {
	for _, value := range []string{
		`{"inbounds":[{"type":"socks","users":[]}]}`,
		`{"inbounds":[{"type":"direct"}]}`,
		`{"inbounds":[{"type":"unknown","users":[{"password":"p"}]}]}`,
		`{"inbounds":[{"type":"tuic","users":[{"uuid":"id"}]}]}`,
		`{"inbounds":[{"type":"shadowsocks","method":"2022-blake3-aes-128-gcm","password":"bad"}]}`,
	} {
		if err := ValidateIdentity([]byte(value)); err == nil {
			t.Fatalf("accepted unsafe identity: %s", value)
		}
	}
	_, err := Derive([]byte(`{"inbounds":[{"type":"trojan","users":[{"password":"old"}]}]}`), "../account", "binding", 1000)
	if err == nil {
		t.Fatal("accepted path injection in account ID")
	}
}

func TestRenderPreservesBaseAndPrependsExactWarpRule(t *testing.T) {
	base := []byte(`{"log":{"output":"/keep.log"},"dns":{"servers":[{"tag":"existing","address":"1.1.1.1"}]},"inbounds":[{"type":"direct","tag":"discard"}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct","rules":[{"domain":["old.example"],"outbound":"direct"}]},"experimental":{"cache_file":{"enabled":true}}}`)
	node := []byte(`{"inbounds":[{"type":"hysteria2","tag":"sbe-hy","listen_port":1001,"users":[{"password":"secret"}]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"direct","tag":"public_key_keep"}]}`)
	data, err := Render(base, [][]byte{node}, 10080, []string{"Example.com", "example.com"}, 40000)
	if err != nil {
		t.Fatal(err)
	}
	v := decode(t, data)
	if len(v["inbounds"].([]any)) != 1 || strings.Contains(string(data), "discard") || !strings.Contains(string(data), "public_key_keep") || !strings.Contains(string(data), "/keep.log") {
		t.Fatalf("base/binding merge incorrect: %s", data)
	}
	rules := v["route"].(map[string]any)["rules"].([]any)
	first := rules[0].(map[string]any)
	if first["action"] != "sniff" || len(first["inbound"].([]any)) != 1 || first["inbound"].([]any)[0] != "sbe-hy" {
		t.Fatal("WARP sniff rule does not cover only the managed inbounds")
	}
	warp := rules[1].(map[string]any)
	if warp["outbound"] != "sbe-warp" || len(warp["domain"].([]any)) != 1 || len(rules) != 3 {
		t.Fatal("exact WARP rule did not follow sniff and precede existing route")
	}
	if rules[2].(map[string]any)["outbound"] != "direct" {
		t.Fatal("existing route order changed")
	}
	api := v["experimental"].(map[string]any)["v2ray_api"].(map[string]any)
	if api["listen"] != "127.0.0.1:10080" || !api["stats"].(map[string]any)["enabled"].(bool) {
		t.Fatal("statistics listener is not private/enabled")
	}
	if v["dns"].(map[string]any)["servers"] == nil {
		t.Fatal("existing DNS was discarded")
	}
	if server := v["dns"].(map[string]any)["servers"].([]any)[0].(map[string]any); server["address"] != "1.1.1.1" || server["tag"] != "existing" {
		t.Fatal("WARP routing rewrote the existing DNS server")
	}
}

func TestRenderRejectsConflictsAndExternalAPIs(t *testing.T) {
	node := []byte(`{"inbounds":[{"type":"trojan","tag":"b","users":[{"password":"p"}]}]}`)
	for _, base := range []string{
		`{"experimental":{"clash_api":{"external_controller":"0.0.0.0:9090"}}}`,
		`{"experimental":{"v2ray_api":{"listen":"0.0.0.0:8080"}}}`,
		`{"services":[{"type":"ssm-api"}]}`,
	} {
		if _, err := Render([]byte(base), [][]byte{node}, 8080, nil, 0); err == nil {
			t.Fatalf("accepted inherited management API: %s", base)
		}
	}
	if _, err := Render([]byte(`{}`), [][]byte{node, node}, 8080, nil, 0); err == nil {
		t.Fatal("duplicate inbound tag accepted")
	}
	conflict := []byte(`{"inbounds":[{"type":"trojan","tag":"b","users":[{"password":"p"}]}],"outbounds":[{"type":"socks","tag":"direct","server":"example.com"}]}`)
	if _, err := Render([]byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`), [][]byte{conflict}, 8080, nil, 0); err == nil {
		t.Fatal("conflicting outbound tag accepted")
	}
	for _, domain := range []string{"*.example.com", "https://example.com", "127.0.0.1", "bad domain.com", "-bad.com"} {
		if _, err := Render([]byte(`{}`), [][]byte{node}, 8080, []string{domain}, 40000); err == nil {
			t.Fatalf("accepted invalid exact domain %q", domain)
		}
	}
	if _, err := Render([]byte(`{}`), [][]byte{node}, 8080, []string{"example.com"}, 0); err == nil {
		t.Fatal("enabled WARP rules without a proxy")
	}
}

func TestNodeRootFieldsAreRejectedBeforeTheyCanBeLost(t *testing.T) {
	for _, field := range []string{"route", "dns", "experimental", "services", "log", "future_root"} {
		t.Run(field, func(t *testing.T) {
			node := []byte(`{"inbounds":[{"type":"trojan","tag":"b","listen_port":1000,"users":[{"password":"p"}]}],"` + field + `":{}}`)
			if err := ValidateIdentity(node); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatal("node add validation accepted unsupported root semantics")
			}
			if _, err := Derive(node, "a", "b", 1001); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatal("binding derivation accepted unsupported root semantics")
			}
			if _, err := Render([]byte(`{}`), [][]byte{node}, 8080, nil, 0); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatal("render silently discarded unsupported node root semantics")
			}
		})
	}
}

func TestBaseRootFieldsRemainAvailable(t *testing.T) {
	base := []byte(`{"route":{"final":"direct"},"dns":{"servers":[{"tag":"private","address":"10.0.0.53"}]},"outbounds":[{"type":"direct","tag":"direct"}],"log":{"output":"/private/log"},"future_root":{"counter":9007199254740993},"experimental":{"cache_file":{"enabled":true}}}`)
	node := []byte(`{"inbounds":[{"type":"trojan","tag":"b","listen_port":1000,"users":[{"password":"p"}]}]}`)
	data, err := Render(base, [][]byte{node}, 8080, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"10.0.0.53", "/private/log", "9007199254740993", "cache_file", `"final": "direct"`} {
		if !strings.Contains(string(data), preserved) {
			t.Fatalf("base root field was lost: %s", preserved)
		}
	}
}
