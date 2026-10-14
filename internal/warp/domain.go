package warp

import (
	"fmt"
	"net"
	"strings"
)

// ValidateDomain accepts a single exact DNS hostname, including ASCII punycode.
func ValidateDomain(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSuffix(value, "."))
	if len(domain) == 0 || len(domain) > 253 || net.ParseIP(domain) != nil {
		return "", fmt.Errorf("an exact DNS domain is required")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("domain must contain at least two labels")
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid DNS label")
		}
		for _, character := range label {
			if !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-') {
				return "", fmt.Errorf("domain contains an invalid character")
			}
		}
	}
	allDigits := true
	for _, character := range labels[len(labels)-1] {
		allDigits = allDigits && character >= '0' && character <= '9'
	}
	if allDigits {
		return "", fmt.Errorf("domain must not have an entirely numeric final label")
	}
	return domain, nil
}
