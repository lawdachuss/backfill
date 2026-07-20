package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ─── Shared media-fetch client ───────────────────────────────────────────────
//
// The standalone media-fetch helpers below (SeekStreaming + UPnShare) make only
// simple GET requests to each host's public manage API. They share one
// rate-limited HTTP client so the backfill never storms a single host.

var mediaFetchClient = &http.Client{Timeout: 30 * time.Second}

// ─── UPnShare media (poster + preview) fetch ─────────────────────────────────
//
// These helpers are used by the backfill worker to populate thumbnail_url and
// preview_url for recordings that live on UPnShare, without re-uploading or
// generating sprites. Mirrors the logic in the node-3 recorder's uploader.

type upnshareManageVideo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Poster   string `json:"poster"`
	Preview  string `json:"preview"`
	AssetURL string `json:"assetUrl"`
}

type upnshareManageListResp struct {
	Data []upnshareManageVideo `json:"data"`
}

// ExtractUPnShareVideoID returns the video ID from a UPnShare embed URL of the
// form https://<domain>/#<id>.
func ExtractUPnShareVideoID(embedURL string) string {
	if idx := strings.LastIndex(embedURL, "#"); idx >= 0 {
		return embedURL[idx+1:]
	}
	return ""
}

// GetUPnShareMediaURLs fetches the poster and preview URLs for a UPnShare video.
//
// The recorder rotates through multiple API keys on upload, so a given video may
// live under ANY of the configured keys. We therefore try every key until one
// returns a match. The reliable lookup is a manage-list search by the recording's
// full filename (UPnShare indexes videos by original filename), matched on exact
// name — the same path the recorder uses.
func GetUPnShareMediaURLs(apiKeys []string, videoID, filename string) (posterURL, previewURL string, err error) {
	if len(apiKeys) == 0 {
		return "", "", fmt.Errorf("UPnShare key not configured")
	}

	// Resolve via the manage list, trying each key in turn.
	for _, apiKey := range apiKeys {
		if filename != "" {
			if list, se := searchUPnShareByName(filename, apiKey); se == nil {
				for i := range list {
					if list[i].Name == filename {
						p, pr := buildUPnShareURLs(&list[i])
						return p, pr, nil
					}
				}
			}

			// Fallback: search by username + timestamp-token match for recordings
			// whose stored filename differs from the host's name.
			user := upnshareUsername(filename)
			token := upnshareTimestampToken(filename)
			if user != "" {
				if list, se := searchUPnShareByName(user, apiKey); se == nil {
					if v := matchUPnShareVideo(list, filename, token, videoID); v != nil {
						p, pr := buildUPnShareURLs(v)
						return p, pr, nil
					}
				}
			}
		}

		// Last resort: by-ID lookup (works only when the player ID equals the
		// manage id). Tried per-key since the id may be scoped to a key.
		if videoID != "" {
			if detail, e := fetchUPnShareByID(apiKey, videoID); e == nil {
				p, pr := buildUPnShareURLs(detail)
				return p, pr, nil
			}
		}
	}

	return "", "", fmt.Errorf("UPnShare media not available yet for %s", videoID)
}

func fetchUPnShareByID(apiKey, videoID string) (*upnshareManageVideo, error) {
	req, e := http.NewRequest("GET", fmt.Sprintf("https://upnshare.com/api/v1/video/manage/%s", videoID), nil)
	if e != nil {
		return nil, fmt.Errorf("create request: %w", e)
	}
	req.Header.Set("api-token", apiKey)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, e := mediaFetchClient.Do(req)
	if e != nil {
		return nil, fmt.Errorf("request: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var detail upnshareManageVideo
	if e := json.NewDecoder(resp.Body).Decode(&detail); e != nil {
		return nil, fmt.Errorf("decode: %w", e)
	}
	if detail.ID == "" {
		return nil, fmt.Errorf("empty response")
	}
	return &detail, nil
}

// buildUPnShareURLs assembles the absolute poster/preview URLs from a manage
// video record.
func buildUPnShareURLs(v *upnshareManageVideo) (posterURL, previewURL string) {
	if v == nil || v.AssetURL == "" {
		return "", ""
	}
	if v.Poster != "" {
		posterURL = v.AssetURL + v.Poster
	}
	if v.Preview != "" {
		previewURL = v.AssetURL + v.Preview
	}
	return posterURL, previewURL
}

// matchUPnShareVideo picks the best manage-list match for our recording.
// Priority: exact name > name contains the timestamp token > name starts with
// the recording stem (suffix-stripped) > matches the stored video ID.
func matchUPnShareVideo(list []upnshareManageVideo, filename, token, videoID string) *upnshareManageVideo {
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	stem = strings.TrimPrefix(stem, "merged-")

	var fallback *upnshareManageVideo
	for i := range list {
		v := &list[i]
		switch {
		case v.Name == filename:
			return v
		case token != "" && strings.Contains(v.Name, token):
			return v
		case strings.HasPrefix(v.Name, stem):
			if fallback == nil {
				fallback = v
			}
		case v.ID == videoID && fallback == nil:
			fallback = v
		}
	}
	return fallback
}

// upnshareUsername extracts the chaturbate username from a recording filename
// of the form "username_YYYY-MM-DD_HH-MM-SS[.suffix].mp4". It mirrors the
// parsing in channel/channel_file.go so the watcher and the recorder agree on
// where the username ends (handling usernames that contain "_20" or hyphens).
func upnshareUsername(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	stem := strings.TrimPrefix(base, "merged-")
	idx := upnshareDateRe.FindStringSubmatchIndex(stem)
	if idx == nil {
		return ""
	}
	candidate := stem[:idx[0]] // index of the leading "_" before the date

	// Deduplicate merged "<user>-<user>" usernames.
	searchFrom := 0
	for {
		hyphen := strings.Index(candidate[searchFrom:], "-")
		if hyphen < 0 {
			break
		}
		hyphen += searchFrom
		if candidate[:hyphen] == candidate[hyphen+1:] {
			return candidate[:hyphen]
		}
		searchFrom = hyphen + 1
	}
	return candidate
}

// upnshareTimestampToken returns the unique "YYYY-MM-DD_HH-MM-SS" portion of a
// recording filename, used to pinpoint the exact video among a user's list.
func upnshareTimestampToken(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	m := upnshareDateTokenRe.FindString(base)
	return m
}

var (
	// Matches the leading "_" of the "_YYYY-MM-DD_" timestamp separator.
	upnshareDateRe = regexp.MustCompile(`_(20\d{2}-\d{2}-\d{2})[_-]`)
	// Matches the full "YYYY-MM-DD_HH-MM-SS" timestamp token.
	upnshareDateTokenRe = regexp.MustCompile(`20\d{2}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}`)
)

func searchUPnShareByName(search, apiKey string) ([]upnshareManageVideo, error) {
	reqURL := fmt.Sprintf("https://upnshare.com/api/v1/video/manage?search=%s&perPage=10", url.QueryEscape(search))
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("api-token", apiKey)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := mediaFetchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var listResp upnshareManageListResp
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return listResp.Data, nil
}

// ─── SeekStreaming media (poster + preview) fetch ────────────────────────────

// GetSeekStreamingMediaURLs fetches both the poster and preview URLs for a
// SeekStreaming video. It mirrors the recorder: search the manage list by the
// recording's full filename and match on exact name (the embed URL's #fragment
// is NOT a reliable manage id, so by-ID lookups 404).
func GetSeekStreamingMediaURLs(key, videoID, filename string) (posterURL, previewURL string, err error) {
	if filename == "" {
		return "", "", fmt.Errorf("empty filename for seekstreaming lookup")
	}
	reqURL := fmt.Sprintf("https://seekstreaming.com/api/v1/video/manage?search=%s&perPage=5", url.QueryEscape(filename))
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("api-token", key)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := mediaFetchClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	_body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status %d — %s", resp.StatusCode, strings.TrimSpace(string(_body)))
	}

	var listResp struct {
		Data []struct {
			Name     string `json:"name"`
			Poster   string `json:"poster"`
			Preview  string `json:"preview"`
			AssetURL string `json:"assetUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(_body, &listResp); err != nil {
		return "", "", fmt.Errorf("decode: %w (body: %s)", err, string(_body))
	}

	for _, v := range listResp.Data {
		if v.Name == filename {
			if v.Poster != "" && v.AssetURL != "" {
				posterURL = v.AssetURL + v.Poster
			}
			if v.Preview != "" && v.AssetURL != "" {
				previewURL = v.AssetURL + v.Preview
			}
			return posterURL, previewURL, nil
		}
	}

	return "", "", fmt.Errorf("SeekStreaming media not available yet for %s", filename)
}

// MediaHostOf resolves which video host a recording's embed URL points at and
// returns the host tag plus the video ID (the fragment after '#').
//
//	SeekStreaming -> https://chuglii.seeks.cloud/#<id>  (or *.seekstreaming.info)
//	UPnShare      -> https://<prefix>.upns.online/#<id>
func MediaHostOf(embedURL string) (host, videoID string) {
	if idx := strings.LastIndex(embedURL, "#"); idx >= 0 {
		videoID = embedURL[idx+1:]
	}
	hostPart := embedURL
	if idx := strings.LastIndex(embedURL, "#"); idx >= 0 {
		hostPart = embedURL[:idx]
	}
	hostPart = strings.ToLower(hostPart)

	switch {
	case strings.Contains(hostPart, "seekstreaming") || strings.Contains(hostPart, "seeks.cloud"):
		return "seekstreaming", videoID
	case strings.Contains(hostPart, "upns") || strings.Contains(hostPart, "upnshare"):
		// Covers *.upns.online and upnshare.com player domains.
		return "upnshare", videoID
	}
	return "", videoID
}
