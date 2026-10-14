package warp

import (
	"strings"
	"testing"
)

func TestValidateDomain(t *testing.T) {
	for input, want := range map[string]string{
		"Example.COM":      "example.com",
		"www.example.com.": "www.example.com",
		"xn--fiqs8s.cn":    "xn--fiqs8s.cn",
		"a-b.example":      "a-b.example",
	} {
		got, err := ValidateDomain(input)
		if err != nil || got != want {
			t.Errorf("ValidateDomain(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "localhost", "https://example.com", "example.com/path", "example.com:443",
		"*.example.com", "127.0.0.1", "001.002.003.004", "[::1]", "::1",
		"example.com\n", " example.com", "example.com;id", "$(id).example.com",
		"example..com", "example.com..", "-example.com", "example-.com",
		"under_score.com", "example.com?x=1", "example.com#x", "example.123",
		"中文.cn", strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "com",
	} {
		if _, err := ValidateDomain(input); err == nil {
			t.Errorf("accepted invalid exact domain %q", input)
		}
	}
}
