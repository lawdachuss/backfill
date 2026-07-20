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
// The embed URL stores UPnShare's *player* ID, which is NOT the same as the
// manage-API video id, so a direct by-ID lookup 404s. The reliable way to find
// the video is to search the manage list by the recording's username and then
// pick the entry whose name contains this recording's exact timestamp token
// (the unique YYYY-MM-DD_HH-MM-SS portion). We fall back to a prefix/ID match
// only if the token isn't present.
func GetUPnShareMediaURLs(apiKey, videoID, filename string) (posterURL, previewURL string, err error) {
	fetchByID := func() (*upnshareManageVideo, error) {
		if videoID == "" {
			return nil, fmt.Errorf("empty video ID")
		}
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

	// Fast path: the by-ID endpoint occasionally works (e.g. when the player ID
	// happens to equal the manage id).
	if detail, e := fetchByID(); e == nil {
		p, pr := buildUPnShareURLs(detail)
		return p, pr, nil
	}

	// Otherwise resolve via the username search + timestamp-token match.
	if filename != "" {
		user := upnshareUsername(filename)
		token := upnshareTimestampToken(filename)
		if list, se := searchUPnShareByName(user, apiKey); se == nil {
			if v := matchUPnShareVideo(list, filename, token, videoID); v != nil {
				p, pr := buildUPnShareURLs(v)
				return p, pr, nil
			}
		}
	}

	return "", "", fmt.Errorf("UPnShare media not available yet for %s", videoID)
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
// SeekStreaming video in a single API call.
func GetSeekStreamingMediaURLs(key, videoID string) (posterURL, previewURL string, err error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://seekstreaming.com/api/v1/video/manage/%s", videoID), nil)
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

	var detail struct {
		Poster   string `json:"poster"`
		Preview  string `json:"preview"`
		AssetURL string `json:"assetUrl"`
	}
	if err := json.Unmarshal(_body, &detail); err != nil {
		return "", "", fmt.Errorf("decode: %w (body: %s)", err, string(_body))
	}

	if detail.Poster != "" && detail.AssetURL != "" {
		posterURL = detail.AssetURL + detail.Poster
	}
	if detail.Preview != "" && detail.AssetURL != "" {
		previewURL = detail.AssetURL + detail.Preview
	}
	return posterURL, previewURL, nil
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
