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

type LobFileUploader struct {
	apiKey string
	client *http.Client
}

func NewLobFileUploader(apiKey string) *LobFileUploader {
	return &LobFileUploader{
		apiKey: apiKey,
		client: newNoProxyClient(10 * time.Minute),
	}
}

type lobFileUploadResponse struct {
	Success bool   `json:"success"`
	URL     string `json:"url"`
	Error   string `json:"error,omitempty"`
}

func (u *LobFileUploader) Upload(filePath string) (string, error) {
	if u.apiKey == "" {
		return "", fmt.Errorf("lobfile: API key not set (set LOBFILE_API_KEY env var)")
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
		}
		url, err := u.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if isRetryableLobFileError(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("lobfile: all 3 attempts failed, last: %w", lastErr)
}

func (u *LobFileUploader) uploadOnce(filePath string) (string, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)

	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("lobfile: open file: %w", err)
	}
	defer file.Close()

	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		mw.Close()
		return "", fmt.Errorf("lobfile: create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		mw.Close()
		return "", fmt.Errorf("lobfile: copy file: %w", err)
	}

	mw.Close()

	req, err := http.NewRequest("POST", "https://lobfile.com/api/v3/upload", &b)
	if err != nil {
		return "", fmt.Errorf("lobfile: create request: %w", err)
	}

	req.ContentLength = int64(b.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("X-API-Key", u.apiKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("lobfile: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return "", fmt.Errorf("lobfile: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lobfile: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result lobFileUploadResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("lobfile: decode response: %w", err)
	}

	if !result.Success {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = string(raw)
		}
		return "", fmt.Errorf("lobfile: upload failed: %s", errMsg)
	}

	if result.URL == "" {
		return "", fmt.Errorf("lobfile: empty URL in response")
	}

	return result.URL, nil
}

func isRetryableLobFileError(err error) bool {
	errStr := err.Error()
	if strings.Contains(errStr, "status 5") {
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
