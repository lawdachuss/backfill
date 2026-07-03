package internal

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/server"
)

// sharedTransport is a singleton http.RoundTripper reused across all channels.
// It uses httpcloak's Chrome 146 Windows TLS/HTTP2 fingerprint to bypass
// Cloudflare WAF TCP RST that Go's default crypto/tls triggers.
func sharedTransport() http.RoundTripper {
	return getSharedTransport()
}

// WaitForChaturbateRateLimit blocks until a rate-limit slot is available.
// Call this before every Chaturbate API request to avoid triggering
// Cloudflare's DDoS protection when many channels reconnect simultaneously.
// Uses the adaptive rate limiter that adjusts based on error feedback.
func WaitForChaturbateRateLimit(ctx context.Context) error {
	if chaturbateRateLimiter().Acquire(ctx.Done()) {
		return nil
	}
	return ctx.Err()
}

// ReportChaturbateSuccess notifies the rate limiter and circuit breaker
// of a successful API call, allowing rate to gradually increase.
func ReportChaturbateSuccess() {
	chaturbateRateLimiter().Success()
	chaturbateBreaker.Success()
}

// ReportChaturbateFailure notifies the rate limiter and circuit breaker
// of a failed API call, triggering backoff.
func ReportChaturbateFailure() {
	chaturbateRateLimiter().Failure()
	chaturbateBreaker.Failure()
}

// AllowChaturbateRequest checks the circuit breaker.
// Returns false if the circuit is open and requests should not proceed.
func AllowChaturbateRequest() bool {
	return chaturbateBreaker.Allow()
}

// ChaturbateRate returns the current adaptive rate limit in req/s.
func ChaturbateRate() int {
	return chaturbateRateLimiter().CurrentRate()
}

// ChaturbatePeakRate returns the highest rate reached this session.
func ChaturbatePeakRate() int {
	return chaturbateRateLimiter().PeakRate()
}

// Req represents an HTTP client with customized settings.
type Req struct {
	client *http.Client
}

// NewReq creates a new HTTP client reusing the shared transport.
func NewReq() *Req {
	return &Req{
		client: &http.Client{
			Transport: sharedTransport(),
		},
	}
}

// NewNoProxyReq creates an HTTP client that bypasses any environment-configured
// proxy (ALL_PROXY / HTTP_PROXY / HTTPS_PROXY). This is used for liveness checks
// and other non-streaming requests where the SOCKS5 proxy is not needed.
// Uses standard Go TLS (no httpcloak fingerprint) since the chatvideocontext
// GET endpoint does not trigger Cloudflare WAF TCP RST.
func NewNoProxyReq() *Req {
	return &Req{
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// CreateTransport returns the shared httpcloak transport (kept for backward compatibility).
func CreateTransport() http.RoundTripper {
	return sharedTransport()
}

// Get sends an HTTP GET request and returns the response as a string.
func (h *Req) Get(ctx context.Context, url string) (string, error) {
	resp, err := h.GetBytes(ctx, url)
	if err != nil {
		return "", fmt.Errorf("get bytes: %w", err)
	}
	return string(resp), nil
}

// GetBytes sends an HTTP GET request and returns the response as a byte slice.
func (h *Req) GetBytes(ctx context.Context, url string) ([]byte, error) {
	req, cancel, err := CreateRequest(ctx, url)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("new request: %w", err)
	}
	defer cancel()

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client do: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Check for Age Verification
	if strings.Contains(string(b), "Verify your age") {
		return nil, ErrAgeVerification
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if strings.Contains(string(b), "This room requires a password") {
			return nil, ErrPasswordRequired
		}
		snippet := string(b)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, snippet)
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("forbidden: %w", ErrPrivateStream)
	}

	if resp.StatusCode != http.StatusOK {
		snippet := string(b)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, snippet)
	}

	return b, nil
}

// Head sends an HTTP HEAD request and returns the status code.
func (h *Req) Head(ctx context.Context, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	SetRequestHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// GetBytesWithTimeout is like GetBytes but with a caller-specified timeout.
// This is needed for proxied CDN segment downloads where the SOCKS5 proxy
// adds significant latency — the default 30s may not be enough to read
// multi-megabyte video segments end-to-end.
func (h *Req) GetBytesWithTimeout(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	req, cancel, err := CreateRequestWithTimeout(ctx, url, timeout)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("new request: %w", err)
	}
	defer cancel()

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client do: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if strings.Contains(string(b), "Verify your age") {
		return nil, ErrAgeVerification
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if strings.Contains(string(b), "This room requires a password") {
			return nil, ErrPasswordRequired
		}
		snippet := string(b)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, snippet)
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("forbidden: %w", ErrPrivateStream)
	}

	if resp.StatusCode != http.StatusOK {
		snippet := string(b)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, snippet)
	}

	return b, nil
}

// CreateRequest constructs an HTTP GET request with necessary headers (30s timeout).
func CreateRequest(ctx context.Context, url string) (*http.Request, context.CancelFunc, error) {
	return CreateRequestWithTimeout(ctx, url, 30*time.Second)
}

// CreateRequestWithTimeout is like CreateRequest but with a custom timeout.
func CreateRequestWithTimeout(ctx context.Context, url string, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, cancel, err
	}
	SetRequestHeaders(req)
	return req, cancel, nil
}

// SetRequestHeaders applies necessary headers to the request.
func SetRequestHeaders(req *http.Request) {
	req.Header.Set("X-Requested-With", "XMLHttpRequest") // Helps avoid Age Verification redirect

	if server.Config.UserAgent != "" {
		req.Header.Set("User-Agent", strings.TrimSpace(server.Config.UserAgent))
	}
	if server.Config.Cookies != "" {
		cookies := ParseCookies(server.Config.Cookies)
		for name, value := range cookies {
			req.AddCookie(&http.Cookie{Name: name, Value: value})
		}
	}

	domain := strings.TrimRight(server.Config.Domain, "/")
	if domain != "" {
		req.Header.Set("Origin", domain)
		req.Header.Set("Referer", domain+"/")
	}
}

// ParseCookies converts a cookie string into a map.
func ParseCookies(cookieStr string) map[string]string {
	cookies := make(map[string]string)
	pairs := strings.Split(cookieStr, ";")

	// Iterate over each cookie pair and extract key-value pairs
	for _, pair := range pairs {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			// Trim spaces around key and value
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			// Store cookie name and value in the map
			cookies[key] = value
		}
	}
	return cookies
}
