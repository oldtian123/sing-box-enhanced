package warp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

const traceURL = "https://www.cloudflare.com/cdn-cgi/trace"

type Client struct {
	Binary      string
	Port        int
	AcceptTerms bool
}

type Status struct {
	Installed       bool   `json:"installed"`
	DaemonConnected bool   `json:"daemon_connected"`
	ProxyReachable  bool   `json:"proxy_reachable"`
	ExitIP          string `json:"exit_ip"`
	Location        string `json:"location"`
	WARP            string `json:"warp"`
}

// Status probes only through the explicit local SOCKS5 endpoint; it never falls
// back to the machine's direct connection or environment proxy settings.
func (client Client) Status(ctx context.Context) (Status, error) {
	var result Status
	_, err := exec.LookPath(client.binary())
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, nil
	}
	result.Installed = true
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.requireProxyMode(ctx); err != nil {
		return result, err
	}
	command := client.cliCommand(ctx, "status")
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return result, fmt.Errorf("warp-cli status failed: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "Status update: Connected" {
			result.DaemonConnected = true
		}
	}
	httpClient, reachable, err := proxyHTTPClient(client.port())
	if err != nil {
		return result, err
	}
	defer httpClient.CloseIdleConnections()
	trace, err := fetchTrace(ctx, httpClient, traceURL)
	result.ProxyReachable = reachable.Load()
	if err != nil {
		return result, err
	}
	result.ExitIP, result.Location, result.WARP = trace.ExitIP, trace.Location, trace.WARP
	return result, nil
}

func (client Client) Connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.requireProxyMode(ctx); err != nil {
		return err
	}
	return client.command(ctx, "connect")
}

func (client Client) Disconnect(ctx context.Context) error {
	return client.command(ctx, "disconnect")
}

// Register requires explicit consent and configures only the local proxy.
// It does not connect the tunnel or return registration output.
func (client Client) Register(ctx context.Context) error {
	if !client.AcceptTerms {
		return fmt.Errorf("WARP terms must be explicitly accepted before registration")
	}
	port := client.port()
	if port < 1 || port > 65535 {
		return fmt.Errorf("WARP proxy port must be between 1 and 65535")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"registration", "new"},
		{"mode", "proxy"},
		{"proxy", "port", strconv.Itoa(port)},
		{"tunnel", "protocol", "set", "MASQUE"},
	} {
		if err := client.command(ctx, args...); err != nil {
			return err
		}
	}
	return client.requireProxyMode(ctx)
}

func (client Client) binary() string {
	if client.Binary == "" {
		return "warp-cli"
	}
	return client.Binary
}

func (client Client) port() int {
	if client.Port == 0 {
		return 40000
	}
	return client.Port
}

// A connect in a global tunnel mode could change host routing. Require the
// configured local proxy endpoint before either connecting or probing it.
func (client Client) requireProxyMode(ctx context.Context) error {
	port := client.port()
	if port < 1 || port > 65535 {
		return fmt.Errorf("WARP proxy port must be between 1 and 65535")
	}
	command := client.cliCommand(ctx, "settings")
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("warp-cli settings failed: %w", err)
	}
	if !proxyModeMatches(string(output), port) {
		return fmt.Errorf("WARP must use local proxy mode on port %d; review warp-cli settings", port)
	}
	return nil
}

func proxyModeMatches(settings string, port int) bool {
	wanted := "WarpProxy on port " + strconv.Itoa(port)
	found := false
	for _, line := range strings.Split(settings, "\n") {
		line = strings.TrimSpace(line)
		// warp-cli prefixes settings with their configuration source.
		if strings.HasPrefix(line, "(") {
			_, remainder, ok := strings.Cut(line, ")")
			if !ok {
				continue
			}
			line = strings.TrimSpace(remainder)
		}
		label, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(label) != "Mode" {
			continue
		}
		if found || strings.TrimSpace(value) != wanted {
			return false
		}
		found = true
	}
	return found
}

func (client Client) cliCommand(ctx context.Context, args ...string) *exec.Cmd {
	if client.AcceptTerms {
		args = append([]string{"--accept-tos"}, args...)
	}
	command := exec.CommandContext(ctx, client.binary(), args...)
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	command.Stderr = io.Discard
	return command
}

func (client Client) command(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := client.cliCommand(ctx, args...)
	// Command output, including registration information, remains private.
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("warp-cli %s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

func proxyHTTPClient(port int) (*http.Client, *atomic.Bool, error) {
	if port < 1 || port > 65535 {
		return nil, nil, fmt.Errorf("WARP proxy port must be between 1 and 65535")
	}
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil,
		&net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return nil, nil, fmt.Errorf("create WARP SOCKS5 dialer: %w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, nil, fmt.Errorf("SOCKS5 dialer does not support cancellation")
	}
	reachable := new(atomic.Bool)
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := contextDialer.DialContext(ctx, network, address)
			if err == nil {
				reachable.Store(true)
			}
			return connection, err
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("WARP trace must not redirect")
		},
	}, reachable, nil
}

func fetchTrace(ctx context.Context, client *http.Client, url string) (Status, error) {
	var result Status
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return result, err
	}
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("WARP proxy trace failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("WARP trace returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	if err != nil {
		return result, fmt.Errorf("read WARP trace: %w", err)
	}
	if len(body) > 16*1024 {
		return result, fmt.Errorf("WARP trace is too large")
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if !found || (key != "ip" && key != "loc" && key != "warp") {
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return result, fmt.Errorf("WARP trace has duplicate %s", key)
		}
		fields[key] = value
	}
	if net.ParseIP(fields["ip"]) == nil || len(fields["loc"]) != 2 ||
		fields["loc"][0] < 'A' || fields["loc"][0] > 'Z' || fields["loc"][1] < 'A' || fields["loc"][1] > 'Z' {
		return result, fmt.Errorf("WARP trace has no valid exit IP and location")
	}
	if fields["warp"] != "on" && fields["warp"] != "plus" && fields["warp"] != "off" {
		return result, fmt.Errorf("WARP trace has no valid WARP status")
	}
	result.ExitIP, result.Location, result.WARP = fields["ip"], fields["loc"], fields["warp"]
	return result, nil
}
