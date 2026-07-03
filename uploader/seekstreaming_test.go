package uploader

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSeekStreaming413IsFatal verifies that a 413 Payload Too Large response
// from the TUS upload endpoint is treated as a fatal error — the uploader
// returns immediately without retrying.
//
// It uses a mock HTTPS server that simulates the full SeekStreaming upload
// flow: GET upload endpoint → POST create TUS upload → PATCH upload file.
// The PATCH handler returns 413, and the test verifies:
//   - Only one PATCH request was made (no retry)
//   - The error message includes "413"
//
// The mock's custom transport routes all hostnames to the test server via
// DialContext, so getUploadEndpoint(), createTUSUpload(), and uploadFileTUS()
// all reach the mock without needing to modify production URLs.
func TestSeekStreaming413IsFatal(t *testing.T) {
	var (
		getCount   int32 // GET  /api/v1/video/upload
		postCount  int32 // POST /tus/*
		patchCount int32 // PATCH /tus/*
	)

	mockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("mock: %s %s", r.Method, r.URL.Path)

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/video/upload":
			atomic.AddInt32(&getCount, 1)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(seekStreamingUploadEndpointResp{
				TusURL:      "https://seekstreaming.com/tus/upload123",
				AccessToken: "test-access-token",
			}); err != nil {
				t.Errorf("mock: encode upload endpoint response: %v", err)
			}

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/tus/"):
			atomic.AddInt32(&postCount, 1)
			w.Header().Set("Location", "https://seekstreaming.com/tus/upload123")
			w.WriteHeader(http.StatusCreated)

		case r.Method == "HEAD" && strings.HasPrefix(r.URL.Path, "/tus/"):
			w.Header().Set("Upload-Offset", "0")
			w.Header().Set("Tus-Resumable", "1.0.0")
			w.WriteHeader(http.StatusOK)

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/tus/"):
			atomic.AddInt32(&patchCount, 1)
			// Simulate a Cloudflare-wrapped 413 just like the real error.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			fmt.Fprint(w, `<html><head><title>413 Payload Too Large</title></head>
<body><center><h1>413 Payload Too Large</h1></center>
<hr><center>cloudflare</center></body></html>`)

		default:
			t.Errorf("mock: unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	mockURL, err := url.Parse(mockServer.URL)
	if err != nil {
		t.Fatalf("parse mock server URL: %v", err)
	}
	mockHost := mockURL.Host // "127.0.0.1:PORT"

	uploader := NewSeekStreamingUploader("test-api-key")
	uploader.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Route all connections to the mock server, regardless
				// of the original hostname in the URL.
				return net.Dial("tcp", mockHost)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Create a temp file with enough content to simulate a real upload.
	tmpFile, err := os.CreateTemp("", "seekstreaming-test-*.mp4")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString("fake-video-content-for-413-test"); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		t.Fatalf("write temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Upload should fail immediately with a 413 error.
	_, err = uploader.Upload(tmpPath)
	if err == nil {
		t.Fatal("expected 413 Payload Too Large error, got nil")
	}
	t.Logf("upload error: %v", err)

	// Verify the error mentions 413.
	if !strings.Contains(err.Error(), "413") {
		t.Fatalf("error should mention 413, got: %v", err)
	}

	// Verify exactly one PATCH request — no retries.
	if n := atomic.LoadInt32(&patchCount); n != 1 {
		t.Fatalf("expected exactly 1 PATCH request (413 should not retry), got %d", n)
	}

	// Verify GET and POST each happened once (happy path for those phases).
	if n := atomic.LoadInt32(&getCount); n != 1 {
		t.Fatalf("expected 1 GET upload endpoint request, got %d", n)
	}
	if n := atomic.LoadInt32(&postCount); n != 1 {
		t.Fatalf("expected 1 POST create TUS request, got %d", n)
	}
}

// TestSeekStreaming413FromCreateIsFatal verifies that a 413 returned at the
// TUS creation step (POST) is also treated as fatal without retrying.
// This tests the isUploadPayloadTooLarge check inside the createTUSUpload
// error path.
func TestSeekStreaming413FromCreateIsFatal(t *testing.T) {
	var (
		getCount  int32
		postCount int32
	)

	mockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("mock: %s %s", r.Method, r.URL.Path)

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/video/upload":
			atomic.AddInt32(&getCount, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(seekStreamingUploadEndpointResp{
				TusURL:      "https://seekstreaming.com/tus/upload456",
				AccessToken: "test-token",
			})

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/tus/"):
			atomic.AddInt32(&postCount, 1)
			// TUS creation itself returns 413 (Upload-Length too large).
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			fmt.Fprint(w, `413 Payload Too Large`)

		default:
			t.Errorf("mock: unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	mockURL, _ := url.Parse(mockServer.URL)

	uploader := NewSeekStreamingUploader("test-key")
	uploader.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", mockURL.Host)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	tmpFile, _ := os.CreateTemp("", "seekstreaming-test-*.mp4")
	tmpPath := tmpFile.Name()
	tmpFile.WriteString("fake-content")
	tmpFile.Close()
	defer os.Remove(tmpPath)

	_, err := uploader.Upload(tmpPath)
	if err == nil {
		t.Fatal("expected 413 error from TUS creation, got nil")
	}
	t.Logf("upload error: %v", err)

	if !strings.Contains(err.Error(), "413") {
		t.Fatalf("error should mention 413, got: %v", err)
	}

	// Only 1 POST to create — no retry.
	if n := atomic.LoadInt32(&postCount); n != 1 {
		t.Fatalf("expected exactly 1 POST (413 should not retry), got %d", n)
	}

	// GET should have happened exactly once.
	if n := atomic.LoadInt32(&getCount); n != 1 {
		t.Fatalf("expected 1 GET, got %d", n)
	}
}

// TestIsUploadPayloadTooLarge is a fast unit test that directly validates
// the error-detection function without any HTTP calls or retries.
func TestIsUploadPayloadTooLarge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "other error", err: fmt.Errorf("something else"), want: false},
		{name: "502 Bad Gateway", err: fmt.Errorf("tus upload status 502: Bad Gateway"), want: false},
		{name: "503 Service Unavailable", err: fmt.Errorf("tus upload status 503: Service Unavailable"), want: false},
		{name: "413 from PATCH", err: fmt.Errorf("upload file: tus upload status 413: Payload Too Large"), want: true},
		{name: "413 from POST create", err: fmt.Errorf("create tus upload: tus create status 413: Payload Too Large"), want: true},
		{name: "413 with full HTML body", err: fmt.Errorf("tus upload status 413: <html><title>413 Payload Too Large</title></html>"), want: true},
		{name: "413 wrapped by fmt.Errorf", err: fmt.Errorf("upload file: %w", fmt.Errorf("tus upload status 413: too big")), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUploadPayloadTooLarge(tt.err)
			if got != tt.want {
				t.Errorf("isUploadPayloadTooLarge(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestSeekStreaming502IsRetryable verifies that a transient 502 Bad Gateway
// IS retried (unlike 413). This serves as a regression guard to make sure
// the 413 check doesn't accidentally block legitimate retries.
//
// This test takes ~15s due to the production retry backoff (5s + 10s sleeps).
// Use go test -short to skip it and rely on the fast direct unit tests above.
func TestSeekStreaming502IsRetryable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow 502 retry test (~15s); TestIsUploadPayloadTooLarge covers detection logic")
	}
	var (
		getCount   int32
		postCount  int32
		patchCount int32
	)

	mockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("mock: %s %s", r.Method, r.URL.Path)

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/video/upload":
			atomic.AddInt32(&getCount, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(seekStreamingUploadEndpointResp{
				TusURL:      "https://seekstreaming.com/tus/upload789",
				AccessToken: "test-token",
			})

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/tus/"):
			atomic.AddInt32(&postCount, 1)
			w.Header().Set("Location", "https://seekstreaming.com/tus/upload789")
			w.WriteHeader(http.StatusCreated)

		case r.Method == "HEAD" && strings.HasPrefix(r.URL.Path, "/tus/"):
			w.Header().Set("Upload-Offset", "0")
			w.Header().Set("Tus-Resumable", "1.0.0")
			w.WriteHeader(http.StatusOK)

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/tus/"):
			n := atomic.AddInt32(&patchCount, 1)
			// Return 502 for the first two PATCH attempts, then 204 on the
			// third (final) attempt to let the upload succeed.
			if n < 3 {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `<html><head><title>502 Bad Gateway</title></head></html>`)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}

		default:
			t.Errorf("mock: unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	mockURL, _ := url.Parse(mockServer.URL)

	uploader := NewSeekStreamingUploader("test-key")
	uploader.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", mockURL.Host)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	tmpFile, _ := os.CreateTemp("", "seekstreaming-test-*.mp4")
	tmpPath := tmpFile.Name()
	tmpFile.WriteString("fake-content")
	tmpFile.Close()
	defer os.Remove(tmpPath)

	url, err := uploader.Upload(tmpPath)
	if err != nil {
		t.Fatalf("expected eventual success after 502 retries, got: %v", err)
	}
	t.Logf("upload URL: %s", url)

	// Should have retried: 3 PATCH attempts (2 502s → 1 success).
	if n := atomic.LoadInt32(&patchCount); n != 3 {
		t.Fatalf("expected 3 PATCH requests (2 retries after 502), got %d", n)
	}
}
