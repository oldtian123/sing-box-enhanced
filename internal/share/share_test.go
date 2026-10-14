package share

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

const testUUID = "550e8400-e29b-41d4-a716-446655440000"

func node(t *testing.T, protocol string, fields map[string]any) []byte {
	t.Helper()
	in := map[string]any{"type": protocol, "tag": "binding", "listen": "::", "listen_port": 24443, "users": []any{map[string]any{"uuid": testUUID, "password": "a:@/?#% 中文", "username": "alice"}}}
	for k, value := range fields {
		in[k] = value
	}
	data, err := json.Marshal(map[string]any{"inbounds": []any{in}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parsed(t *testing.T, value string) *url.URL {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func vmessObject(t *testing.T, value string) map[string]any {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "vmess://"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGenerateEightBasicProtocols(t *testing.T) {
	for _, protocol := range []string{"vmess", "vless", "trojan", "hysteria2", "anytls", "tuic", "shadowsocks", "socks"} {
		t.Run(protocol, func(t *testing.T) {
			fields := map[string]any{}
			if protocol == "trojan" || protocol == "hysteria2" || protocol == "anytls" || protocol == "tuic" {
				fields["tls"] = map[string]any{"enabled": true, "server_name": "tls.example"}
			}
			if protocol == "shadowsocks" {
				fields["method"] = "aes-128-gcm"
				fields["password"] = "a:@/?#% 中文"
				fields["users"] = nil
			}
			value, err := Generate(node(t, protocol, fields), "2001:db8::1", "节点 #1 中文")
			if err != nil {
				t.Fatal(err)
			}
			if protocol == "vmess" {
				v := vmessObject(t, value)
				if v["add"] != "2001:db8::1" || v["port"] != "24443" || v["ps"] != "节点 #1 中文" {
					t.Fatalf("incorrect VMess IPv6 export: %s", value)
				}
				return
			}
			u := parsed(t, value)
			if u.Hostname() != "2001:db8::1" || u.Port() != "24443" || u.Fragment != "节点 #1 中文" || !strings.Contains(value, "[2001:db8::1]") {
				t.Fatalf("incorrect IPv6 or fragment escaping: %s", value)
			}
			if u.Query().Get("insecure") != "" || u.Query().Get("allowInsecure") != "" {
				t.Fatal("export silently disabled TLS verification")
			}
			if protocol == "trojan" || protocol == "hysteria2" || protocol == "anytls" {
				if u.User.Username() != "a:@/?#% 中文" {
					t.Fatalf("password did not round trip safely: %s", value)
				}
			}
			if protocol == "tuic" {
				pass, _ := u.User.Password()
				if u.User.Username() != testUUID || pass != "a:@/?#% 中文" {
					t.Fatal("TUIC UUID and password did not round trip")
				}
			}
			if protocol == "shadowsocks" {
				data, err := base64.RawURLEncoding.DecodeString(u.User.Username())
				if err != nil || string(data) != "aes-128-gcm:a:@/?#% 中文" {
					t.Fatal("Shadowsocks credentials did not round trip")
				}
			}
			if protocol == "socks" {
				data, err := base64.StdEncoding.DecodeString(u.User.Username())
				if err != nil || string(data) != "alice:a:@/?#% 中文" {
					t.Fatal("SOCKS credentials did not round trip")
				}
			}
		})
	}
}

func TestTLSVerificationOnlyExplicitlyDisabled(t *testing.T) {
	data := node(t, "hysteria2", map[string]any{"tls": map[string]any{"enabled": true, "certificate_path": "/selfsigned.pem", "key_path": "/never-read.key"}})
	safe, err := Generate(data, "server.example", "safe")
	if err != nil {
		t.Fatal(err)
	}
	if parsed(t, safe).Query().Get("sni") != "server.example" || strings.Contains(safe, "insecure") {
		t.Fatal("certificate path changed the verification policy")
	}
	unsafe, err := GenerateWithOptions(data, "server.example", "explicit", true)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed(t, unsafe).Query()
	if q.Get("insecure") != "1" || q.Get("allowInsecure") != "1" {
		t.Fatal("explicit insecure choice was not encoded")
	}
	vmess := node(t, "vmess", map[string]any{"tls": map[string]any{"enabled": true}})
	value, err := GenerateWithOptions(vmess, "server.example", "vmess", true)
	if err != nil || vmessObject(t, value)["allowInsecure"] != true {
		t.Fatal("VMess did not encode its explicit insecure choice")
	}
}

func TestTransportVariantsAndEscaping(t *testing.T) {
	for _, kind := range []string{"ws", "http", "httpupgrade"} {
		t.Run(kind, func(t *testing.T) {
			fields := map[string]any{"transport": map[string]any{"type": kind, "path": "/sbe/bind/a?token=1&key=#空 格", "headers": map[string]any{"Host": "front.example"}}, "tls": map[string]any{"enabled": true, "server_name": "sni.example"}}
			value, err := Generate(node(t, "vless", fields), "server.example:443", "label #")
			if err != nil {
				t.Fatal(err)
			}
			q := parsed(t, value).Query()
			expectedType := kind
			if kind == "http" {
				expectedType = "h2"
			}
			if q.Get("type") != expectedType || q.Get("path") != "/sbe/bind/a?token=1&key=#空 格" || q.Get("host") != "front.example" || q.Get("sni") != "sni.example" || q.Get("security") != "tls" {
				t.Fatalf("transport was lost in export: %s", value)
			}
			value, err = Generate(node(t, "vmess", fields), "server.example", "vmess")
			if err != nil {
				t.Fatal(err)
			}
			if vmessObject(t, value)["net"] != expectedType {
				t.Fatal("VMess transport conversion was incorrect")
			}
		})
	}
	plainHTTP := node(t, "vmess", map[string]any{"transport": map[string]any{"type": "http"}})
	value, err := Generate(plainHTTP, "server.example", "plain-http")
	if err != nil {
		t.Fatal(err)
	}
	v := vmessObject(t, value)
	if v["net"] != "tcp" || v["type"] != "http" {
		t.Fatal("upstream VMess TCP HTTP format was not retained")
	}
	quic := node(t, "vmess", map[string]any{"transport": map[string]any{"type": "quic"}, "tls": map[string]any{"enabled": true}})
	value, err = Generate(quic, "server.example", "quic")
	if err != nil || vmessObject(t, value)["alpn"] != "h3" {
		t.Fatal("upstream VMess QUIC format was not retained")
	}
}

func TestCaddyRequiresKnownPublicPortAndHost(t *testing.T) {
	fields := map[string]any{"listen": "127.0.0.1", "transport": map[string]any{"type": "ws", "path": "/sbe/bind/old", "headers": map[string]any{"host": "front.example"}}}
	data := node(t, "vless", fields)
	if _, err := Generate(data, "45.8.173.97", "frontend"); err == nil || !strings.Contains(err.Error(), "public frontend port") {
		t.Fatal("Caddy backend port was advertised as a public port")
	}
	value, err := Generate(data, "45.8.173.97:8443", "frontend")
	if err != nil {
		t.Fatal(err)
	}
	u := parsed(t, value)
	if u.Hostname() != "front.example" || u.Port() != "8443" || u.Query().Get("security") != "tls" || u.Query().Get("sni") != "front.example" {
		t.Fatalf("Caddy frontend endpoint incorrectly restored: %s", value)
	}
	fields["transport"] = map[string]any{"type": "ws", "path": "/sbe/bind/old"}
	if _, err := Generate(node(t, "vless", fields), "45.8.173.97:8443", "frontend"); err == nil || !strings.Contains(err.Error(), "public Host") {
		t.Fatal("Caddy host was guessed without frontend information")
	}
}

func TestRealityUsesPublicMetadataAndNeverPrivateKey(t *testing.T) {
	public := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	data := node(t, "vless", map[string]any{"users": []any{map[string]any{"uuid": testUUID, "flow": "xtls-rprx-vision"}}, "tls": map[string]any{"enabled": true, "server_name": "reality.example", "reality": map[string]any{"enabled": true, "private_key": "PRIVATE_MUST_NEVER_ESCAPE", "short_id": []string{"abcd"}}}})
	var root map[string]any
	json.Unmarshal(data, &root)
	root["outbounds"] = []any{map[string]any{"type": "direct", "tag": "public_key_" + public}}
	data, _ = json.Marshal(root)
	value, err := GenerateWithOptions(data, "[2001:db8::2]:443", "reality", true)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed(t, value).Query()
	if q.Get("pbk") != public || q.Get("sid") != "abcd" || q.Get("flow") != "xtls-rprx-vision" || q.Get("security") != "reality" || q.Get("sni") != "reality.example" || strings.Contains(value, "PRIVATE") || q.Get("insecure") != "" {
		t.Fatalf("REALITY export lost authentication or exposed private values: %s", value)
	}
	delete(root, "outbounds")
	data, _ = json.Marshal(root)
	if _, err := Generate(data, "server.example", "missing"); err == nil || !strings.Contains(err.Error(), "public key metadata") {
		t.Fatal("REALITY private key was substituted for missing public metadata")
	}
}

func TestShadowsocks2022ServerAndUserKeyCombination(t *testing.T) {
	for _, tc := range []struct {
		method string
		size   int
	}{{"2022-blake3-aes-128-gcm", 16}, {"2022-blake3-aes-256-gcm", 32}} {
		t.Run(tc.method, func(t *testing.T) {
			serverKey := base64.StdEncoding.EncodeToString(make([]byte, tc.size))
			userBytes := make([]byte, tc.size)
			userBytes[0] = 1
			userKey := base64.StdEncoding.EncodeToString(userBytes)
			data := node(t, "shadowsocks", map[string]any{"method": tc.method, "password": serverKey, "users": []any{map[string]any{"password": userKey}}})
			value, err := Generate(data, "[2001:db8::1]:443", "ss2022")
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.RawURLEncoding.DecodeString(parsed(t, value).User.Username())
			if err != nil || string(decoded) != tc.method+":"+serverKey+":"+userKey {
				t.Fatal("SS2022 sharing lost the required server and user key combination")
			}
		})
	}
}

func TestHysteriaObfsAndTUICOptions(t *testing.T) {
	hy := node(t, "hysteria2", map[string]any{"tls": map[string]any{"enabled": true}, "obfs": map[string]any{"type": "salamander", "password": "obfs:@#"}})
	value, err := Generate(hy, "server.example", "hy")
	if err != nil {
		t.Fatal(err)
	}
	q := parsed(t, value).Query()
	if q.Get("obfs") != "salamander" || q.Get("obfs-password") != "obfs:@#" || q.Get("alpn") != "h3" {
		t.Fatal("Hysteria2 connection parameters were lost")
	}
	tuic := node(t, "tuic", map[string]any{"tls": map[string]any{"enabled": true}, "congestion_control": "bbr"})
	value, err = Generate(tuic, "server.example", "tuic")
	if err != nil || parsed(t, value).Query().Get("congestion_control") != "bbr" {
		t.Fatal("TUIC congestion option was lost")
	}
}

func TestUnsupportedAndAmbiguousInputsFail(t *testing.T) {
	for _, data := range [][]byte{
		node(t, "unknown", nil),
		node(t, "direct", nil),
		node(t, "vless", map[string]any{"users": []any{map[string]any{"uuid": testUUID}, map[string]any{"uuid": testUUID}}}),
		node(t, "vmess", map[string]any{"transport": map[string]any{"type": "grpc"}}),
		node(t, "vless", map[string]any{"transport": map[string]any{"type": "ws", "headers": map[string]any{"X-Token": "required"}}}),
		node(t, "vless", map[string]any{"transport": map[string]any{"type": "ws", "max_early_data": 2048}}),
		node(t, "vless", map[string]any{"transport": map[string]any{"type": "ws", "future_setting": true}}),
		node(t, "trojan", nil),
		node(t, "anytls", map[string]any{"tls": "invalid"}),
	} {
		if _, err := Generate(data, "server.example", "invalid"); err == nil {
			t.Fatalf("unsupported connection semantics were silently dropped: %s", data)
		}
	}
	for _, server := range []string{"", "https://server.example", "server.example:0", "server.example:65536", "server.example:bad", "server.example\nother"} {
		if _, err := Generate(node(t, "vless", nil), server, "invalid"); err == nil {
			t.Fatalf("unsafe or invalid server accepted %q", server)
		}
	}
}
