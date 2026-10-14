package caddy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func template(transport, tls, path string) []byte {
	return []byte(`{"inbounds":[{"type":"vmess","listen":"127.0.0.1","listen_port":1000,"tag":"sbe-bind","transport":{"type":"` + transport + `","path":"` + path + `","headers":{"host":"example.com"}},"tls":{"enabled":` + tls + `}}]}`)
}

func TestPlanSelectsActualBackendProtocol(t *testing.T) {
	for _, tc := range []struct{ transport, tls, backend string }{
		{"ws", "false", "127.0.0.1:1000"},
		{"httpupgrade", "false", "127.0.0.1:1000"},
		{"http", "false", "h2c://127.0.0.1:1000"},
		{"http", "true", "https://127.0.0.1:1000"},
	} {
		t.Run(tc.transport+tc.tls, func(t *testing.T) {
			mapping, err := Plan(template(tc.transport, tc.tls, "/old"), template(tc.transport, tc.tls, "/sbe/bind/old"))
			if err != nil {
				t.Fatal(err)
			}
			if mapping.Backend != tc.backend || mapping.Host != "example.com" || mapping.Path != "/sbe/bind/old" || !strings.Contains(mapping.Snippet, "BEGIN sing-box-enhanced sbe-bind") {
				t.Fatalf("incorrect frontend plan: %+v", mapping)
			}
			if tc.transport == "http" && tc.tls == "true" && !strings.Contains(mapping.Snippet, "versions 2") {
				t.Fatal("TLS HTTP backend lost HTTP/2 transport")
			}
		})
	}
}

func TestPlanRejectsUnsafeOrPublicBackend(t *testing.T) {
	for _, binding := range [][]byte{
		template("ws", "false", "/bad*"),
		[]byte(strings.ReplaceAll(string(template("ws", "false", "/safe")), "127.0.0.1", "0.0.0.0")),
		template("grpc", "false", "/safe"),
	} {
		if _, err := Plan(template("ws", "false", "/old"), binding); err == nil {
			t.Fatal("unsafe frontend plan accepted")
		}
	}
}

func fakeManager(t *testing.T) (Manager, string) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "caddy")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SBE_CADDY_LOG\"\nif [ \"$1\" = validate ] && [ -f \"$SBE_CADDY_FAIL\" ]; then exit 1; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(main, []byte("{\n admin off\n}\nimport "+filepath.Join(dir, "*.conf")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "example.com.conf")
	if err := os.WriteFile(site, []byte("# keep original\nexample.com:443 {\n    reverse_proxy /old 127.0.0.1:9999\n    header X-Keep \"value with { braces }\"\n    reverse_proxy https://example.net {\n        header_up Host {upstream_hostport}\n    }\n    import /some/original.conf.add\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SBE_CADDY_LOG", filepath.Join(dir, "calls.log"))
	t.Setenv("SBE_CADDY_FAIL", filepath.Join(dir, "fail"))
	return Manager{Binary: binary, MainFile: main, BackupDir: filepath.Join(dir, "backups")}, site
}

func TestApplyAndRemovePreserveOriginalSite(t *testing.T) {
	m, site := fakeManager(t)
	original, _ := os.ReadFile(site)
	mapping, err := Plan(template("ws", "false", "/old"), template("ws", "false", "/sbe/bind/old"))
	if err != nil {
		t.Fatal(err)
	}
	firstHash, err := m.Apply(site, mapping)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(site)
	if !strings.Contains(string(data), mapping.Snippet) || !strings.Contains(string(data), "reverse_proxy /old 127.0.0.1:9999") || !strings.Contains(string(data), "{upstream_hostport}") || !strings.Contains(string(data), "import /some/original.conf.add") {
		t.Fatal("owned insertion lost original site content")
	}
	secondHash, err := m.Apply(site, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatal("reapplying the mapping changed its content hash")
	}
	data, _ = os.ReadFile(site)
	if strings.Count(string(data), "BEGIN sing-box-enhanced sbe-bind") != 1 {
		t.Fatal("reapply duplicated the owned fragment")
	}
	if _, err := m.Remove(site, mapping.Tag); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(site)
	if strings.Contains(string(data), "sing-box-enhanced") || !strings.Contains(string(data), "reverse_proxy /old 127.0.0.1:9999") {
		t.Fatal("removal changed original routes")
	}
	if string(data) != string(original) {
		t.Fatal("removal did not preserve original site bytes")
	}
	files, err := os.ReadDir(m.BackupDir)
	if err != nil || len(files) == 0 {
		t.Fatal("missing private backup")
	}
	first, _ := os.ReadFile(filepath.Join(m.BackupDir, files[0].Name()))
	if len(first) == 0 || len(original) == 0 {
		t.Fatal("empty backup")
	}
	calls, _ := os.ReadFile(os.Getenv("SBE_CADDY_LOG"))
	if strings.Contains(string(calls), "reload") || strings.Contains(string(calls), "restart") {
		t.Fatal("publication activated Caddy unexpectedly")
	}
}

func TestFullValidationFailureRestoresFile(t *testing.T) {
	m, site := fakeManager(t)
	original, _ := os.ReadFile(site)
	mapping, _ := Plan(template("ws", "false", "/old"), template("ws", "false", "/sbe/bind/old"))
	if err := os.WriteFile(os.Getenv("SBE_CADDY_FAIL"), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(site, mapping); err == nil {
		t.Fatal("full validation failure was not reported")
	}
	data, _ := os.ReadFile(site)
	if string(data) != string(original) {
		t.Fatal("full validation failure did not restore original site")
	}
}

func TestPublicationRejectsTamperedFragmentsAndUnimportedSite(t *testing.T) {
	m, site := fakeManager(t)
	mapping, _ := Plan(template("ws", "false", "/old"), template("ws", "false", "/sbe/bind/old"))
	mapping.Snippet = strings.Replace(mapping.Snippet, "reverse_proxy", "respond", 1)
	if _, err := m.Apply(site, mapping); err == nil {
		t.Fatal("tampered generated snippet accepted")
	}
	mapping, _ = Plan(template("ws", "false", "/old"), template("ws", "false", "/sbe/bind/old"))
	if err := os.WriteFile(m.MainFile, []byte("# import "+site+"\nother.example {\n respond ok\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(site, mapping); err == nil {
		t.Fatal("unimported site was reported published")
	}
}

func TestSiteSelectionAndOwnedMarkerValidation(t *testing.T) {
	if _, err := siteOpening([]byte("other.example:443 {\n}\n"), "example.com"); err == nil {
		t.Fatal("wrong hostname site selected")
	}
	if _, err := siteOpening([]byte("example.com:443 {\n}\nexample.com:8443 {\n}\n"), "example.com"); err == nil {
		t.Fatal("ambiguous site selected")
	}
	if _, _, err := remove([]byte("    # BEGIN sing-box-enhanced sbe-bind\nmissing end\n"), "sbe-bind"); err == nil {
		t.Fatal("incomplete owned block was removed")
	}
}
