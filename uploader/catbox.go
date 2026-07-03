package uploader

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CatboxUploader handles uploading files to Catbox.moe.
// Anonymous uploads are supported. For more reliable uploads, set the
// CATBOX_USERHASH environment variable (find it on catbox.moe after logging in).
type CatboxUploader struct {
	client   *http.Client
	userhash string
}

// NewCatboxUploader creates a new Catbox.moe uploader.
// Reads CATBOX_USERHASH from the environment for authenticated uploads.
// Uses newNoProxyClient — uploads must never route through the Chaturbate SOCKS5 proxy.
func NewCatboxUploader() *CatboxUploader {
	return &CatboxUploader{
		client:   newNoProxyClient(5 * time.Minute),
		userhash: os.Getenv("CATBOX_USERHASH"),
	}
}

// Upload uploads a file to Catbox.moe and returns the direct file URL.
// Retries with MIME type fallback.
//
// API: POST https://catbox.moe/user/api.php
// Fields: reqtype=fileupload, fileToUpload=@file (multipart)
//         userhash=<hash> (optional, for authenticated uploads)
// Response on success: plain text URL like "https://files.catbox.moe/abc123.webp"
// Response on error: plain text error message.
//
// Upload strategy (tried in order):
//   1. Authenticated upload with application/octet-stream (bypasses proxy IP blocks)
//   2. If 412: authenticated upload with video/mp4 MIME type
//   3. If still failing: retry up to 3 times with exponential backoff
func (u *CatboxUploader) Upload(filePath string) (string, error) {
	mimeTypes := []string{"application/octet-stream", "video/mp4"}
	useAuth := u.userhash != ""

	var lastErr error
	for _, mimeType := range mimeTypes {
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
			}

			url, err := u.uploadOnce(filePath, mimeType, useAuth)
			if err == nil {
				return url, nil
			}

			lastErr = err
			errStr := err.Error()

			// "Uploads paused" = Catbox has permanently disabled uploads — fail immediately
			if strings.Contains(errStr, "Uploads paused") {
				return "", err
			}

			// 412 "Invalid uploader" = IP blocked / wrong MIME — try next MIME or fail
			if strings.Contains(errStr, "412") || strings.Contains(errStr, "Invalid uploader") {
				break
			}

			// Non-retryable errors: fail immediately
			if !isRetryableCatboxError(err) {
				return "", err
			}
		}
	}

	return "", fmt.Errorf("catbox: all strategies failed, last: %w", lastErr)
}

// uploadOnce streams the file through io.Pipe with a standard multipart writer.
// This is the canonical Go approach to multipart uploads — it guarantees
// correct boundary formatting and part ordering that Catbox expects.
//
// mimeType is the Content-Type for the file part (e.g. "application/octet-stream" or "video/mp4").
// useAuth controls whether the userhash field is included.
func (u *CatboxUploader) uploadOnce(filePath string, mimeType string, useAuth bool) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("catbox: open file: %w", err)
	}
	defer file.Close()

	// Use io.Pipe so the multipart writer writes directly into the request body,
	// streaming the file without buffering it in RAM. The multipart writer
	// handles all boundary formatting correctly.
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)

	errChan := make(chan error, 1)
	go func() {
		defer pipeWriter.Close()
		defer writer.Close()

		// reqtype is always required
		if err := writer.WriteField("reqtype", "fileupload"); err != nil {
			errChan <- fmt.Errorf("write reqtype: %w", err)
			return
		}

		// Send userhash if available and requested (authenticated uploads bypass IP blocks)
		if useAuth && u.userhash != "" {
			if err := writer.WriteField("userhash", u.userhash); err != nil {
				errChan <- fmt.Errorf("write userhash: %w", err)
				return
			}
		}

		// Use CreatePart instead of CreateFormFile so we can set the correct
		// Content-Type for the file (video/mp4 vs application/octet-stream).
		// Catbox may handle files differently based on MIME type.
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="fileToUpload"; filename="%s"`, filepath.Base(filePath)))
		h.Set("Content-Type", mimeType)
		part, err := writer.CreatePart(h)
		if err != nil {
			errChan <- fmt.Errorf("create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("copy file: %w", err)
			return
		}

		errChan <- nil
	}()

	req, err := http.NewRequest("POST", "https://catbox.moe/user/api.php", pipeReader)
	if err != nil {
		pipeReader.CloseWithError(err)
		return "", fmt.Errorf("catbox: create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://catbox.moe")
	req.Header.Set("Referer", "https://catbox.moe/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Priority", "u=1, i")

	resp, err := u.client.Do(req)
	if err != nil {
		pipeReader.CloseWithError(err)
		// Drain error channel to avoid goroutine leak
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("catbox: send request: %w", err)
	}
	defer resp.Body.Close()

	// Check for errors from the goroutine
	select {
	case err := <-errChan:
		if err != nil {
			return "", err
		}
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("catbox: timeout waiting for file copy to complete")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("catbox: read response: %w", err)
	}

	text := strings.TrimSpace(string(raw))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("catbox: status %d: %s", resp.StatusCode, text)
	}

	if text == "" {
		return "", fmt.Errorf("catbox: empty response")
	}

	if !strings.HasPrefix(text, "https://") {
		return "", fmt.Errorf("catbox: unexpected response: %s", text)
	}

	return text, nil
}

// isRetryableCatboxError returns true if the error represents a transient
// failure that might succeed on retry.
func isRetryableCatboxError(err error) bool {
	errStr := err.Error()

	if strings.Contains(errStr, "status 5") {
		return true
	}

	// "Invalid uploader" (HTTP 412) — Catbox may be rate-limiting or
	// blocking the IP. Sleep with backoff and retry.
	if strings.Contains(errStr, "Invalid uploader") {
		return true
	}

	if strings.Contains(errStr, "stat file") || strings.Contains(errStr, "open file") {
		return true
	}

	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}

	return false
}
