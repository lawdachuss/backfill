package uploader

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// seekStreamingSem limits concurrent uploads to SeekStreaming.
const seekStreamingSemCap = 3

// seekStreamingChunkSize is the maximum bytes sent in a single TUS PATCH
// request. Splitting uploads into chunks avoids Cloudflare's 100-second proxy
// timeout on the upstream server, which causes a 502 Bad Gateway for large
// files sent as a single monolithic PATCH body.
const seekStreamingChunkSize = 50 * 1024 * 1024 // 50 MB

var seekStreamingSem = make(chan struct{}, seekStreamingSemCap)

type SeekStreamingUploader struct {
	key    string
	client *http.Client
}

func NewSeekStreamingUploader(key string) *SeekStreamingUploader {
	return &SeekStreamingUploader{
		key: key,
		client: &http.Client{
			Timeout: 120 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    true,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				DisableKeepAlives:     true,
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			},
		},
	}
}

type seekStreamingUploadEndpointResp struct {
	TusURL      string `json:"tusUrl"`
	AccessToken string `json:"accessToken"`
}

func (u *SeekStreamingUploader) Upload(filePath string) (string, error) {
	return u.UploadWithProgress(filePath, nil)
}

// isUploadPayloadTooLarge returns true if the error indicates the upload was
// rejected because the file exceeds the server's size limit (HTTP 413).
// Unlike transient errors (502, 503), a 413 will not resolve on retry because
// the file size does not change between attempts.
func isUploadPayloadTooLarge(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "status 413") ||
		strings.Contains(err.Error(), "413 Payload Too Large")
}

func (u *SeekStreamingUploader) UploadWithProgress(filePath string, progress ProgressFunc) (string, error) {
	seekStreamingSem <- struct{}{}
	defer func() { <-seekStreamingSem }()

	ep, err := u.getUploadEndpoint()
	if err != nil {
		return "", fmt.Errorf("seekstreaming: %w", err)
	}
	fmt.Printf("[seekstreaming] got upload endpoint — tusUrl=%s accessToken=%s\n",
		maskSensitiveURL(ep.TusURL), maskSensitive(ep.AccessToken))

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(uploadBackoff(attempt-2, lastErr))
		}

		uploadURL, err := u.createTUSUpload(ep, filePath)
		if err != nil {
			lastErr = fmt.Errorf("create tus upload: %w", err)
			if isUploadRateLimited(err) {
				time.Sleep(uploadBackoff(attempt, err))
				lastErr = nil
				continue
			}
			// 413 from createTUSUpload is also fatal (Upload-Length header too large)
			if isUploadPayloadTooLarge(err) {
				return "", lastErr
			}
			if attempt < 3 {
				continue
			}
			return "", lastErr
		}

		// Mask the upload URL for logging (it may contain a video ID or token)
		fmt.Printf("[seekstreaming] tus upload created — Location: %s\n", maskSensitiveURL(uploadURL))

		videoID, err := u.uploadFileTUS(uploadURL, filePath, progress)
		if err != nil {
			lastErr = fmt.Errorf("upload file: %w", err)
			if isUploadRateLimited(err) {
				time.Sleep(uploadBackoff(attempt, err))
				lastErr = nil
				continue
			}
			// 413 Payload Too Large is not retryable — the file didn't shrink.
			if isUploadPayloadTooLarge(err) {
				return "", lastErr
			}
			if attempt < 3 {
				continue
			}
			return "", lastErr
		}

		// Log the video ID we extracted from the TUS upload URL
		fmt.Printf("[seekstreaming] upload complete — extracted videoID=%s\n", videoID)

		// Verify the uploaded video exists by calling the manage API.
		// Log the response so we can confirm the video is accessible and
		// see the assetUrl which may indicate the correct embed URL format.
		_, verifyErr := GetSeekStreamingPosterURL(u.key, videoID)
		if verifyErr != nil {
			fmt.Printf("[seekstreaming] WARNING: manage API verification failed for videoID=%s: %v\n",
				videoID, verifyErr)
		} else {
			fmt.Printf("[seekstreaming] manage API OK — assetUrl/poster available for videoID=%s\n", videoID)
		}

		embedURL := fmt.Sprintf("https://chuglii.embedseek.com/#%s", videoID)
		fmt.Printf("[seekstreaming] embed URL: %s\n", embedURL)
		return embedURL, nil
	}
	return "", lastErr
}

func (u *SeekStreamingUploader) getUploadEndpoint() (*seekStreamingUploadEndpointResp, error) {
	req, err := http.NewRequest("GET", "https://seekstreaming.com/api/v1/video/upload", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("api-token", u.key)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status 429: rate limit — %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var ep seekStreamingUploadEndpointResp
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if ep.TusURL == "" || ep.AccessToken == "" {
		return nil, fmt.Errorf("empty tus URL or access token in response")
	}

	return &ep, nil
}

func (u *SeekStreamingUploader) createTUSUpload(ep *seekStreamingUploadEndpointResp, filePath string) (string, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}

	filename := filepath.Base(filePath)
	filetype := mimeTypeByExt(filepath.Ext(filename))

	b64 := func(s string) string {
		return base64.StdEncoding.EncodeToString([]byte(s))
	}

	metadata := fmt.Sprintf("accessToken %s,filename %s,filetype %s", b64(ep.AccessToken), b64(filename), b64(filetype))

	req, err := http.NewRequest("POST", ep.TusURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", fmt.Sprintf("%d", fi.Size()))
	req.Header.Set("Upload-Metadata", metadata)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tus create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("tus create status %d: %s", resp.StatusCode, string(body))
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("missing Location header in tus create response")
	}

	return location, nil
}

func (u *SeekStreamingUploader) uploadFileTUS(uploadURL, filePath string, progress ProgressFunc) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	fi, _ := os.Stat(filePath)
	fileSize := fi.Size()

	offset, err := u.getTUSOffset(uploadURL)
	if err != nil {
		return "", fmt.Errorf("get offset: %w", err)
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("seek to offset %d: %w", offset, err)
		}
	}

	buf := make([]byte, seekStreamingChunkSize)
	for offset < fileSize {
		chunkSize := int64(seekStreamingChunkSize)
		if remaining := fileSize - offset; remaining < chunkSize {
			chunkSize = remaining
		}

		n, err := io.ReadFull(f, buf[:chunkSize])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return "", fmt.Errorf("read chunk at offset %d: %w", offset, err)
		}
		if int64(n) == 0 {
			break
		}

		chunkBody := bytes.NewReader(buf[:n])
		req, err := http.NewRequest("PATCH", uploadURL, chunkBody)
		if err != nil {
			return "", fmt.Errorf("create patch request: %w", err)
		}
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.ContentLength = int64(n)
		req.Header.Set("User-Agent", defaultUserAgent)

		resp, err := u.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("tus upload chunk at offset %d: %w", offset, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("tus upload status %d at offset %d: %s", resp.StatusCode, offset, string(body))
		}

		newOffset := resp.Header.Get("Upload-Offset")
		if newOffset != "" {
			offset, err = strconv.ParseInt(newOffset, 10, 64)
			if err != nil {
				return "", fmt.Errorf("parse upload-offset header: %w", err)
			}
		} else {
			offset += int64(n)
		}

		if progress != nil {
			progress("SeekStreaming", offset, fileSize)
		}
	}

	parts := strings.Split(strings.TrimRight(uploadURL, "/"), "/")
	return parts[len(parts)-1], nil
}

// getTUSOffset performs a TUS HEAD request to determine how many bytes have
// already been uploaded. Returns 0 for new or unknown uploads.
func (u *SeekStreamingUploader) getTUSOffset(uploadURL string) (int64, error) {
	req, err := http.NewRequest("HEAD", uploadURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create head request: %w", err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("head request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return 0, nil
	}

	offsetStr := resp.Header.Get("Upload-Offset")
	if offsetStr == "" {
		return 0, nil
	}

	offset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil {
		return 0, nil
	}
	return offset, nil
}

func ExtractSeekStreamingVideoID(embedURL string) string {
	if idx := strings.LastIndex(embedURL, "#"); idx >= 0 {
		return embedURL[idx+1:]
	}
	return ""
}

func GetSeekStreamingPosterURL(key, videoID string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://seekstreaming.com/api/v1/video/manage/%s", videoID), nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("api-token", key)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	_body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d — %s", resp.StatusCode, strings.TrimSpace(string(_body)))
	}

	var detail struct {
		Poster   string `json:"poster"`
		AssetURL string `json:"assetUrl"`
	}
	if err := json.Unmarshal(_body, &detail); err != nil {
		return "", fmt.Errorf("decode: %w (body: %s)", err, string(_body))
	}
	if detail.Poster == "" || detail.AssetURL == "" {
		return "", fmt.Errorf("no poster available — response: %s", string(_body))
	}
	return detail.AssetURL + detail.Poster, nil
}

// maskSensitive masks a sensitive string for logging (shows first 4 + last 4 chars).
func maskSensitive(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) < 12 {
		return "<masked>"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

// maskSensitiveURL masks sensitive parts of a URL for logging (shows scheme + host, masks path).
func maskSensitiveURL(rawURL string) string {
	if rawURL == "" {
		return "<empty>"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<invalid-url>"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

func mimeTypeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".flv":
		return "video/x-flv"
	case ".ts":
		return "video/mp2t"
	case ".m4v":
		return "video/x-m4v"
	default:
		return "video/mp4"
	}
}
