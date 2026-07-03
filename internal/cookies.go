package internal

import (
	"fmt"
	"strings"
	"time"
)

// CSRFFromCookies returns the csrftoken cookie value, if present.
func CSRFFromCookies(cookieStr string) string {
	for name, value := range ParseCookies(cookieStr) {
		if strings.EqualFold(name, "csrftoken") {
			return value
		}
	}
	return ""
}

// CSRFTokenForRequest returns a csrftoken from existing cookies or generates one.
func CSRFTokenForRequest(cookieStr string) string {
	if token := CSRFFromCookies(cookieStr); token != "" {
		return token
	}
	return fmt.Sprintf("%016x%016x", time.Now().UnixNano(), time.Now().UnixNano()^0xDEADBEEF)
}

// FormatCookieHeader builds a single Cookie header with exactly one csrftoken entry.
func FormatCookieHeader(existingCookies, csrfToken string) string {
	m := ParseCookies(existingCookies)
	if csrfToken == "" {
		csrfToken = CSRFTokenForRequest("")
	}
	m["csrftoken"] = csrfToken

	parts := make([]string, 0, len(m))
	for name, value := range m {
		if value == "" {
			continue
		}
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// JoinCookiePairs merges name=value pairs into a cookie header string.
func JoinCookiePairs(pairs ...string) string {
	var parts []string
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair != "" {
			parts = append(parts, pair)
		}
	}
	return strings.Join(parts, "; ")
}
