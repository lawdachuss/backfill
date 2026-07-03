package uploader

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type X02Uploader struct {
	apiKey string
	client *http.Client
}

func NewX02Uploader(apiKey string) *X02Uploader {
	return &X02Uploader{
		apiKey: apiKey,
		client: newNoProxyClient(5 * time.Minute),
	}
}

func (u *X02Uploader) Upload(filePath string) (string, error) {
	if u.apiKey == "" {
		return "", fmt.Errorf("x02: API key not set (set X02_API_KEY env var)")
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
		if strings.Contains(err.Error(), "Invalid API key") {
			return "", err
		}
		if isRetryableX02Error(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("x02: all 3 attempts failed, last: %w", lastErr)
}

func (u *X02Uploader) uploadOnce(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("x02: open file: %w", err)
	}
	defer file.Close()

	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)

	errChan := make(chan error, 1)
	go func() {
		defer pipeWriter.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			errChan <- fmt.Errorf("x02: create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("x02: copy file: %w", err)
			return
		}

		errChan <- nil
	}()

	req, err := http.NewRequest("POST", "https://x02.me/api/upload", pipeReader)
	if err != nil {
		pipeReader.CloseWithError(err)
		return "", fmt.Errorf("x02: create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", u.apiKey)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		pipeReader.CloseWithError(err)
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("x02: send request: %w", err)
	}
	defer resp.Body.Close()

	select {
	case err := <-errChan:
		if err != nil {
			return "", err
		}
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("x02: timeout waiting for file copy to complete")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("x02: read response: %w", err)
	}

	text := strings.TrimSpace(string(raw))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("x02: status %d: %s", resp.StatusCode, text)
	}

	if text == "" {
		return "", fmt.Errorf("x02: empty response")
	}

	if !strings.HasPrefix(text, "https://") {
		return "", fmt.Errorf("x02: unexpected response: %s", text)
	}

	return text, nil
}

func isRetryableX02Error(err error) bool {
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
