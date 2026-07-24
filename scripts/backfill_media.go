//go:build ignore

// backfill_media.go — backfills missing thumbnail and preview URLs for all
// recordings in the Supabase database.
//
// Phase 1 (host API):  For recordings with embed_url on SeekStreaming or
//   UPnShare, query the host's manage API for poster (thumbnail) and preview
//   URLs — no video download required. Also checks ALL upload_links for
//   SeekStreaming/UPnShare links when embed_url doesn't resolve.
// Phase 2 (FFmpeg):    For recordings that still need assets and have upload
//   links on hosts without a poster/preview API (GoFile, Streamtape, etc.),
//   resolve a direct download URL and use FFmpeg HTTP range requests to
//   generate the thumbnail and preview without downloading the full video.
//
// Usage:
//   go run scripts/backfill_media.go [flags]
//
// Flags:
//   -dry-run        Print what would be done without writing to DB
//   -concurrency N  Number of concurrent workers (default 1)
//   -limit N        Stop after processing N recordings (0 = unlimited)
//   -duration       Max duration to run before exiting (e.g. 5h45m)
//   -delay          Delay between consecutive backfills (e.g. 5m)
//   -thumb-only     Only backfill thumbnails (no preview fetch)
//   -trigger-workflow  Trigger a new workflow run on exit if duration exceeded
//   -no-ffmpeg      Skip the FFmpeg fallback (host API only)

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teacat/chaturbate-dvr/config"
	"github.com/teacat/chaturbate-dvr/database"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// ─── flags ───────────────────────────────────────────────────────────────────

var (
	flagDryRun       = flag.Bool("dry-run", false, "Print plan without writing to DB")
	flagConcurrency  = flag.Int("concurrency", 1, "Concurrent workers (hosts are rate-limited; 1 is safest)")
	flagLimit        = flag.Int("limit", 0, "Max recordings to process (0 = unlimited)")
	flagDuration     = flag.String("duration", "", "Max duration to run before exiting (e.g. 5h45m)")
	flagDelay        = flag.String("delay", "", "Delay between consecutive video backfills (e.g. 5m)")
	flagThumbOnly    = flag.Bool("thumb-only", false, "Only backfill thumbnails (no preview fetch)")
	flagTrigger      = flag.Bool("trigger-workflow", false, "Trigger a new workflow run on exit if duration exceeded")
	flagShard        = flag.Int("shard", 0, "Zero-based shard index when splitting work across a matrix (0..shards-1)")
	flagShards       = flag.Int("shards", 1, "Total number of shards (1 = process everything in one job)")
	flagNoCache      = flag.Bool("no-cache", false, "Ignore the memorized cache and re-query every recording")
	flagNoFFmpeg     = flag.Bool("no-ffmpeg", false, "Skip the FFmpeg fallback (host API only)")
)

// ─── counters ─────────────────────────────────────────────────────────────────

var (
	cntTotal        int64
	cntThumb        int64
	cntPreview      int64
	cntSkipped      int64
	cntFailed       int64
	cntNotReady     int64
	cntCacheHit     int64
	cntFFmpegThumb  int64
	cntFFmpegPrev   int64
	cntNoSource     int64
)

// ─── memorized cache ──────────────────────────────────────────────────────────

const cacheFile = "backfill_cache.json"

type cacheEntry struct {
	Thumbnail string `json:"thumbnail"`
	Preview   string `json:"preview"`
	NotAvail  bool   `json:"not_avail,omitempty"`
}

var cacheMu sync.Mutex
var cacheMap map[string]cacheEntry

func loadCache() {
	cacheMap = map[string]cacheEntry{}
	f, err := os.Open(cacheFile)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewDecoder(f).Decode(&cacheMap)
}

func initCache() {
	if cacheMap == nil {
		cacheMap = map[string]cacheEntry{}
	}
}

func saveCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	f, err := os.Create(cacheFile)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(cacheMap)
}

// ─── .env loader ─────────────────────────────────────────────────────────────

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.Trim(strings.TrimSpace(parts[1]), `'\"`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func logf(format string, a ...interface{}) {
	log.Printf("[backfill] "+format, a...)
}

func errorf(format string, a ...interface{}) {
	log.Printf("[backfill:ERR] "+format, a...)
}

// ─── Upload links map ─────────────────────────────────────────────────────────

// loadAllUploadLinks fetches ALL upload_links and groups them by recording_id.
// Used to find SeekStreaming/UPnShare links when embed_url doesn't resolve.
func loadAllUploadLinks(client *database.Client) map[string][]database.UploadLink {
	logf("Pre-fetching all upload links for FFmpeg fallback…")
	links, err := client.GetAllUploadLinks()
	if err != nil {
		errorf("GetAllUploadLinks: %v", err)
		return nil
	}
	byRec := make(map[string][]database.UploadLink, len(links))
	for _, l := range links {
		byRec[l.RecordingID] = append(byRec[l.RecordingID], l)
	}
	logf("Total upload links: %d, linked recordings: %d", len(links), len(byRec))
	return byRec
}

// ─── GoFile download URL resolution ──────────────────────────────────────────

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ─── GoFile circuit breaker ─────────────────────────────────────────────────

// gofileBreakerMax failures before the circuit trips and skips further GoFile
// resolution. This prevents wasting 60s per file when GoFile is unreachable.
const gofileBreakerMax = 3

var gofileBreaker struct {
	mu       sync.Mutex
	failures int
	tripped  bool
}

func gofileBreakerIsTripped() bool {
	gofileBreaker.mu.Lock()
	defer gofileBreaker.mu.Unlock()
	return gofileBreaker.tripped
}

func gofileBreakerRecordFailure() {
	gofileBreaker.mu.Lock()
	defer gofileBreaker.mu.Unlock()
	gofileBreaker.failures++
	if gofileBreaker.failures >= gofileBreakerMax && !gofileBreaker.tripped {
		gofileBreaker.tripped = true
		logf("  ⚡ GoFile circuit breaker tripped after %d consecutive failures — skipping remaining GoFile resolutions", gofileBreaker.failures)
	}
}

func gofileBreakerRecordSuccess() {
	gofileBreaker.mu.Lock()
	defer gofileBreaker.mu.Unlock()
	gofileBreaker.failures = 0
	if gofileBreaker.tripped {
		gofileBreaker.tripped = false
		logf("  ✓ GoFile circuit breaker reset — GoFile seems to be back online")
	}
}

// ─── Streamtape circuit breaker ──────────────────────────────────────────────

// streamtapeBreakerMax failures before the circuit trips and skips further
// Streamtape resolution. This prevents wasting time on every file when
// Streamtape is consistently rate-limiting or unreachable.
const streamtapeBreakerMax = 3

var streamtapeBreaker struct {
	mu       sync.Mutex
	failures int
	tripped  bool
}

func streamtapeBreakerIsTripped() bool {
	streamtapeBreaker.mu.Lock()
	defer streamtapeBreaker.mu.Unlock()
	return streamtapeBreaker.tripped
}

func streamtapeBreakerRecordFailure() {
	streamtapeBreaker.mu.Lock()
	defer streamtapeBreaker.mu.Unlock()
	streamtapeBreaker.failures++
	if streamtapeBreaker.failures >= streamtapeBreakerMax && !streamtapeBreaker.tripped {
		streamtapeBreaker.tripped = true
		logf("  ⚡ Streamtape circuit breaker tripped after %d consecutive failures — skipping remaining Streamtape resolutions", streamtapeBreaker.failures)
	}
}

func streamtapeBreakerRecordSuccess() {
	streamtapeBreaker.mu.Lock()
	defer streamtapeBreaker.mu.Unlock()
	streamtapeBreaker.failures = 0
	if streamtapeBreaker.tripped {
		streamtapeBreaker.tripped = false
		logf("  ✓ Streamtape circuit breaker reset — Streamtape seems to be back online")
	}
}

// extractGoFileCode extracts the file/folder code from a GoFile URL.
// Works with: https://gofile.io/d/{code}, https://gofile.io/w/{code}
func extractGoFileCode(rawURL string) string {
	// Try direct link format first: https://gofile.io/d/CODE
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	if len(parts) > 0 {
		code := parts[len(parts)-1]
		if len(code) >= 4 && !strings.Contains(code, ".") {
			return code
		}
	}
	return ""
}

// getGoFileGuestToken obtains a guest account token from GoFile.
// Used to access the contents API for resolving file download URLs.
func getGoFileGuestToken() (string, error) {
	resp, err := httpClient.Post("https://api.gofile.io/accounts", "application/json",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("create account status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Token          string `json:"token"`
			AccountType    string `json:"accountType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode account: %w", err)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("create account status: %s", result.Status)
	}
	return result.Data.Token, nil
}

// getGoFileDirectURL resolves a GoFile share URL to a direct download link.
// Uses the GoFile contents API with guest authentication.
// Checks the circuit breaker first — if tripped, returns immediately without
// making any API calls to avoid wasting time when GoFile is unreachable.
func getGoFileDirectURL(shareURL string) (string, int64, error) {
	if gofileBreakerIsTripped() {
		return "", 0, fmt.Errorf("GoFile circuit breaker tripped — skipping")
	}

	code := extractGoFileCode(shareURL)
	if code == "" {
		return "", 0, fmt.Errorf("cannot extract GoFile code from %s", shareURL)
	}

	token, err := getGoFileGuestToken()
	if err != nil {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("guest token: %w", err)
	}

	req, err := http.NewRequest("GET",
		fmt.Sprintf("https://api.gofile.io/contents/%s?cache=true", code), nil)
	if err != nil {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("contents request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("contents status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			Children map[string]struct {
				Link string `json:"link"`
				Size int64  `json:"size"`
				Mime string `json:"mimetype"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("decode contents: %w", err)
	}
	if result.Status != "ok" {
		gofileBreakerRecordFailure()
		return "", 0, fmt.Errorf("contents status: %s", result.Status)
	}

	for _, child := range result.Data.Children {
		if child.Link != "" && strings.HasPrefix(child.Mime, "video/") {
			gofileBreakerRecordSuccess()
			return child.Link, child.Size, nil
		}
	}
	for _, child := range result.Data.Children {
		if child.Link != "" {
			gofileBreakerRecordSuccess()
			return child.Link, child.Size, nil
		}
	}

	gofileBreakerRecordFailure()
	return "", 0, fmt.Errorf("no downloadable link found in GoFile contents")
}

// ─── Streamtape download URL resolution ────────────────────────────────────

// extractStreamtapeID extracts the file ID from a Streamtape embed URL.
// Works with: https://streamtape.com/e/{id}/, https://streamtape.com/v/{id}/...
// The file ID is the path segment that immediately follows "/e/" or "/v/".
func extractStreamtapeID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	for i, p := range parts {
		if (p == "e" || p == "v") && i+1 < len(parts) {
			candidate := parts[i+1]
			if !strings.ContainsAny(candidate, ".") {
				return candidate
			}
		}
	}
	return ""
}

// streamtapeRetryMaxAttempts is the maximum number of attempts for Streamtape
// resolution when rate-limited with a 403.
const streamtapeRetryMaxAttempts = 5

// streamtapeWaitDuration extracts the required wait duration from a Streamtape
// 403 error message like "You need to wait 56 more seconds".
// Returns 0 if the message doesn't match the expected format.
func streamtapeWaitDuration(errMsg string) time.Duration {
	var secs int
	if n, _ := fmt.Sscanf(errMsg, "You need to wait %d more seconds", &secs); n == 1 && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// getStreamtapeDirectURL resolves a Streamtape embed URL to a direct download
// link using the Streamtape API's dlticket + dl endpoints.
// Retries with backoff on 403 rate-limit errors.
// Requires valid API login and key credentials.
func getStreamtapeDirectURL(shareURL, login, key string) (string, int64, error) {
	if streamtapeBreakerIsTripped() {
		return "", 0, fmt.Errorf("Streamtape circuit breaker tripped — skipping")
	}

	fileID := extractStreamtapeID(shareURL)
	if fileID == "" {
		return "", 0, fmt.Errorf("cannot extract Streamtape file ID from %s", shareURL)
	}

	var lastErr error
	for attempt := 0; attempt < streamtapeRetryMaxAttempts; attempt++ {
		if attempt > 0 {
			// Use the wait duration from the Streamtape 403 message if available,
			// otherwise fall back to exponential backoff.
			wait := streamtapeWaitDuration(lastErr.Error())
			if wait == 0 {
				wait = time.Duration(5<<uint(attempt-1)) * time.Second // 5s, 10s, 20s, 40s
			} else {
				wait += 1 * time.Second // small buffer
			}
			logf("  ⏳ Streamtape rate-limited, waiting %v before retry (attempt %d/%d)...", wait, attempt+1, streamtapeRetryMaxAttempts)
			time.Sleep(wait)
		}

		// Step 1: Get download ticket
		dlTicketURL := fmt.Sprintf("https://api.streamtape.com/file/dlticket?file=%s&login=%s&key=%s",
			fileID, url.QueryEscape(login), url.QueryEscape(key))

		req, err := http.NewRequest("GET", dlTicketURL, nil)
		if err != nil {
			return "", 0, fmt.Errorf("create dlticket request: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", 0, fmt.Errorf("dlticket request: %w", err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return "", 0, fmt.Errorf("dlticket status %d: %s", resp.StatusCode, string(body))
		}

		var ticketResp struct {
			Status int    `json:"status"`
			Msg    string `json:"msg"`
			Result struct {
				Ticket   string `json:"ticket"`
				WaitTime int    `json:"wait_time"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &ticketResp); err != nil {
			return "", 0, fmt.Errorf("decode dlticket response: %w", err)
		}
		if ticketResp.Status != 200 {
			if ticketResp.Status == 403 {
				lastErr = fmt.Errorf("dlticket API error %d: %s", ticketResp.Status, ticketResp.Msg)
				continue
			}
			return "", 0, fmt.Errorf("dlticket API error %d: %s", ticketResp.Status, ticketResp.Msg)
		}
		if ticketResp.Result.Ticket == "" {
			return "", 0, fmt.Errorf("empty ticket in dlticket response")
		}

		// Respect the mandatory cooldown before calling dl
		if ticketResp.Result.WaitTime > 0 {
			logf("  ⏳ Streamtape requires %ds wait before download...", ticketResp.Result.WaitTime)
			time.Sleep(time.Duration(ticketResp.Result.WaitTime) * time.Second)
		}

		// Step 2: Get direct download URL using the ticket
		dlURL := fmt.Sprintf("https://api.streamtape.com/file/dl?file=%s&ticket=%s",
			fileID, url.QueryEscape(ticketResp.Result.Ticket))

		req2, err := http.NewRequest("GET", dlURL, nil)
		if err != nil {
			return "", 0, fmt.Errorf("create dl request: %w", err)
		}
		req2.Header.Set("User-Agent", "Mozilla/5.0")

		resp2, err := httpClient.Do(req2)
		if err != nil {
			return "", 0, fmt.Errorf("dl request: %w", err)
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		if resp2.StatusCode != 200 {
			return "", 0, fmt.Errorf("dl status %d: %s", resp2.StatusCode, string(body2))
		}

		var dlResp struct {
			Status int    `json:"status"`
			Msg    string `json:"msg"`
			Result struct {
				URL string `json:"url"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body2, &dlResp); err != nil {
			return "", 0, fmt.Errorf("decode dl response: %w", err)
		}
		if dlResp.Status != 200 {
			if dlResp.Status == 403 {
				lastErr = fmt.Errorf("dl API error %d: %s", dlResp.Status, dlResp.Msg)
				continue
			}
			return "", 0, fmt.Errorf("dl API error %d: %s", dlResp.Status, dlResp.Msg)
		}
		if dlResp.Result.URL == "" {
			return "", 0, fmt.Errorf("empty URL in dl response")
		}

		// Verify the resolved URL is accessible before returning it.
		// Streamtape direct URLs are often short-lived or IP-locked.
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		verifyReq, verifyErr := http.NewRequestWithContext(verifyCtx, "HEAD", dlResp.Result.URL, nil)
		if verifyErr == nil {
			verifyReq.Header.Set("User-Agent", "Mozilla/5.0")
			verifyResp, verifyErr := httpClient.Do(verifyReq)
			verifyCancel()
			if verifyErr == nil {
				verifyResp.Body.Close()
				if verifyResp.StatusCode != 200 {
					logf("  ⚠ Streamtape URL returned HTTP %d, retrying...", verifyResp.StatusCode)
					lastErr = fmt.Errorf("Streamtape URL verification failed: HTTP %d", verifyResp.StatusCode)
					continue
				}
			}
		} else {
			verifyCancel()
		}

		// Success — reset the circuit breaker
		streamtapeBreakerRecordSuccess()

		return dlResp.Result.URL, 0, nil
	}

	// All retries exhausted — record failure for circuit breaker
	streamtapeBreakerRecordFailure()
	return "", 0, fmt.Errorf("Streamtape resolution failed after %d attempts: %v", streamtapeRetryMaxAttempts, lastErr)
}

// defaultFFmpegUserAgent is the User-Agent sent by FFmpeg when downloading
// video URLs. Streamtape and other CDNs may block FFmpeg's default UA.
const defaultFFmpegUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36"

// ─── FFmpeg helpers ──────────────────────────────────────────────────────────

func ffmpegBinPath() string {
	if p := os.Getenv("FFMPEG_PATH"); p != "" {
		return p
	}
	return "ffmpeg"
}

// ffmpegRunLocal runs ffmpeg with the given arguments and waits for completion.
func ffmpegRunLocal(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, ffmpegBinPath(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg %v: %s (%v)", args, string(output), err)
	}
	return nil
}

// ─── Thumbnail generation from URL (FFmpeg HTTP range) ───────────────────────

// urlGenThumbnail extracts a single frame from a video URL at 15% duration,
// scales it to 1280x720, and writes a JPEG thumbnail to the given output path.
func urlGenThumbnail(videoURL string, dur float64, tmpDir, filename string) (string, error) {
	if dur <= 0 {
		dur = 30 // default assumption if unknown
	}
	seekSec := dur * 0.15
	thumbFile := filepath.Join(tmpDir, filename+".thumb.jpg")

	args := []string{
		"-user_agent", defaultFFmpegUserAgent,
		"-ss", fmt.Sprintf("%.1f", seekSec),
		"-i", videoURL,
		"-vframes", "1",
		"-q:v", "3",
		"-vf", "scale='min(1280,iw)':'min(720,ih)':force_original_aspect_ratio=decrease",
		"-y", thumbFile,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := ffmpegRunLocal(ctx, args...); err != nil {
		return "", fmt.Errorf("generate thumbnail: %w", err)
	}
	return thumbFile, nil
}

// urlGenPreview generates a short WebP preview from a video URL.
// Extracts 8 short clips at regular intervals and concatenates them.
func urlGenPreview(videoURL string, dur float64, tmpDir, filename string) (string, error) {
	if dur <= 0 {
		dur = 30
	}
	previewFile := filepath.Join(tmpDir, filename+".preview.webp")

	// Sample 8 clips of 1 second each spaced evenly across the video
	interval := dur / 9 // slightly less than 1/8 to avoid going past the end
	startSec := dur * 0.05 // start at 5% to skip intro
	clipDir := filepath.Join(tmpDir, filename+"_clips")
	if err := os.MkdirAll(clipDir, 0755); err != nil {
		return "", fmt.Errorf("create clip dir: %w", err)
	}
	defer os.RemoveAll(clipDir)

	// First collect clip count — estimate how many full clips we can fit
	clipCount := 8
	if dur < 16 {
		clipCount = int(dur / 2)
		if clipCount < 2 {
			clipCount = 2
		}
	}
	interval = (dur * 0.9) / float64(clipCount+1)

	var clipFiles []string
	for i := 0; i < clipCount; i++ {
		seek := startSec + interval*float64(i)
		clipFile := filepath.Join(clipDir, fmt.Sprintf("clip%d.webp", i))
		args := []string{
			"-user_agent", defaultFFmpegUserAgent,
			"-ss", fmt.Sprintf("%.1f", seek),
			"-i", videoURL,
			"-t", "1",
			"-vf", "fps=10,scale='min(320,iw)':'min(180,ih)':force_original_aspect_ratio=decrease",
			"-loop", "0",
			"-y", clipFile,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := ffmpegRunLocal(ctx, args...)
		cancel()
		if err != nil {
			continue
		}
		if st, err := os.Stat(clipFile); err == nil && st.Size() > 100 {
			clipFiles = append(clipFiles, clipFile)
		}
	}

	if len(clipFiles) == 0 {
		return "", fmt.Errorf("no preview clips could be generated")
	}

	// Concatenate clips into single preview
	var filterParts []string
	for _, f := range clipFiles {
		filterParts = append(filterParts, fmt.Sprintf("file '%s'", strings.ReplaceAll(f, "'", "'\\''")))
	}
	concatFile := filepath.Join(tmpDir, filename+"_concat.txt")
	if err := os.WriteFile(concatFile, []byte(strings.Join(filterParts, "\n")), 0644); err != nil {
		return "", fmt.Errorf("write concat file: %w", err)
	}
	defer os.Remove(concatFile)

	concatArgs := []string{
		"-f", "concat",
		"-safe", "0",
		"-i", concatFile,
		"-c:v", "libwebp",
		"-lossless", "0",
		"-q:v", "60",
		"-y", previewFile,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := ffmpegRunLocal(ctx, concatArgs...); err != nil {
		return "", fmt.Errorf("concat preview: %w", err)
	}
	return previewFile, nil
}

// generateMediaFromURL generates thumbnail and/or preview from a CDN URL.
// Returns file paths for the generated assets AND the temp directory path.
// Caller must clean up the temp directory after uploading the files.
func generateMediaFromURL(cdnURL, filename string, fileSize int64, needThumb, needPreview bool) (thumbPath, previewPath, tmpDir string, err error) {
	tmpDir, err = os.MkdirTemp("", "backfill-*")
	if err != nil {
		return "", "", "", fmt.Errorf("create temp dir: %w", err)
	}

	// Probe duration via ffprobe
	dur := probeDuration(cdnURL)
	if dur <= 0 && fileSize > 0 {
		// Estimate: assume ~5 Mbps average bitrate
		dur = float64(fileSize) * 8 / (5 * 1024 * 1024)
	}
	if dur <= 0 {
		dur = 30 // fallback
	}
	logf("  duration=%.1fs size=%d", dur, fileSize)

	var wg sync.WaitGroup
	var thumbErr, prevErr error

	if needThumb {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tp, err := urlGenThumbnail(cdnURL, dur, tmpDir, filename)
			if err != nil {
				thumbErr = fmt.Errorf("thumbnail: %w", err)
				return
			}
			thumbPath = tp
		}()
	}

	if needPreview {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pp, err := urlGenPreview(cdnURL, dur, tmpDir, filename)
			if err != nil {
				prevErr = fmt.Errorf("preview: %w", err)
				return
			}
			previewPath = pp
		}()
	}

	wg.Wait()

	if thumbErr != nil {
		return thumbPath, previewPath, tmpDir, thumbErr
	}
	if prevErr != nil {
		return thumbPath, previewPath, tmpDir, prevErr
	}
	if thumbPath == "" && previewPath == "" {
		return "", "", tmpDir, fmt.Errorf("no media generated")
	}
	return thumbPath, previewPath, tmpDir, nil
}

// ffprobeBinPath returns the path to ffprobe.
func ffprobeBinPath() string {
	if p := os.Getenv("FFPROBE_PATH"); p != "" {
		return p
	}
	// Same directory as ffmpeg
	if ff := ffmpegBinPath(); ff != "ffmpeg" {
		if dir := filepath.Dir(ff); dir != "." {
			probe := filepath.Join(dir, "ffprobe")
			if _, err := os.Stat(probe); err == nil {
				return probe
			}
			probe += ".exe"
			if _, err := os.Stat(probe); err == nil {
				return probe
			}
		}
	}
	return "ffprobe"
}

// probeDuration uses ffprobe to get the duration of a remote video URL.
func probeDuration(videoURL string) float64 {
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		"-user_agent", defaultFFmpegUserAgent,
		videoURL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobeBinPath(), args...)
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	var dur float64
	if _, err := fmt.Sscanf(string(output), "%f", &dur); err != nil {
		return 0
	}
	return dur
}

// ─── Upload helpers ──────────────────────────────────────────────────────────

// uploadGeneratedMedia uploads a generated thumbnail and/or preview file.
// Thumbnails go to Pixhost (via ThumbnailUploader), previews go to Catbox.
func uploadGeneratedMedia(thumbPath, previewPath string) (thumbURL, previewURL string, err error) {
	if thumbPath != "" {
		thumbUp := uploader.NewThumbnailUploader(os.Getenv("IMGBB_API_KEY"))
		url, upErr := thumbUp.Upload(thumbPath)
		if upErr != nil {
			// Fallback: try Catbox
			cb := uploader.NewCatboxUploader()
			url, upErr = cb.Upload(thumbPath)
			if upErr != nil {
				return "", "", fmt.Errorf("upload thumbnail: %w", upErr)
			}
		}
		thumbURL = url
		logf("  ✓ thumbnail uploaded: %s", thumbURL)
	}

	if previewPath != "" {
		cb := uploader.NewCatboxUploader()
		url, upErr := cb.Upload(previewPath)
		if upErr != nil {
			return thumbURL, "", fmt.Errorf("upload preview: %w", upErr)
		}
		previewURL = url
		logf("  ✓ preview uploaded: %s", previewURL)
	}

	return thumbURL, previewURL, nil
}

// ─── host media fetch ─────────────────────────────────────────────────────────

func fetchHostMedia(seekKey string, upnKeys []string, host, videoID, filename string) (thumb, prev string, err error) {
	switch host {
	case "seekstreaming":
		if seekKey == "" {
			return "", "", fmt.Errorf("SeekStreaming key not configured")
		}
		return uploader.GetSeekStreamingMediaURLs(seekKey, videoID, filename)
	case "upnshare":
		if len(upnKeys) == 0 {
			return "", "", fmt.Errorf("UPnShare key not configured")
		}
		return uploader.GetUPnShareMediaURLs(upnKeys, videoID, filename)
	default:
		return "", "", fmt.Errorf("unknown host %q", host)
	}
}

// ─── Media source resolution from upload links ───────────────────────────────

// resolveMediaSource iterates through a recording's upload links to find a
// downloadable source URL. Tries hosts in priority order:
//   1. GoFile (with circuit breaker — auto-skips after N consecutive failures)
//   2. Streamtape (if credentials are provided)
//   3. Any non-GoFile/non-Streamtape URL directly (works for PixelDrain, Catbox)
// Returns the direct CDN URL and file size.
func resolveMediaSource(links []database.UploadLink, streamtapeLogin, streamtapeKey string) (string, int64, error) {
	// Priority 1: GoFile (unless circuit breaker is tripped)
	if !gofileBreakerIsTripped() {
		for _, link := range links {
			h := strings.ToLower(link.Host)
			u := strings.ToLower(link.URL)

			if strings.Contains(h, "gofile") || strings.Contains(u, "gofile.io") {
				directURL, size, err := getGoFileDirectURL(link.URL)
				if err != nil {
					logf("  GoFile resolution failed for %s: %v", link.URL, err)
					continue
				}
				return directURL, size, nil
			}
		}
	}

	// Priority 2: Streamtape (if credentials available and circuit not tripped)
	if streamtapeBreakerIsTripped() {
		logf("  Streamtape circuit breaker is tripped — skipping Streamtape resolution")
	} else if streamtapeLogin != "" && streamtapeKey != "" {
		for _, link := range links {
			h := strings.ToLower(link.Host)
			u := strings.ToLower(link.URL)

			if strings.Contains(h, "streamtape") || strings.Contains(u, "streamtape.com") {
				directURL, size, err := getStreamtapeDirectURL(link.URL, streamtapeLogin, streamtapeKey)
				if err != nil {
					logf("  Streamtape resolution failed for %s: %v", link.URL, err)
					continue
				}
				return directURL, size, nil
			}
		}
	}

	// Priority 3: try any link URL directly (works for PixelDrain, Catbox, LobFile, etc.)
	for _, link := range links {
		if strings.HasPrefix(link.URL, "http") &&
			!strings.Contains(link.URL, "gofile.io") &&
			!strings.Contains(link.URL, "streamtape.com") {
			return link.URL, 0, nil
		}
	}

	return "", 0, fmt.Errorf("no resolvable media source found")
}

// ─── worker ──────────────────────────────────────────────────────────────────

type workItem struct {
	rec         database.Recording
	host        string
	videoID     string
	uploadLinks []database.UploadLink // for FFmpeg fallback
}

// processOne handles a single recording. Returns true if any work was done.
func processOne(item workItem, seekKey string, upnKeys []string, streamtapeLogin, streamtapeKey string, dryRun, thumbOnly bool) bool {
	rec := item.rec
	atomic.AddInt64(&cntTotal, 1)

	needThumb := rec.ThumbnailURL == ""
	needPreview := rec.PreviewURL == ""

	if !needThumb && !needPreview {
		atomic.AddInt64(&cntSkipped, 1)
		return false
	}

	// ── Memorized cache ──────────────────────────────────────────────────────
	cacheMu.Lock()
	cached, inCache := cacheMap[rec.Filename]
	cacheMu.Unlock()
	if inCache {
		atomic.AddInt64(&cntCacheHit, 1)
		if cached.NotAvail {
			atomic.AddInt64(&cntSkipped, 1)
			return false
		}
		if needThumb && cached.Thumbnail != "" {
			rec.ThumbnailURL = cached.Thumbnail
			needThumb = false
		}
		if needPreview && cached.Preview != "" {
			rec.PreviewURL = cached.Preview
			needPreview = false
		}
		if !needThumb && !needPreview {
			atomic.AddInt64(&cntSkipped, 1)
			return false
		}
		if cached.Thumbnail != "" {
			rec.ThumbnailURL = cached.Thumbnail
		}
		if cached.Preview != "" {
			rec.PreviewURL = cached.Preview
		}
	}

	logf("%-60s  [thumb=%v preview=%v]  host=%s",
		rec.Filename, needThumb, needPreview, item.host)

	thumb := rec.ThumbnailURL
	preview := rec.PreviewURL

	// ── Phase 1: Try host API ────────────────────────────────────────────────
	if item.host != "" {
		hosts := []string{item.host}
		if item.host != "seekstreaming" && item.host != "upnshare" {
			hosts = append(hosts, "seekstreaming", "upnshare")
		}
		for _, host := range hosts {
			t, p, err := fetchHostMedia(seekKey, upnKeys, host, item.videoID, rec.Filename)
			if err != nil {
				logf("  %s API: %v", host, err)
				continue
			}
			if needThumb && t != "" {
				thumb = t
				needThumb = false
				atomic.AddInt64(&cntThumb, 1)
				logf("  ✓ thumb via %s: %s", host, t)
			}
			if !thumbOnly && needPreview && p != "" {
				preview = p
				needPreview = false
				atomic.AddInt64(&cntPreview, 1)
				logf("  ✓ preview via %s: %s", host, p)
			}
			if !needThumb && (!thumbOnly || !needPreview) {
				break
			}
		}
	}

	// ── Phase 2: FFmpeg fallback (only when no host API is available) ──────
	if !*flagNoFFmpeg && item.host == "" && (needThumb || (!thumbOnly && needPreview)) && len(item.uploadLinks) > 0 {
		logf("  ⚡ trying FFmpeg fallback for %s…", rec.Filename)

		cdnURL, fileSize, err := resolveMediaSource(item.uploadLinks, streamtapeLogin, streamtapeKey)
		if err != nil {
			logf("  ⚡ no resolvable media source: %v", err)
			atomic.AddInt64(&cntNoSource, 1)
		} else {
			genThumb := needThumb
			genPrev := !thumbOnly && needPreview

			tPath, pPath, tmpDir, genErr := generateMediaFromURL(cdnURL, rec.Filename, fileSize, genThumb, genPrev)
			if tmpDir != "" {
				defer os.RemoveAll(tmpDir)
			}
			if genErr != nil {
				logf("  ⚡ FFmpeg generation failed: %v", genErr)
			} else {
				upThumb, upPrev, upErr := uploadGeneratedMedia(tPath, pPath)
				if upErr != nil {
					logf("  ⚡ upload failed: %v", upErr)
				} else {
					if upThumb != "" {
						thumb = upThumb
						needThumb = false
						atomic.AddInt64(&cntFFmpegThumb, 1)
					}
					if upPrev != "" {
						preview = upPrev
						needPreview = false
						atomic.AddInt64(&cntFFmpegPrev, 1)
					}
				}
			}
		}
	}

	// ── Check if anything changed ────────────────────────────────────────────
	if needThumb || needPreview {
		atomic.AddInt64(&cntNotReady, 1)
		cacheMu.Lock()
		cacheMap[rec.Filename] = cacheEntry{NotAvail: true}
		cacheMu.Unlock()
		return false
	}

	changed := thumb != rec.ThumbnailURL || preview != rec.PreviewURL
	if !changed {
		atomic.AddInt64(&cntFailed, 1)
		return false
	}

	if dryRun {
		logf("  [dry-run] would set thumb=%v preview=%v for %s",
			thumb != rec.ThumbnailURL, preview != rec.PreviewURL, rec.Filename)
		return true
	}

	if err := server.UpdateRecordingMediaURLs(rec.Filename, thumb, preview); err != nil {
		errorf("  DB patch failed for %s: %v", rec.Filename, err)
		atomic.AddInt64(&cntFailed, 1)
		return false
	}
	logf("  ✓ DB updated for %s (thumb=%v preview=%v)", rec.Filename,
		thumb != rec.ThumbnailURL, preview != rec.PreviewURL)

	cacheMu.Lock()
	cacheMap[rec.Filename] = cacheEntry{Thumbnail: thumb, Preview: preview}
	cacheMu.Unlock()
	return true
}

// ─── Workflow trigger ────────────────────────────────────────────────────────

func triggerWorkflowDispatch(repo, token string) error {
	logf("Triggering workflow_dispatch for %s...", repo)

	ref := os.Getenv("GITHUB_REF_NAME")
	if ref == "" {
		ref = "main"
	}

	urlStr := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/backfill.yml/dispatches", repo)
	body, err := json.Marshal(map[string]string{"ref": ref})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", urlStr, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Go-GitHub-Trigger")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	logf("Successfully triggered workflow dispatch!")
	return nil
}

// ─── main ────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmsgprefix)

	if !*flagNoCache {
		loadCache()
	}
	initCache()
	defer saveCache()

	loadDotEnv(".env")

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	}
	seekKey := os.Getenv("SEEKSTREAMING_KEY")
	streamtapeLogin := os.Getenv("STREAMTAPE_LOGIN")
	streamtapeKey := os.Getenv("STREAMTAPE_API_KEY")
	if streamtapeKey == "" {
		streamtapeKey = os.Getenv("STREAMTAPE_KEY")
	}
	if streamtapeLogin == "" || streamtapeKey == "" {
		logf("Streamtape credentials not configured — skipping Streamtape resolution")
	} else {
		logf("Streamtape resolution enabled")
	}
	ffmpegPath := os.Getenv("FFMPEG_PATH")


	var upnKeys []string
	if v := os.Getenv("UPNSHARE_KEYS"); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				upnKeys = append(upnKeys, k)
			}
		}
	}
	if len(upnKeys) == 0 {
		if v := strings.TrimSpace(os.Getenv("UPNSHARE_KEY")); v != "" {
			upnKeys = append(upnKeys, v)
		}
	}

	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY (or SUPABASE_SERVICE_ROLE_KEY) must be set")
	}

	server.Config = &entity.Config{
		SupabaseURL:      supabaseURL,
		SupabaseAPIKey:   supabaseKey,
		SeekStreamingKey: seekKey,
		UpnshareKeys:     upnKeys,
		FFmpegPath:       ffmpegPath,
	}

	if ffmpegPath != "" {
		config.SetFFmpegPath(ffmpegPath)
	}

	client := database.NewClient(supabaseURL, supabaseKey)

	logf("Fetching all recordings…")
	recordings, err := client.GetAllRecordings()
	if err != nil {
		log.Fatalf("GetAllRecordings: %v", err)
	}
	logf("Total recordings: %d", len(recordings))

	// Pre-fetch all upload links for the FFmpeg fallback
	var uploadLinksByRecID map[string][]database.UploadLink
	if !*flagNoFFmpeg {
		uploadLinksByRecID = loadAllUploadLinks(client)
	} else {
		logf("FFmpeg fallback disabled via -no-ffmpeg flag")
	}

	// Filter to those missing a thumbnail and/or preview.
	var todo []workItem
	for _, r := range recordings {
		if r.ThumbnailURL != "" && r.PreviewURL != "" {
			continue
		}

		host, videoID := uploader.MediaHostOf(r.EmbedURL)

		// If embed URL doesn't resolve, try upload links for SeekStreaming/UPnShare
		if (host == "" || videoID == "") && uploadLinksByRecID != nil {
			if links, ok := uploadLinksByRecID[r.ID]; ok {
				for _, link := range links {
					h, vid := uploader.MediaHostOf(link.URL)
					if h != "" && vid != "" {
						host, videoID = h, vid
						break
					}
				}
			}
		}

		item := workItem{rec: r, host: host, videoID: videoID}

		// Attach upload links for FFmpeg fallback
		if host == "" || videoID == "" {
			if uploadLinksByRecID != nil {
				if links, ok := uploadLinksByRecID[r.ID]; ok {
					item.uploadLinks = links
				}
			}
		} else if uploadLinksByRecID != nil {
			if links, ok := uploadLinksByRecID[r.ID]; ok {
				item.uploadLinks = links
			}
		}

		todo = append(todo, item)
	}

	logf("Recordings needing work (with any data): %d", len(todo))

	// Count how many have upload links vs not
	ffmpegCount := 0
	for _, item := range todo {
		if (item.host == "" || item.videoID == "") && len(item.uploadLinks) > 0 {
			ffmpegCount++
		}
	}
	logf("  — with resolvable host API: %d", len(todo)-ffmpegCount)
	logf("  — FFmpeg fallback candidates: %d", ffmpegCount)

	totalPending := len(todo)

	// Split work across matrix shards
	if *flagShards > 1 {
		shard := *flagShard
		if shard < 0 {
			shard = 0
		}
		if shard >= *flagShards {
			shard = *flagShards - 1
		}
		var sliced []workItem
		for i := shard; i < len(todo); i += *flagShards {
			sliced = append(sliced, todo[i])
		}
		todo = sliced
		logf("Shard %d/%d — processing %d of %d pending recordings", shard, *flagShards, len(todo), totalPending)
	}

	if *flagLimit > 0 && len(todo) > *flagLimit {
		logf("Limiting to %d recordings (--limit flag)", *flagLimit)
		todo = todo[:*flagLimit]
	}
	if *flagDryRun {
		logf("*** DRY RUN — no writes will occur ***")
	}

	start := time.Now()
	var durationExceeded bool

	if *flagConcurrency <= 1 {
		for i, item := range todo {
			if *flagDuration != "" {
				maxDur, err := time.ParseDuration(*flagDuration)
				if err == nil && time.Since(start) >= maxDur {
					logf("Duration limit of %s reached. Exiting loop to trigger next run...", *flagDuration)
					durationExceeded = true
					break
				}
			}

			didWork := processOne(item, seekKey, upnKeys, streamtapeLogin, streamtapeKey, *flagDryRun, *flagThumbOnly)

			if i < len(todo)-1 && didWork {
				if *flagDelay != "" {
					dDur, err := time.ParseDuration(*flagDelay)
					if err == nil && dDur > 0 {
						logf("Waiting %s before next backfill to avoid rate-limiting...", *flagDelay)
						time.Sleep(dDur)
					}
				}
			}
		}
	} else {
		work := make(chan workItem, *flagConcurrency*4)
		var wg sync.WaitGroup
		for i := 0; i < *flagConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range work {
					processOne(item, seekKey, upnKeys, streamtapeLogin, streamtapeKey, *flagDryRun, *flagThumbOnly)
				}
			}()
		}
		for _, item := range todo {
			work <- item
		}
		close(work)
		wg.Wait()
	}

	if durationExceeded && *flagTrigger && *flagShard == 0 {
		githubToken := os.Getenv("GITHUB_TOKEN")
		if githubToken == "" {
			githubToken = os.Getenv("GH_TOKEN")
		}
		githubRepo := os.Getenv("GITHUB_REPOSITORY")
		if githubToken != "" && githubRepo != "" {
			if err := triggerWorkflowDispatch(githubRepo, githubToken); err != nil {
				errorf("Failed to trigger workflow dispatch: %v", err)
			}
		} else {
			errorf("GITHUB_TOKEN/GH_TOKEN or GITHUB_REPOSITORY not set, cannot trigger next workflow")
		}
	}

	elapsed := time.Since(start).Round(time.Second)
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("              BACKFILL COMPLETE")
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  Elapsed:              %v\n", elapsed)
	fmt.Printf("  Total processed:      %d\n", atomic.LoadInt64(&cntTotal))
	fmt.Printf("  ✓ Thumbnails (API):   %d\n", atomic.LoadInt64(&cntThumb))
	fmt.Printf("  ✓ Previews (API):     %d\n", atomic.LoadInt64(&cntPreview))
	fmt.Printf("  ✓ Thumbnails (FFmpeg):%d\n", atomic.LoadInt64(&cntFFmpegThumb))
	fmt.Printf("  ✓ Previews (FFmpeg):  %d\n", atomic.LoadInt64(&cntFFmpegPrev))
	fmt.Printf("  ⏳ Not ready yet:      %d\n", atomic.LoadInt64(&cntNotReady))
	fmt.Printf("  ✗ Failed:             %d\n", atomic.LoadInt64(&cntFailed))
	fmt.Printf("  ⏭ Already complete:   %d\n", atomic.LoadInt64(&cntSkipped))
	fmt.Printf("  ⚡ Cache hits:         %d\n", atomic.LoadInt64(&cntCacheHit))
	fmt.Printf("  ❌ No media source:    %d\n", atomic.LoadInt64(&cntNoSource))
	fmt.Println("═══════════════════════════════════════════════════")
}
