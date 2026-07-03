package uploader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SupabaseStorageUploader struct {
	supabaseURL string
	serviceKey  string
	bucketName  string
	client      *http.Client
}

func NewSupabaseStorageUploader() *SupabaseStorageUploader {
	return &SupabaseStorageUploader{
		supabaseURL: os.Getenv("SUPABASE_URL"),
		serviceKey:  os.Getenv("SUPABASE_SERVICE_ROLE_KEY"),
		bucketName:  "previews",
		client:      newNoProxyClient(5 * time.Minute),
	}
}

func (u *SupabaseStorageUploader) Upload(filePath string) (string, error) {
	if u.supabaseURL == "" {
		return "", fmt.Errorf("supabase_storage: SUPABASE_URL not set")
	}
	if u.serviceKey == "" {
		return "", fmt.Errorf("supabase_storage: SUPABASE_SERVICE_ROLE_KEY not set")
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
		if isRetryableSupabaseError(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("supabase_storage: all 3 attempts failed, last: %w", lastErr)
}

func (u *SupabaseStorageUploader) uploadOnce(filePath string) (string, error) {
	fileName := filepath.Base(filePath)
	objectPath := fmt.Sprintf("%s/%s", u.bucketName, fileName)

	file, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("supabase_storage: read file: %w", err)
	}

	url := fmt.Sprintf("%s/storage/v1/object/%s", u.supabaseURL, objectPath)
	req, err := http.NewRequest("POST", url, bytes.NewReader(file))
	if err != nil {
		return "", fmt.Errorf("supabase_storage: create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+u.serviceKey)
	req.Header.Set("Content-Type", "video/mp4")
	req.Header.Set("x-upsert", "true")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("supabase_storage: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("supabase_storage: read response: %w", err)
	}

	text := strings.TrimSpace(string(raw))

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("supabase_storage: status %d: %s", resp.StatusCode, text)
	}

	// The public URL for the uploaded file
	publicURL := fmt.Sprintf("%s/storage/v1/object/public/%s", u.supabaseURL, objectPath)
	return publicURL, nil
}

// EnsureBucket ensures the previews bucket exists, creating it if necessary.
func (u *SupabaseStorageUploader) EnsureBucket() error {
	if u.supabaseURL == "" || u.serviceKey == "" {
		return nil
	}

	// Check if bucket exists
	checkURL := fmt.Sprintf("%s/storage/v1/bucket/%s", u.supabaseURL, u.bucketName)
	req, err := http.NewRequest("GET", checkURL, nil)
	if err != nil {
		return fmt.Errorf("supabase_storage: create bucket check request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.serviceKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("supabase_storage: check bucket: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// Create bucket as public
	body := map[string]interface{}{
		"id":     u.bucketName,
		"name":   u.bucketName,
		"public": true,
	}
	bodyJSON, _ := json.Marshal(body)

	createURL := fmt.Sprintf("%s/storage/v1/bucket", u.supabaseURL)
	req, err = http.NewRequest("POST", createURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return fmt.Errorf("supabase_storage: create bucket request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.serviceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err = u.client.Do(req)
	if err != nil {
		return fmt.Errorf("supabase_storage: create bucket: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase_storage: create bucket status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	return nil
}

func isRetryableSupabaseError(err error) bool {
	errStr := err.Error()
	if strings.Contains(errStr, "status 5") {
		return true
	}
	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}
	return false
}
