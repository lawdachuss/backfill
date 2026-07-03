//go:build ignore

// backfill_media.go — backfills missing thumbnail, sprite, and preview URLs for
// all recordings in the Supabase database.
//
// Usage:
//   go run scripts/backfill_media.go [flags]
//
// Flags:
//   -dry-run        Print what would be done without writing to DB or downloading
//   -concurrency N  Number of concurrent workers (default 2)
//   -thumb-only     Only backfill thumbnails (fast, no download needed for SeekStreaming)
//   -limit N        Stop after processing N recordings (0 = unlimited)
//
// Strategy:
//   Phase 1 (thumbnails) — for recordings that have a SeekStreaming embed URL but
//     no thumbnail_url, fetch the poster image from the SeekStreaming API. No local
//     video file needed.
//   Phase 2 (sprite + preview, and remaining thumbnails) — downloads the video from
//     GoFile, runs GenerateThumbnailForFile, uploads results, then deletes temp file.
//     Falls back to Mixdrop embed URL download if GoFile is unavailable.

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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	flagDryRun      = flag.Bool("dry-run", false, "Print plan without writing to DB or downloading")
	flagConcurrency = flag.Int("concurrency", 2, "Concurrent workers")
	flagThumbOnly   = flag.Bool("thumb-only", false, "Only backfill thumbnails (no downloads)")
	flagLimit       = flag.Int("limit", 0, "Max recordings to process (0 = unlimited)")
	flagDuration    = flag.String("duration", "", "Max duration to run before exiting (e.g. 5h45m)")
	flagDelay       = flag.String("delay", "", "Delay between consecutive video backfills (e.g. 5m)")
	flagTrigger     = flag.Bool("trigger-workflow", false, "Trigger a new workflow run on exit if duration exceeded")
)

// ─── counters ─────────────────────────────────────────────────────────────────

var (
	cntTotal      int64
	cntThumb      int64
	cntSprite     int64
	cntPreview    int64
	cntDownloaded int64
	cntSkipped    int64
	cntFailed     int64
)

// ─── .env loader (same pattern as upload_videos.go) ─────────────────────────

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
		v := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
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

// ─── Streamtape download ───────────────────────────────────────────────────────

// stHTTPClient is a shared HTTP client for Streamtape API calls.
var stHTTPClient = &http.Client{Timeout: 60 * time.Minute}

func httpGetJSON(rawURL string) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	return result, json.NewDecoder(resp.Body).Decode(&result)
}

// extractStreamtapeID extracts the video ID from a Streamtape embed or page URL.
// e.g. https://streamtape.com/e/0Bygzkk7D8fbXlK/ → 0Bygzkk7D8fbXlK
func extractStreamtapeID(stURL string) string {
	parts := strings.Split(strings.TrimRight(stURL, "/"), "/")
	for i, p := range parts {
		if (p == "e" || p == "v") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	// fallback: last non-empty segment
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return ""
}

// getStreamtapeDirectURL obtains the CDN direct URL for a Streamtape video
// without downloading any content. It performs the ticket + dl API steps.
func getStreamtapeDirectURL(stEmbedURL, stLogin, stKey string) (string, error) {
	videoID := extractStreamtapeID(stEmbedURL)
	if videoID == "" {
		return "", fmt.Errorf("cannot extract video ID from: %s", stEmbedURL)
	}

	var ticket string
	var resultData map[string]interface{}
	ticketURL := fmt.Sprintf(
		"https://api.streamtape.com/file/dlticket?file=%s&login=%s&key=%s",
		videoID, stLogin, stKey)

	for attempt := 1; attempt <= 5; attempt++ {
		ticketData, err := httpGetJSON(ticketURL)
		if err != nil {
			return "", fmt.Errorf("streamtape ticket request: %w", err)
		}
		statusVal, _ := ticketData["status"].(float64)
		msg, _ := ticketData["msg"].(string)
		if statusVal == 403 || statusVal == 429 || strings.Contains(msg, "wait") {
			logf("  streamtape: ticket rate-limited (%s), waiting 4s (attempt %d/5)…", msg, attempt)
			time.Sleep(4 * time.Second)
			continue
		}
		if result, _ := ticketData["result"].(string); result != "" && strings.Contains(result, "Error") {
			return "", fmt.Errorf("streamtape ticket error: %s", result)
		}
		resultData, _ = ticketData["result"].(map[string]interface{})
		ticket, _ = resultData["ticket"].(string)
		if ticket != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if ticket == "" {
		return "", fmt.Errorf("no ticket from Streamtape after retries")
	}
	if wait, ok := resultData["wait"].(float64); ok && wait > 0 {
		logf("  streamtape: waiting %.0fs before dl URL…", wait)
		time.Sleep(time.Duration(wait+1) * time.Second)
	}

	dlInfoURL := fmt.Sprintf("https://api.streamtape.com/file/dl?file=%s&ticket=%s", videoID, ticket)
	for attempt := 1; attempt <= 6; attempt++ {
		dlData, err := httpGetJSON(dlInfoURL)
		if err != nil {
			return "", fmt.Errorf("streamtape dl info: %w", err)
		}
		statusVal, _ := dlData["status"].(float64)
		msg, _ := dlData["msg"].(string)
		dlResult, _ := dlData["result"].(map[string]interface{})
		directURL, _ := dlResult["url"].(string)
		if directURL != "" {
			return directURL, nil
		}
		if statusVal == 403 || statusVal == 429 || strings.Contains(msg, "wait") {
			logf("  streamtape: dl URL rate-limited (%s), waiting 4s (attempt %d/6)…", msg, attempt)
			time.Sleep(4 * time.Second)
			continue
		}
		return "", fmt.Errorf("no direct URL from Streamtape: %v", dlData)
	}
	return "", fmt.Errorf("failed to get direct URL from Streamtape after retries")
}

// ─── URL-based media generation ──────────────────────────────────────────────────────────────────
//
// Instead of downloading the entire video (which is slow and disk-heavy),
// we pass the Streamtape CDN URL directly to FFmpeg. FFmpeg uses HTTP range
// requests to seek to specific timestamps, downloading only what it needs:
//
//   Thumbnail:  ~10 MB  (seeks to 15%, grabs 1 frame)
//   Sprite:     ~80 MB  (seeks to 16 positions, 1 frame each, then xstack)
//   Preview:   ~150 MB  (seeks to 16 positions, 0.56s clip each, then concat)
//
// This replaces a 912 MB full download with ~240 MB of targeted range reads.

// ffmpegBin returns the ffmpeg binary path from env or PATH.
func ffmpegBin() string {
	if p := os.Getenv("FFMPEG_PATH"); p != "" {
		return p
	}
	return "ffmpeg"
}

// ffprobeBin returns the ffprobe binary path derived from ffmpegBin.
func ffprobeBin() string {
	bin := ffmpegBin()
	// If it's a full path like /usr/bin/ffmpeg, swap to ffprobe.
	if strings.HasSuffix(bin, "ffmpeg") {
		return bin[:len(bin)-len("ffmpeg")] + "ffprobe"
	}
	return "ffprobe"
}

// ffmpegRun runs ffmpeg with the given arguments and returns any error.
func ffmpegRun(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, ffmpegBin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n--- ffmpeg output ---\n%s", err, string(out))
	}
	return nil
}

// ffprobeURLDuration probes the duration of a remote video URL using ffprobe.
func ffprobeURLDuration(videoURL string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobeBin(),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoURL,
	).Output()
	if err != nil {
		return 0
	}
	dur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return dur
}

// generateMediaFromURL generates thumbnail, sprite, and preview for a video
// reachable at cdnURL using FFmpeg HTTP range requests. No full download.
func generateMediaFromURL(cdnURL, filename string, needThumb, needSprite, needPreview bool) (thumbURL, spriteURL, previewURL string) {
	tmpDir, err := os.MkdirTemp("", "backfill-url-*")
	if err != nil {
		errorf("  temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	logf("  🔍 probing duration via ffprobe…")
	dur := ffprobeURLDuration(cdnURL)
	if dur <= 0 {
		errorf("  could not probe duration from URL")
		return
	}
	logf("  ✓ duration: %.1fs (%.1f min)", dur, dur/60)

	type result struct{ url string }
	thumbCh := make(chan result, 1)
	spriteCh := make(chan result, 1)
	previewCh := make(chan result, 1)

	if needThumb {
		go func() {
			url, err := urlGenThumbnail(cdnURL, dur, tmpDir, filename)
			if err != nil {
				errorf("  thumbnail URL gen failed: %v", err)
			}
			thumbCh <- result{url}
		}()
	} else {
		thumbCh <- result{}
	}

	if needSprite {
		go func() {
			url, err := urlGenSprite(cdnURL, dur, tmpDir, filename)
			if err != nil {
				errorf("  sprite URL gen failed: %v", err)
			}
			spriteCh <- result{url}
		}()
	} else {
		spriteCh <- result{}
	}

	if needPreview {
		go func() {
			url, err := urlGenPreview(cdnURL, dur, tmpDir, filename)
			if err != nil {
				errorf("  preview URL gen failed: %v", err)
			}
			previewCh <- result{url}
		}()
	} else {
		previewCh <- result{}
	}

	thumbURL = (<-thumbCh).url
	spriteURL = (<-spriteCh).url
	previewURL = (<-previewCh).url
	return
}

const (
	urlThumbW, urlThumbH   = 1280, 720
	urlSpriteW, urlSpriteH = 640, 360
	urlSpriteCols          = 4
	urlSpriteRows          = 4
	urlSpriteN             = urlSpriteCols * urlSpriteRows // 16
	urlPreviewW            = 320
	urlPreviewDur          = 9.0  // total preview seconds
	urlPreviewSegs         = 16   // number of clips
)

// urlGenThumbnail seeks to 15% of the video and grabs one frame.
// FFmpeg uses an HTTP range request — only ~10 MB downloaded.
func urlGenThumbnail(cdnURL string, dur float64, tmpDir, filename string) (string, error) {
	seekPos := 3.0
	if dur > 0 && dur < 3 {
		seekPos = dur * 0.5
	} else if dur > 0 {
		seekPos = dur * 0.15
	}

	thumbPath := filepath.Join(tmpDir, filename+".thumb.jpg")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err := ffmpegRun(ctx,
		"-y",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
		"-ss", fmt.Sprintf("%.2f", seekPos),
		"-i", cdnURL,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
			urlThumbW, urlThumbH, urlThumbW, urlThumbH),
		"-c:v", "mjpeg", "-q:v", "3",
		thumbPath,
	)
	if err != nil {
		return "", fmt.Errorf("ffmpeg thumbnail: %w", err)
	}

	imgUploader := uploader.NewMultiImageUploader()
	remoteURL, _, uploadErr := imgUploader.Upload(thumbPath)
	if uploadErr != nil {
		return "", fmt.Errorf("upload thumbnail: %w", uploadErr)
	}
	logf("  ✓ thumbnail: %s", remoteURL)
	return remoteURL, nil
}

// urlGenSprite extracts 16 individual frames via -ss HTTP range seeks,
// then tiles them into a 4×4 grid using xstack.
func urlGenSprite(cdnURL string, dur float64, tmpDir, filename string) (string, error) {
	interval := 10.0
	if dur > 0 {
		interval = dur / float64(urlSpriteN)
		if interval < 0.1 {
			interval = 0.1
		}
	}

	var framePaths []string
	for i := 0; i < urlSpriteN; i++ {
		pos := float64(i)*interval + interval*0.5
		if dur > 0 && pos > dur {
			pos = dur - 0.5
		}
		framePath := filepath.Join(tmpDir, fmt.Sprintf("frame_%02d.jpg", i))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := ffmpegRun(ctx,
			"-y",
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
			"-ss", fmt.Sprintf("%.2f", pos),
			"-i", cdnURL,
			"-vframes", "1",
			"-vf", fmt.Sprintf(
				"scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
				urlSpriteW, urlSpriteH, urlSpriteW, urlSpriteH),
			"-c:v", "mjpeg", "-q:v", "3",
			framePath,
		)
		cancel()
		if err != nil {
			return "", fmt.Errorf("sprite frame %d: %w", i, err)
		}
		framePaths = append(framePaths, framePath)
		logf("  sprite frame %d/%d done", i+1, urlSpriteN)
	}

	// Build xstack layout: columns × rows at 640×360 each
	var inputArgs, inputLabels, layoutParts []string
	for i, p := range framePaths {
		inputArgs = append(inputArgs, "-i", p)
		inputLabels = append(inputLabels, fmt.Sprintf("[%d]", i))
		col := i % urlSpriteCols
		row := i / urlSpriteCols
		layoutParts = append(layoutParts, fmt.Sprintf("%d_%d", col*urlSpriteW, row*urlSpriteH))
	}

	spritePath := filepath.Join(tmpDir, filename+".sprite.jpg")
	filterComplex := strings.Join(inputLabels, "") +
		fmt.Sprintf("xstack=inputs=%d:layout=%s:fill=black[out]",
			urlSpriteN, strings.Join(layoutParts, "|"))

	tileArgs := append([]string{"-y"}, inputArgs...)
	tileArgs = append(tileArgs,
		"-filter_complex", filterComplex,
		"-map", "[out]",
		"-c:v", "mjpeg", "-q:v", "3",
		spritePath)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ffmpegRun(ctx, tileArgs...); err != nil {
		return "", fmt.Errorf("xstack: %w", err)
	}

	imgUploader := uploader.NewMultiImageUploader()
	remoteURL, _, uploadErr := imgUploader.Upload(spritePath)
	if uploadErr != nil {
		return "", fmt.Errorf("upload sprite: %w", uploadErr)
	}
	logf("  ✓ sprite: %s", remoteURL)
	return remoteURL, nil
}

// urlGenPreview extracts 16 short clips via -ss HTTP range seeks,
// writes a concat list, and stitches them into a 9-second preview MP4.
func urlGenPreview(cdnURL string, dur float64, tmpDir, filename string) (string, error) {
	segDur := urlPreviewDur / float64(urlPreviewSegs) // ~0.5625s per clip
	step := 10.0
	if dur > 0 {
		step = dur / float64(urlPreviewSegs)
	}

	var clipPaths []string
	for i := 0; i < urlPreviewSegs; i++ {
		midpoint := step * (float64(i) + 0.5)
		startPos := midpoint - segDur/2
		if startPos < 0 {
			startPos = 0
		}
		if dur > 0 && startPos+segDur > dur {
			startPos = dur - segDur
		}

		clipPath := filepath.Join(tmpDir, fmt.Sprintf("clip_%02d.mp4", i))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err := ffmpegRun(ctx,
			"-y",
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
			"-ss", fmt.Sprintf("%.3f", startPos),
			"-i", cdnURL,
			"-t", fmt.Sprintf("%.3f", segDur),
			"-vf", fmt.Sprintf("scale=%d:-2:flags=lanczos", urlPreviewW),
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28",
			"-pix_fmt", "yuv420p", "-an",
			clipPath,
		)
		cancel()
		if err != nil {
			return "", fmt.Errorf("preview clip %d: %w", i, err)
		}
		clipPaths = append(clipPaths, clipPath)
		logf("  preview clip %d/%d done", i+1, urlPreviewSegs)
	}

	// Write concat list
	concatListPath := filepath.Join(tmpDir, "concat_list.txt")
	var concatLines []string
	for _, p := range clipPaths {
		concatLines = append(concatLines, fmt.Sprintf("file '%s'", p))
	}
	if err := os.WriteFile(concatListPath, []byte(strings.Join(concatLines, "\n")), 0644); err != nil {
		return "", fmt.Errorf("write concat list: %w", err)
	}

	previewPath := filepath.Join(tmpDir, filename+".preview.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := ffmpegRun(ctx,
		"-y",
		"-f", "concat", "-safe", "0", "-i", concatListPath,
		"-c", "copy",
		"-movflags", "+faststart",
		previewPath,
	); err != nil {
		return "", fmt.Errorf("concat: %w", err)
	}

	fi, _ := os.Stat(previewPath)
	previewSize := "unknown"
	if fi != nil {
		previewSize = fmt.Sprintf("%.1f MB", float64(fi.Size())/1024/1024)
	}

	catbox := uploader.NewCatboxUploader()
	remoteURL, err := catbox.Upload(previewPath)
	if err != nil {
		catboxErr := err
		logf("  catbox failed (%s): %v", previewSize, catboxErr)
		lobfile := uploader.NewLobFileUploader(os.Getenv("LOBFILE_API_KEY"))
		remoteURL, err = lobfile.Upload(previewPath)
		if err != nil {
			return "", fmt.Errorf("upload preview (%s, catbox+lobfile both failed): catbox: %v; lobfile: %w", previewSize, catboxErr, err)
		}
	}
	logf("  ✓ preview: %s", remoteURL)
	return remoteURL, nil
}



// ─── SeekStreaming thumbnail ──────────────────────────────────────────────────

func seekPosterURL(embedURL, seekKey string) string {
	if seekKey == "" || embedURL == "" {
		return ""
	}
	videoID := uploader.ExtractSeekStreamingVideoID(embedURL)
	if videoID == "" {
		return ""
	}
	url, err := uploader.GetSeekStreamingPosterURL(seekKey, videoID)
	if err != nil {
		return ""
	}
	return url
}

// ─── DB patching ─────────────────────────────────────────────────────────────

func patchDB(filename, thumb, sprite, preview string, dryRun bool) error {
	if dryRun {
		logf("  [dry-run] patch %s → thumb=%v sprite=%v preview=%v",
			filename, thumb != "", sprite != "", preview != "")
		return nil
	}
	// Patch recordings table (primary)
	if err := server.UpdateRecordingThumbnails(filename, thumb, sprite, preview); err != nil {
		return fmt.Errorf("patch recordings: %w", err)
	}
	// Also upsert into preview_images (backward compat with LoadPreviewLinks)
	if err := server.SavePreviewLinks(filename, thumb, sprite, preview); err != nil {
		logf("  warn: preview_images upsert for %s: %v", filename, err)
	}
	return nil
}

// ─── worker ──────────────────────────────────────────────────────────────────

type workItem struct {
	rec   database.Recording
	links map[string]string // host → URL
}

func processOne(item workItem, seekKey, stLogin, stKey string, dryRun, thumbOnly bool) bool {
	rec := item.rec
	atomic.AddInt64(&cntTotal, 1)

	needThumb   := rec.ThumbnailURL == ""
	needSprite  := rec.SpriteURL == ""
	needPreview := rec.PreviewURL == ""

	if !needThumb && !needSprite && !needPreview {
		atomic.AddInt64(&cntSkipped, 1)
		return false
	}

	logf("%-60s  [thumb=%v sprite=%v preview=%v]",
		rec.Filename, needThumb, needSprite, needPreview)

	thumb   := rec.ThumbnailURL
	sprite  := rec.SpriteURL
	preview := rec.PreviewURL

	// ── Phase 1: SeekStreaming poster → thumbnail (zero downloads needed) ─────
	if needThumb && strings.Contains(rec.EmbedURL, "seekstreaming") {
		if url := seekPosterURL(rec.EmbedURL, seekKey); url != "" {
			logf("  ✓ thumb via SeekStreaming poster")
			thumb = url
			needThumb = false
			atomic.AddInt64(&cntThumb, 1)
		}
	}

	// ── Phase 2: URL-based generation via Streamtape CDN + FFmpeg range requests ─
	// No full file download — FFmpeg seeks via HTTP range requests.
	if !thumbOnly && (needThumb || needSprite || needPreview) {
		stURL := item.links["Streamtape"]
		if stURL == "" {
			errorf("  no Streamtape link — skipping %s", rec.Filename)
			atomic.AddInt64(&cntFailed, 1)
			if thumb != rec.ThumbnailURL {
				_ = patchDB(rec.Filename, thumb, sprite, preview, dryRun)
			}
			return false
		}

		if dryRun {
			logf("  [dry-run] would fetch Streamtape CDN URL and run FFmpeg (no download)")
			_ = patchDB(rec.Filename, thumb, sprite, preview, dryRun)
			return true
		}

		logf("  🔗 getting Streamtape CDN URL (no download)…")
		cdnURL, err := getStreamtapeDirectURL(stURL, stLogin, stKey)
		if err != nil {
			errorf("  CDN URL failed: %v", err)
			atomic.AddInt64(&cntFailed, 1)
			if thumb != rec.ThumbnailURL {
				_ = patchDB(rec.Filename, thumb, sprite, preview, dryRun)
			}
			return true // tried Streamtape, count as tried
		}
		logf("  ✓ CDN URL acquired — generating via FFmpeg HTTP range requests…")
		atomic.AddInt64(&cntDownloaded, 1) // count as "processed"

		genThumb, genSprite, genPreview := generateMediaFromURL(cdnURL, rec.Filename, needThumb, needSprite, needPreview)

		if needThumb && genThumb != "" {
			thumb = genThumb
			atomic.AddInt64(&cntThumb, 1)
		}
		if needSprite && genSprite != "" {
			sprite = genSprite
			atomic.AddInt64(&cntSprite, 1)
		}
		if needPreview && genPreview != "" {
			preview = genPreview
			atomic.AddInt64(&cntPreview, 1)
		}
	}

	// ── Commit to DB ──────────────────────────────────────────────────────────
	changed := thumb != rec.ThumbnailURL || sprite != rec.SpriteURL || preview != rec.PreviewURL
	if !changed {
		errorf("  nothing to update for %s", rec.Filename)
		atomic.AddInt64(&cntFailed, 1)
		return true
	}
	if err := patchDB(rec.Filename, thumb, sprite, preview, dryRun); err != nil {
		errorf("  DB patch failed for %s: %v", rec.Filename, err)
		atomic.AddInt64(&cntFailed, 1)
		return true
	}
	logf("  ✓ DB updated for %s", rec.Filename)
	return true
}

// triggerWorkflowDispatch triggers a workflow dispatch on the specified repository.
func triggerWorkflowDispatch(repo, token string) error {
	logf("Triggering workflow_dispatch for %s...", repo)
	
	// Default to main or get from GITHUB_REF_NAME
	ref := os.Getenv("GITHUB_REF_NAME")
	if ref == "" {
		ref = "main"
	}
	
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/backfill.yml/dispatches", repo)
	
	body, err := json.Marshal(map[string]string{
		"ref": ref,
	})
	if err != nil {
		return err
	}
	
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
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

	// Load .env if present
	loadDotEnv(".env")

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_API_KEY")
	}
	seekKey    := os.Getenv("SEEKSTREAMING_KEY")
	ffmpegPath := os.Getenv("FFMPEG_PATH")
	stLogin    := os.Getenv("STREAMTAPE_LOGIN")
	stKey      := os.Getenv("STREAMTAPE_API_KEY")

	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY (or SUPABASE_SERVICE_ROLE_KEY) must be set")
	}
	if stLogin == "" || stKey == "" {
		log.Fatal("STREAMTAPE_LOGIN and STREAMTAPE_API_KEY must be set in .env")
	}

	// Bootstrap server config
	server.Config = &entity.Config{
		SupabaseURL:      supabaseURL,
		SupabaseAPIKey:   supabaseKey,
		SeekStreamingKey: seekKey,
		StreamtapeLogin:  stLogin,
		StreamtapeKey:    stKey,
		FFmpegPath:       ffmpegPath,
	}

	// Bootstrap FFmpeg path so GenerateThumbnailForFile works
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

	// Filter to those missing at least one URL
	var todo []database.Recording
	for _, r := range recordings {
		if r.ThumbnailURL == "" || r.SpriteURL == "" || r.PreviewURL == "" {
			todo = append(todo, r)
		}
	}
	logf("Missing at least one media URL: %d", len(todo))

	if *flagLimit > 0 && len(todo) > *flagLimit {
		logf("Limiting to %d recordings (--limit flag)", *flagLimit)
		todo = todo[:*flagLimit]
	}
	if *flagDryRun {
		logf("*** DRY RUN — no writes will occur ***")
	}

	// Batch-fetch all upload links at once (avoid N+1 queries)
	logf("Fetching all upload links…")
	allLinks, err := client.GetAllUploadLinks()
	if err != nil {
		log.Fatalf("GetAllUploadLinks: %v", err)
	}
	linksByID := make(map[string]map[string]string, len(recordings))
	for _, l := range allLinks {
		m := linksByID[l.RecordingID]
		if m == nil {
			m = make(map[string]string)
			linksByID[l.RecordingID] = m
		}
		m[l.Host] = l.URL
	}
	logf("Loaded %d upload link rows", len(allLinks))

	start := time.Now()
	var durationExceeded bool

	// If concurrency is 1, process sequentially with delay and duration checks.
	if *flagConcurrency == 1 {
		for i, r := range todo {
			if *flagDuration != "" {
				maxDur, err := time.ParseDuration(*flagDuration)
				if err == nil && time.Since(start) >= maxDur {
					logf("Duration limit of %s reached. Exiting loop to trigger next run...", *flagDuration)
					durationExceeded = true
					break
				}
			}

			item := workItem{rec: r, links: linksByID[r.ID]}
			didWork := processOne(item, seekKey, stLogin, stKey, *flagDryRun, *flagThumbOnly)

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
		// Fall back to original concurrent processing (no delays between jobs, just concurrent execution)
		work := make(chan workItem, *flagConcurrency*4)
		var wg sync.WaitGroup
		for i := 0; i < *flagConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range work {
					processOne(item, seekKey, stLogin, stKey, *flagDryRun, *flagThumbOnly)
				}
			}()
		}
		for _, r := range todo {
			work <- workItem{rec: r, links: linksByID[r.ID]}
		}
		close(work)
		wg.Wait()
	}

	// ── Trigger Next Run ─────────────────────────────────────────────────────────
	if durationExceeded && *flagTrigger {
		githubToken := os.Getenv("GITHUB_TOKEN")
		if githubToken == "" {
			githubToken = os.Getenv("GH_TOKEN")
		}
		githubRepo := os.Getenv("GITHUB_REPOSITORY") // e.g. owner/repo
		if githubToken != "" && githubRepo != "" {
			err := triggerWorkflowDispatch(githubRepo, githubToken)
			if err != nil {
				errorf("Failed to trigger workflow dispatch: %v", err)
			}
		} else {
			errorf("GITHUB_TOKEN/GH_TOKEN or GITHUB_REPOSITORY not set, cannot trigger next workflow")
		}
	}

	// ── Report ───────────────────────────────────────────────────────────────────
	elapsed := time.Since(start).Round(time.Second)
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("                 BACKFILL COMPLETE")
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  Elapsed:              %v\n", elapsed)
	fmt.Printf("  Total processed:      %d\n", atomic.LoadInt64(&cntTotal))
	fmt.Printf("  ✓ Thumbnails fixed:   %d\n", atomic.LoadInt64(&cntThumb))
	fmt.Printf("  ✓ Sprites fixed:      %d\n", atomic.LoadInt64(&cntSprite))
	fmt.Printf("  ✓ Previews fixed:     %d\n", atomic.LoadInt64(&cntPreview))
	fmt.Printf("  ⬇ Files downloaded:   %d\n", atomic.LoadInt64(&cntDownloaded))
	fmt.Printf("  ✗ Failed:             %d\n", atomic.LoadInt64(&cntFailed))
	fmt.Printf("  ⏭ Already complete:   %d\n", atomic.LoadInt64(&cntSkipped))
	fmt.Println("═══════════════════════════════════════════════════")
}
