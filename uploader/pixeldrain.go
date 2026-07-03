package uploader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PixelDrainUploader handles uploading files to PixelDrain.
// Uses the POST API with multipart form and HTTP Basic auth.
// Returns a direct file URL suitable for embedding.
//
// API: POST https://pixeldrain.com/api/file with multipart/form-data
// Auth: Basic with empty username + API key as password
// Fields: file=@file
// Response: {"id":"abc123","success":true}
// File URL: https://pixeldrain.com/api/file/{id}
type PixelDrainUploader struct {
	apiKey string
	client *http.Client
}

// NewPixelDrainUploader creates a new PixelDrain uploader.
// Provide an empty apiKey for anonymous uploads (may not be supported).
func NewPixelDrainUploader(apiKey string) *PixelDrainUploader {
	return &PixelDrainUploader{
		apiKey: apiKey,
		client: newDefaultClient(5 * time.Minute),
	}
}

type pixelDrainUploadResponse struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
}

// Upload uploads a file to PixelDrain and returns the direct file URL.
// Retries up to 3 times with exponential backoff (2s, 4s) on transient errors.
func (u *PixelDrainUploader) Upload(filePath string) (string, error) {
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			time.Sleep(backoff)
		}

		url, err := u.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}

		lastErr = err

		if isRetryablePixelDrainError(err) {
			continue
		}

		return "", err
	}

	return "", fmt.Errorf("pixeldrain: all 3 attempts failed, last: %w", lastErr)
}

func (u *PixelDrainUploader) uploadOnce(filePath string) (string, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)

	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: open file: %w", err)
	}
	defer file.Close()

	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		mw.Close()
		return "", fmt.Errorf("pixeldrain: create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		mw.Close()
		return "", fmt.Errorf("pixeldrain: copy file: %w", err)
	}

	mw.Close()

	req, err := http.NewRequest("POST", "https://pixeldrain.com/api/file", &b)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: create request: %w", err)
	}

	req.ContentLength = int64(b.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	if u.apiKey != "" {
		req.SetBasicAuth("", u.apiKey)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("pixeldrain: read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pixeldrain: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var uploadResp pixelDrainUploadResponse
	if err := json.Unmarshal(raw, &uploadResp); err != nil {
		return "", fmt.Errorf("pixeldrain: decode response: %w", err)
	}

	if !uploadResp.Success || uploadResp.ID == "" {
		return "", fmt.Errorf("pixeldrain: upload failed: %s", strings.TrimSpace(string(raw)))
	}

	return fmt.Sprintf("https://pixeldrain.com/api/file/%s", uploadResp.ID), nil
}

func isRetryablePixelDrainError(err error) bool {
	errStr := err.Error()

	// 5xx server errors — retry
	if strings.HasPrefix(errStr, "pixeldrain: status 5") {
		return true
	}

	// File stat/open errors — retry (AV scanner race on Windows)
	if strings.Contains(errStr, "stat file") || strings.Contains(errStr, "open file") {
		return true
	}

	// Network-level failures — retry
	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}

	return false
}
