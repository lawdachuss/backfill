package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sardanioss/httpcloak"
	"github.com/teacat/chaturbate-dvr/server"
)

// httpcloakTransport wraps httpcloak.Client as an http.RoundTripper.
// It emulates a Chrome 146 TLS/HTTP2 fingerprint to bypass Cloudflare WAF
// TCP RST that Go's default crypto/tls triggers.
// ECH (Encrypted Client Hello) hides the SNI from network observers for
// better Cloudflare bot scores.
//
// When the SOCKS5 proxy is unreachable (i/o timeout, connection refused),
// automatically rotates to the next proxy URL in the list. This handles
// the case where free proxy servers are intermittently available.
type httpcloakTransport struct {
	mu        sync.Mutex
	client    *httpcloak.Client
	proxyURLs []string
	proxyIdx  int
}

// sharedTransportSingleton is a singleton http.RoundTripper for the shared transport.
var sharedTransportSingleton http.RoundTripper
var sharedTransportOnce sync.Once

func getSharedTransport() http.RoundTripper {
	sharedTransportOnce.Do(func() {
		proxyURLs := configuredProxyURLs()
		client := newCloakClient(proxyURLAt(proxyURLs, 0))
		sharedTransportSingleton = &httpcloakTransport{
			client:    client,
			proxyURLs: proxyURLs,
		}
	})
	return sharedTransportSingleton
}

func proxyURLAt(urls []string, idx int) string {
	if len(urls) == 0 {
		return ""
	}
	return urls[idx%len(urls)]
}

// newCloakClient creates a new httpcloak client with the given proxy URL.
func newCloakClient(proxyURL string) *httpcloak.Client {
	opts := []httpcloak.Option{
		httpcloak.WithTimeout(120 * time.Second),
	}
	if proxyURL != "" {
		opts = append(opts, httpcloak.WithProxy(proxyURL))
	}
	return httpcloak.New("chrome-146-windows", opts...)
}

// configuredProxyURLs returns all proxy URLs (supports comma-separated for failover).
func configuredProxyURLs() []string {
	if server.Config == nil {
		return nil
	}
	raw := strings.TrimSpace(server.Config.ProxyURL)
	if raw == "" {
		return nil
	}

	username := strings.TrimSpace(server.Config.ProxyUsername)
	password := strings.TrimSpace(server.Config.ProxyPassword)

	var urls []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = applyProxyAuth(part, username, password)
		urls = append(urls, part)
	}
	return urls
}

func applyProxyAuth(proxyURL, username, password string) string {
	if username == "" && password == "" {
		return proxyURL
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return proxyURL
	}
	if password != "" {
		u.User = url.UserPassword(username, password)
	} else {
		u.User = url.User(username)
	}
	return u.String()
}

// rotateProxy recreates the httpcloak client with the next proxy in the list.
// Returns true if a different proxy was selected.
func (t *httpcloakTransport) rotateProxy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.proxyURLs) <= 1 {
		return false
	}

	t.proxyIdx++
	proxyURL := proxyURLAt(t.proxyURLs, t.proxyIdx)

	// Close old client if it exposes a Close method
	if c, ok := interface{}(t.client).(interface{ Close() error }); ok {
		c.Close()
	}

	t.client = newCloakClient(proxyURL)
	return true
}

// refreshProxies re-reads the proxy list from the current config and
// resets the client to use the first (presumably freshest) proxy.
// Returns true if new proxies were loaded, false if the list is empty.
// This is called when all proxies in the current list have failed,
// allowing the DVR to pick up environment variable updates without a restart.
func (t *httpcloakTransport) refreshProxies() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	newProxies := configuredProxyURLs()
	if len(newProxies) == 0 {
		return false
	}

	// Close old client if it exposes a Close method
	if c, ok := interface{}(t.client).(interface{ Close() error }); ok {
		c.Close()
	}

	t.proxyURLs = newProxies
	t.proxyIdx = 0
	t.client = newCloakClient(proxyURLAt(newProxies, 0))
	return true
}

// WarmupChaturbate makes an initial request to chaturbate.com to establish
// TLS session tickets with Cloudflare before any API calls are made.
// This gives subsequent requests TLS session resumption, making them look
// more like a returning browser visitor.
// Uses a single-attempt round trip — warmup is best-effort and should not
// retry through multiple proxies (that can delay startup by 30s per domain).
func WarmupChaturbate(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://chaturbate.com/", nil)
	if err != nil {
		return
	}
	SetRequestHeaders(req)
	t, ok := getSharedTransport().(*httpcloakTransport)
	if !ok {
		return
	}
	resp, err := t.roundTripOnce(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// WarmupStripchat makes an initial request to stripchat.com to establish TLS
// session tickets before any API calls are made. This is the same idea as
// WarmupChaturbate but for Stripchat's domain.
func WarmupStripchat(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", "https://stripchat.com/", nil)
	if err != nil {
		return
	}
	SetRequestHeaders(req)
	t, ok := getSharedTransport().(*httpcloakTransport)
	if !ok {
		return
	}
	resp, err := t.roundTripOnce(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// isProxyError checks if an error is a proxy connection failure (SOCKS5 unreachable).
// These errors trigger automatic proxy rotation.
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SOCKS5 CONNECT failed") ||
		strings.Contains(msg, "connect to SOCKS5 proxy") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no reachable proxy") ||
		strings.Contains(msg, "tls_handshake")
}

// cdnHostSuffixes lists CDN hostname suffixes that serve HLS segments
// with signed URLs (pkey/token). These hosts are directly reachable from
// any region — the proxy is only needed for geo-unblocking API requests
// (chaturbate.com, stripchat.com). Bypassing the proxy for CDN eliminates
// the slow-proxy → timeout → pkey-expiry failure chain.
var cdnHostSuffixes = []string{
	".doppiocdn.net",
	".doppiocdn.com",
	".live.mmcdn.com",
}

// proxyBypassHosts lists hosts that should never use the proxy.
// Stripchat doesn't need a Netherlands proxy — it has no age verification.
var proxyBypassHosts = []string{
	"stripchat.com",
	".stripchat.com",
}

func isCDNHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range cdnHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func isProxyBypassHost(host string) bool {
	host = strings.ToLower(host)
	for _, h := range proxyBypassHosts {
		if host == h || strings.HasSuffix(host, h) {
			return true
		}
	}
	return false
}

// roundTripOnce executes a single request attempt using the current httpcloak
// client. No proxy rotation — used by warmup functions (best-effort).
func (t *httpcloakTransport) roundTripOnce(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" || isCDNHost(req.URL.Host) || isProxyBypassHost(req.URL.Host) {
		return http.DefaultTransport.RoundTrip(req)
	}

	ctx := req.Context()
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	t.mu.Lock()
	client := t.client
	t.mu.Unlock()

	cloakReq := &httpcloak.Request{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
	}
	if len(bodyBytes) > 0 {
		cloakReq.Body = bytes.NewReader(bodyBytes)
	}

	cloakResp, err := client.Do(ctx, cloakReq)
	if err != nil {
		return nil, err
	}

	body, bodyErr := cloakResp.Bytes()
	if bodyErr != nil {
		cloakResp.Close()
		return nil, bodyErr
	}

	resp := &http.Response{
		StatusCode: cloakResp.StatusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
	if cloakResp.Headers != nil {
		for k, vs := range cloakResp.Headers {
			for _, v := range vs {
				resp.Header.Add(k, v)
			}
		}
	}
	return resp, nil
}

// RoundTrip implements http.RoundTripper. CDN requests bypass the proxy
// entirely. API requests use httpcloak with the SOCKS5 proxy, and
// automatically rotate to the next proxy on connection failure.
func (t *httpcloakTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" || isCDNHost(req.URL.Host) || isProxyBypassHost(req.URL.Host) {
		return http.DefaultTransport.RoundTrip(req)
	}

	ctx := req.Context()
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	// Prepare request body once, reuse across retries
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	// Try up to len(proxyURLs) attempts, rotating proxy on connection failures.
	// If all proxies fail, try to refresh the proxy list from env and retry once.
	// Track per-proxy errors for better diagnostics.
	for refresh := 0; refresh < 2; refresh++ {
		for attempt := 0; attempt < max(1, len(t.proxyURLs)); attempt++ {
			t.mu.Lock()
			client := t.client
			currentProxy := proxyURLAt(t.proxyURLs, t.proxyIdx)
			t.mu.Unlock()

			cloakReq := &httpcloak.Request{
				Method:  req.Method,
				URL:     req.URL.String(),
				Headers: req.Header,
			}
			if len(bodyBytes) > 0 {
				cloakReq.Body = bytes.NewReader(bodyBytes)
			}

			cloakResp, err := client.Do(ctx, cloakReq)

			if err == nil {
				body, bodyErr := cloakResp.Bytes()
				if bodyErr != nil {
					cloakResp.Close()
					return nil, bodyErr
				}

				resp := &http.Response{
					StatusCode: cloakResp.StatusCode,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(body)),
					Request:    req,
				}
				if cloakResp.Headers != nil {
					for k, vs := range cloakResp.Headers {
						for _, v := range vs {
							resp.Header.Add(k, v)
						}
					}
				}
				return resp, nil
			}

			// Proxy connection failure — rotate to next proxy in the list
			if isProxyError(err) {
				if t.rotateProxy() {
					continue
				}
				// Only 1 proxy URL configured and it failed — log the specific error
				return nil, fmt.Errorf("proxy %s: %w", maskProxyHost(currentProxy), err)
			}
			// Non-proxy error (e.g. HTTP-level failure) — surface immediately
			return nil, err
		}

		// All proxies in the current list failed. Try to refresh from env.
		// This handles the case where free proxies have died and the env
		// was updated (e.g. by a wrapper script that re-fetches proxy lists).
		if t.refreshProxies() {
			fmt.Printf("[proxy] all proxies failed — refreshed proxy list from env, retrying with %d URLs\n",
				len(t.proxyURLs))
			continue
		}
		break
	}

	// Build a detailed error message with all proxy URLs tried
	t.mu.Lock()
	proxyCount := len(t.proxyURLs)
	firstProxy := ""
	if proxyCount > 0 {
		firstProxy = maskProxyHost(t.proxyURLs[0])
	}
	t.mu.Unlock()

	return nil, fmt.Errorf("all %d proxies failed (first: %s) — proxy URLs may be unreachable; check proxy configuration or refresh the proxy list",
		proxyCount, firstProxy)
}

// maskProxyHost masks the password portion of a proxy URL for safe logging.
// e.g. "socks5://user:pass@host:1080" → "socks5://user:***@host:1080"
func maskProxyHost(proxyURL string) string {
	if proxyURL == "" {
		return "(none)"
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		// If we can't parse it, just show the scheme + host
		if strings.Contains(proxyURL, "@") {
			parts := strings.SplitN(proxyURL, "@", 2)
			return "***@" + parts[len(parts)-1]
		}
		return proxyURL
	}
	if u.User != nil {
		if _, hasPW := u.User.Password(); hasPW {
			u.User = url.UserPassword(u.User.Username(), "***")
		} else {
			u.User = url.User(u.User.Username())
		}
		return u.String()
	}
	// No auth — just show the host:port
	if u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return proxyURL
}

// max returns the larger of a and b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
