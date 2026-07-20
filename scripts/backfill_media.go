//go:build ignore

// backfill_media.go — backfills missing thumbnail and preview URLs for all
// recordings in the Supabase database by pulling them from the upload hosts
// (SeekStreaming and UPnShare).
//
// Sprites were removed. Thumbnails and previews are now sourced entirely from
// the upload hosts' poster/preview URLs (generated after upload), so this
// script no longer downloads any video or runs FFmpeg.
//
// Strategy:
//   For every recording that is missing a thumbnail_url and/or preview_url,
//   determine which host it lives on from its embed_url (#fragment video ID):
//     - SeekStreaming  (embed contains "seekstreaming" / "seeks.cloud")
//     - UPnShare        (embed contains "upns" / "upnshare")
//   then query that host's manage API for the poster (thumbnail) and preview
//   URLs and patch them into the recordings row. SeekStreaming is tried first,
//   then UPnShare as a fallback, when the embed URL doesn't name a host.
//
// Usage:
//   go run scripts/backfill_media.go [flags]
//
// Flags:
//   -dry-run        Print what would be done without writing to DB
//   -concurrency N  Number of concurrent workers (default 1 — hosts are shared
//                   and rate-limited, so sequential is safest; raise only if you
//                   know your host quota)
//   -limit N        Stop after processing N recordings (0 = unlimited)
//   -duration       Max duration to run before exiting (e.g. 5h45m)
//   -delay          Delay between consecutive backfills (e.g. 5m)
//   -thumb-only     Only backfill thumbnails (no preview fetch)
//   -trigger-workflow  Trigger a new workflow run on exit if duration exceeded

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	flagDryRun      = flag.Bool("dry-run", false, "Print plan without writing to DB")
	flagConcurrency = flag.Int("concurrency", 1, "Concurrent workers (hosts are rate-limited; 1 is safest)")
	flagLimit       = flag.Int("limit", 0, "Max recordings to process (0 = unlimited)")
	flagDuration    = flag.String("duration", "", "Max duration to run before exiting (e.g. 5h45m)")
	flagDelay       = flag.String("delay", "", "Delay between consecutive video backfills (e.g. 5m)")
	flagThumbOnly   = flag.Bool("thumb-only", false, "Only backfill thumbnails (no preview fetch)")
	flagTrigger     = flag.Bool("trigger-workflow", false, "Trigger a new workflow run on exit if duration exceeded")
	flagShard       = flag.Int("shard", 0, "Zero-based shard index when splitting work across a matrix (0..shards-1)")
	flagShards      = flag.Int("shards", 1, "Total number of shards (1 = process everything in one job)")
)

// ─── counters ─────────────────────────────────────────────────────────────────

var (
	cntTotal      int64
	cntThumb      int64
	cntPreview    int64
	cntSkipped    int64
	cntFailed     int64
	cntNotReady   int64
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

// ─── host media fetch ─────────────────────────────────────────────────────────

// fetchHostMedia fetches poster/preview for a single video from the correct
// host backend. Only the host implied by the recording's embed URL is queried.
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
		return uploader.GetUPnShareMediaURLs(upnKeys[0], videoID, filename)
	default:
		return "", "", fmt.Errorf("unknown host %q", host)
	}
}

// ─── worker ──────────────────────────────────────────────────────────────────

type workItem struct {
	rec     database.Recording
	host    string
	videoID string
}

func processOne(item workItem, seekKey string, upnKeys []string, dryRun, thumbOnly bool) bool {
	rec := item.rec
	atomic.AddInt64(&cntTotal, 1)

	needThumb := rec.ThumbnailURL == ""
	needPreview := rec.PreviewURL == ""

	if !needThumb && !needPreview {
		atomic.AddInt64(&cntSkipped, 1)
		return false
	}

	logf("%-60s  [thumb=%v preview=%v]  host=%s",
		rec.Filename, needThumb, needPreview, item.host)

	thumb := rec.ThumbnailURL
	preview := rec.PreviewURL

	// Determine which host(s) to query. If the embed URL names a host, use it;
	// otherwise try SeekStreaming first, then UPnShare.
	hosts := []string{}
	if item.host != "" {
		hosts = append(hosts, item.host)
	} else {
		hosts = append(hosts, "seekstreaming", "upnshare")
	}

	for _, host := range hosts {
		t, p, err := fetchHostMedia(seekKey, upnKeys, host, item.videoID, rec.Filename)
		if err != nil {
			// Almost always "media not generated yet" (404/400). Treat as
			// not-ready and try the next host rather than a hard failure.
			errorf("  %s fetch for %s not ready: %v", host, rec.Filename, err)
			atomic.AddInt64(&cntNotReady, 1)
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

	changed := thumb != rec.ThumbnailURL || preview != rec.PreviewURL
	if !changed {
		errorf("  nothing to update for %s", rec.Filename)
		atomic.AddInt64(&cntFailed, 1)
		return false
	}

	if dryRun {
		logf("  [dry-run] would set thumb=%v preview=%v for %s",
			thumb != rec.ThumbnailURL, preview != rec.PreviewURL, rec.Filename)
		return true
	}

	patchThumb := thumb
	patchPreview := preview
	if err := server.UpdateRecordingMediaURLs(rec.Filename, patchThumb, patchPreview); err != nil {
		errorf("  DB patch failed for %s: %v", rec.Filename, err)
		atomic.AddInt64(&cntFailed, 1)
		return false
	}
	logf("  ✓ DB updated for %s (thumb=%v preview=%v)", rec.Filename,
		thumb != rec.ThumbnailURL, preview != rec.PreviewURL)
	return true
}

// triggerWorkflowDispatch triggers a workflow dispatch on the specified repository.
func triggerWorkflowDispatch(repo, token string) error {
	logf("Triggering workflow_dispatch for %s...", repo)

	ref := os.Getenv("GITHUB_REF_NAME")
	if ref == "" {
		ref = "main"
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/backfill.yml/dispatches", repo)

	body, err := json.Marshal(map[string]string{"ref": ref})
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

	loadDotEnv(".env")

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	if supabaseKey == "" {
		supabaseKey = os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	}
	seekKey := os.Getenv("SEEKSTREAMING_KEY")
	ffmpegPath := os.Getenv("FFMPEG_PATH")

	// UPnShare keys: comma-separated list (UPNSHARE_KEYS) supported, plus the
	// singular UPNSHARE_KEY for backward compatibility.
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
	if seekKey == "" && len(upnKeys) == 0 {
		log.Fatal("SEEKSTREAMING_KEY and/or UPNSHARE_KEY(S) must be set")
	}

	// Bootstrap server config (used by server.UpdateRecordingMediaURLs).
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

	// Filter to those missing a thumbnail and/or preview.
	var todo []workItem
	for _, r := range recordings {
		if r.ThumbnailURL != "" && r.PreviewURL != "" {
			continue
		}
		host, videoID := uploader.MediaHostOf(r.EmbedURL)
		if host == "" || videoID == "" {
			// No embed URL → cannot know which host to query. Skip but count.
			if r.ThumbnailURL == "" || r.PreviewURL == "" {
				atomic.AddInt64(&cntSkipped, 1)
			}
			continue
		}
		todo = append(todo, workItem{rec: r, host: host, videoID: videoID})
	}
	logf("Missing thumbnail and/or preview (with resolvable host): %d", len(todo))
	totalPending := len(todo)

	// Split work across a matrix: each shard handles every Nth recording so the
	// load is evenly distributed and pending rows get retried in parallel.
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

			didWork := processOne(item, seekKey, upnKeys, *flagDryRun, *flagThumbOnly)

			// Only throttle after a real fetch/write. Rows that are "not ready"
			// on the host do no work and carry no rate-limit risk, so we move
			// on to the next recording immediately instead of idling 2 minutes.
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
					processOne(item, seekKey, upnKeys, *flagDryRun, *flagThumbOnly)
				}
			}()
		}
		for _, item := range todo {
			work <- item
		}
		close(work)
		wg.Wait()
	}

	// ── Trigger Next Run ─────────────────────────────────────────────────────
	if durationExceeded && *flagTrigger {
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

	// ── Report ─────────────────────────────────────────────────────────────────
	elapsed := time.Since(start).Round(time.Second)
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("              BACKFILL COMPLETE")
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  Elapsed:              %v\n", elapsed)
	fmt.Printf("  Total processed:      %d\n", atomic.LoadInt64(&cntTotal))
	fmt.Printf("  ✓ Thumbnails fixed:   %d\n", atomic.LoadInt64(&cntThumb))
	fmt.Printf("  ✓ Previews fixed:     %d\n", atomic.LoadInt64(&cntPreview))
	fmt.Printf("  ⏳ Not ready yet:      %d\n", atomic.LoadInt64(&cntNotReady))
	fmt.Printf("  ✗ Failed:             %d\n", atomic.LoadInt64(&cntFailed))
	fmt.Printf("  ⏭ Already complete:   %d\n", atomic.LoadInt64(&cntSkipped))
	fmt.Println("═══════════════════════════════════════════════════")
}
