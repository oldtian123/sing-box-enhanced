package warp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestClientCommandsDoNotChangeModeOrExposeOutput(t *testing.T) {
	directory := t.TempDir()
	log := filepath.Join(directory, "commands")
	t.Setenv("WARP_TEST_LOG", log)
	binary := filepath.Join(directory, "warp-cli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$WARP_TEST_LOG\"\ncase \"$1\" in settings) printf '(User set) Mode: WarpProxy on port 40000\\n';; *) printf 'registration-secret\\n';; esac\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Binary: binary}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(log)
	if err != nil || string(commands) != "settings\nconnect\ndisconnect\n" {
		t.Fatalf("unexpected CLI operations %q, %v", commands, err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'secret' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(context.Background()); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("CLI failure leaked output or succeeded: %v", err)
	}
}

func termsCLI(t *testing.T, settings string) (Client, string) {
	t.Helper()
	directory := t.TempDir()
	log := filepath.Join(directory, "commands")
	t.Setenv("WARP_TEST_LOG", log)
	t.Setenv("WARP_TEST_SETTINGS", settings)
	t.Setenv("WARP_TEST_FAILURE", "")
	binary := filepath.Join(directory, "warp-cli")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$WARP_TEST_LOG"
if [ "$1" != --accept-tos ]; then
    printf 'registration-secret\n' >&2
    exit 1
fi
shift
if [ "$*" = "$WARP_TEST_FAILURE" ]; then
    printf 'registration-secret\n'
    printf 'registration-secret\n' >&2
    exit 1
fi
case "$1" in
    settings) printf '%s\n' "$WARP_TEST_SETTINGS";;
    status) printf 'Status update: Connected\n';;
    *) printf 'registration-secret\n';;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return Client{Binary: binary}, log
}

func TestClientTermsConsentControlsEveryCommand(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(strconv.FormatBool(accept), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			client, log := termsCLI(t, "Mode: WarpProxy on port "+strconv.Itoa(port))
			client.Port, client.AcceptTerms = port, accept
			for _, action := range []func(context.Context) error{client.Connect, client.Disconnect} {
				err := action(context.Background())
				if (err == nil) != accept || (err != nil && strings.Contains(err.Error(), "registration-secret")) {
					t.Fatalf("unexpected command result or leaked output: %v", err)
				}
			}
			status, err := client.Status(context.Background())
			if err == nil || strings.Contains(err.Error(), "registration-secret") || !status.Installed || status.DaemonConnected != accept || status.ProxyReachable {
				t.Fatalf("unexpected status result or leaked output: %+v, %v", status, err)
			}
			want := "settings\ndisconnect\nsettings\n"
			if accept {
				want = "--accept-tos settings\n--accept-tos connect\n--accept-tos disconnect\n--accept-tos settings\n--accept-tos status\n"
			}
			commands, err := os.ReadFile(log)
			if err != nil || string(commands) != want {
				t.Fatalf("unexpected terms flags: %q, %v", commands, err)
			}
		})
	}
}

func TestAcceptedTermsStillRejectUnsafeProxySettings(t *testing.T) {
	for _, settings := range []string{"Mode: WarpWithDnsOverHttps", "Mode: WarpProxy on port 40001"} {
		t.Run(settings, func(t *testing.T) {
			client, log := termsCLI(t, settings)
			client.AcceptTerms = true
			if err := client.Connect(context.Background()); err == nil {
				t.Fatal("accepted terms bypassed the proxy mode guard")
			}
			if _, err := client.Status(context.Background()); err == nil {
				t.Fatal("accepted terms allowed an unsafe proxy probe")
			}
			commands, err := os.ReadFile(log)
			if err != nil || string(commands) != "--accept-tos settings\n--accept-tos settings\n" {
				t.Fatalf("unsafe mode triggered another operation: %q, %v", commands, err)
			}
		})
	}
}

func TestRegisterWithoutConsentDoesNotRunCLI(t *testing.T) {
	client, log := termsCLI(t, "Mode: WarpProxy on port 40000")
	if err := client.Register(context.Background()); err == nil {
		t.Fatal("registration accepted without explicit consent")
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration without consent ran CLI: %v", err)
	}
}

func TestRegisterCommandsAndFailureBoundaries(t *testing.T) {
	steps := []string{"registration new", "mode proxy", "proxy port 44000", "tunnel protocol set MASQUE", "settings"}
	for _, failAt := range append([]string{"", "unsafe final mode", "invalid port"}, steps...) {
		t.Run(failAt, func(t *testing.T) {
			settings := "Mode: WarpProxy on port 44000"
			if failAt == "unsafe final mode" {
				settings = "Mode: WarpWithDnsOverHttps"
			}
			client, log := termsCLI(t, settings)
			client.AcceptTerms, client.Port = true, 44000
			if failAt == "invalid port" {
				client.Port = -1
			}
			t.Setenv("WARP_TEST_FAILURE", failAt)
			err := client.Register(context.Background())
			if (err == nil) != (failAt == "") || (err != nil && strings.Contains(err.Error(), "registration-secret")) {
				t.Fatalf("unexpected registration result or leaked output: %v", err)
			}
			commands, readErr := os.ReadFile(log)
			if failAt == "invalid port" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("invalid registration port ran CLI: %q, %v", commands, readErr)
				}
				return
			}
			var expected []string
			for _, step := range steps {
				expected = append(expected, "--accept-tos "+step)
				if step == failAt {
					break
				}
			}
			if readErr != nil || string(commands) != strings.Join(expected, "\n")+"\n" {
				t.Fatalf("registration continued after failure or connected: %q, %v", commands, readErr)
			}
		})
	}
}

func TestStatusDoesNotTreatDirectConnectivityAsWARP(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "warp-cli")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	script := "#!/bin/sh\ncase \"$1\" in settings) printf 'Mode: WarpProxy on port " + strconv.Itoa(port) + "\\n';; status) printf 'Status update: Connected\\n';; *) exit 1;; esac\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	status, err := (Client{Binary: binary, Port: port}).Status(context.Background())
	if err == nil || !status.Installed || !status.DaemonConnected || status.ProxyReachable || status.ExitIP != "" || status.WARP != "" {
		t.Fatalf("unreachable proxy returned %+v, %v", status, err)
	}
}

func TestConnectAndStatusRejectUnsafeProxySettings(t *testing.T) {
	for name, settings := range map[string]string{
		"global tunnel":    "(Default) Mode: WarpWithDnsOverHttps",
		"other port":       "(User set) Mode: WarpProxy on port 40001",
		"not a mode field": "Notes: WarpProxy on port 40000",
		"conflicting mode": "Mode: WarpProxy on port 40000\nMode: WarpWithDnsOverHttps",
		"missing mode":     "registration-secret",
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			log := filepath.Join(directory, "commands")
			t.Setenv("WARP_TEST_LOG", log)
			t.Setenv("WARP_TEST_SETTINGS", settings)
			binary := filepath.Join(directory, "warp-cli")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$WARP_TEST_LOG\"\ncase \"$1\" in settings) printf '%s\\n' \"$WARP_TEST_SETTINGS\";; *) exit 0;; esac\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			client := Client{Binary: binary}
			if err := client.Connect(context.Background()); err == nil || strings.Contains(err.Error(), "registration-secret") {
				t.Fatalf("unsafe mode connect accepted or leaked settings: %v", err)
			}
			status, err := client.Status(context.Background())
			if err == nil || !status.Installed || status.ProxyReachable || status.ExitIP != "" {
				t.Fatalf("unsafe mode status returned %+v, %v", status, err)
			}
			commands, err := os.ReadFile(log)
			if err != nil || string(commands) != "settings\nsettings\n" {
				t.Fatalf("unsafe settings triggered other CLI operation: %q, %v", commands, err)
			}
		})
	}
}

func TestStatusMissingCLI(t *testing.T) {
	status, err := (Client{Binary: filepath.Join(t.TempDir(), "missing")}).Status(context.Background())
	if err != nil || status.Installed || status.ProxyReachable || status.ExitIP != "" {
		t.Fatalf("missing CLI returned %+v, %v", status, err)
	}
}

func TestProxyTraceUsesExplicitSOCKS5AndTLS(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "ip=203.0.113.10\nloc=JP\nwarp=on\nregistration=never-return-this\n")
	}))
	defer server.Close()
	target := strings.TrimPrefix(server.URL, "https://")
	port, requested := socksServer(t, target)
	client, reachable, err := proxyHTTPClient(port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	status, err := fetchTrace(context.Background(), client, server.URL)
	if err != nil || !reachable.Load() || status.ExitIP != "203.0.113.10" || status.Location != "JP" || status.WARP != "on" {
		t.Fatalf("SOCKS trace returned %+v, %v", status, err)
	}
	if got := <-requested; got != target {
		t.Fatalf("SOCKS connected to %q, want %q", got, target)
	}
}

// A real SOCKS5 TCP CONNECT handshake verifies that HTTP cannot bypass the proxy.
func socksServer(t *testing.T, target string) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requested := make(chan string, 1)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		front, err := listener.Accept()
		if err != nil {
			return
		}
		defer front.Close()
		var greeting [2]byte
		if _, err = io.ReadFull(front, greeting[:]); err != nil || greeting[0] != 5 {
			return
		}
		if _, err = io.CopyN(io.Discard, front, int64(greeting[1])); err != nil {
			return
		}
		if _, err = front.Write([]byte{5, 0}); err != nil {
			return
		}
		var header [4]byte
		if _, err = io.ReadFull(front, header[:]); err != nil || header[0] != 5 || header[1] != 1 || header[3] != 1 {
			return
		}
		var destination [6]byte
		if _, err = io.ReadFull(front, destination[:]); err != nil {
			return
		}
		address := net.JoinHostPort(net.IP(destination[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(destination[4:]))))
		requested <- address
		if address != target {
			return
		}
		back, err := net.Dial("tcp", target)
		if err != nil {
			return
		}
		defer back.Close()
		if _, err = front.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
			return
		}
		go func() { _, _ = io.Copy(back, front) }()
		_, _ = io.Copy(front, back)
	}()
	return listener.Addr().(*net.TCPAddr).Port, requested
}

func TestTraceValidation(t *testing.T) {
	for name, body := range map[string]string{
		"invalid IP":       "ip=direct\nloc=JP\nwarp=on\n",
		"duplicate IP":     "ip=203.0.113.1\nip=203.0.113.2\nloc=JP\nwarp=on\n",
		"missing WARP":     "ip=203.0.113.1\nloc=JP\n",
		"unknown WARP":     "ip=203.0.113.1\nloc=JP\nwarp=maybe\n",
		"invalid location": "ip=203.0.113.1\nloc=Japan\nwarp=on\n",
		"oversized":        strings.Repeat("x", 16*1024+1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, body)
			}))
			defer server.Close()
			if _, err := fetchTrace(context.Background(), server.Client(), server.URL); err == nil {
				t.Fatal("accepted invalid trace")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "ip=203.0.113.1\nloc=JP\nwarp=off\n")
	}))
	defer server.Close()
	status, err := fetchTrace(context.Background(), server.Client(), server.URL)
	if err != nil || status.WARP != "off" {
		t.Fatalf("warp=off must remain visible, got %+v, %v", status, err)
	}
}

func TestSOCKSFailureHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, _, err := proxyHTTPClient(40000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetchTrace(ctx, client, traceURL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
}
