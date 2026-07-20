//go:build ignore

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// ─── flags (added for pagination + sharding) ────────────────────────────────
var (
	flagShard  = flag.Int("shard", 0, "Zero-based shard index (0..shards-1)")
	flagShards = flag.Int("shards", 1, "Total number of shards")
	flagLimit  = flag.Int("limit", 0, "Max recordings to process (0 = unlimited)")
)

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
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, "\"'")
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

type recordingRow struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	Username     string `json:"username"`
	ThumbnailURL string `json:"thumbnail_url"`
	SpriteURL    string `json:"sprite_url"`
	PreviewURL   string `json:"preview_url"`
}

type uploadLinkRow struct {
	Host string `json:"host"`
	URL  string `json:"url"`
}

type previewRow struct {
	Filename     string `json:"filename"`
	ThumbnailURL string `json:"thumbnail_url"`
	SpriteURL    string `json:"sprite_url"`
	PreviewURL   string `json:"preview_url"`
}

func supabaseGet(path string) ([]byte, error) {
	base := os.Getenv("SUPABASE_URL")
	key := os.Getenv("SUPABASE_API_KEY")
	if base == "" || key == "" {
		return nil, fmt.Errorf("Supabase not configured")
	}
	req, err := http.NewRequest("GET", base+"/rest/v1"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func supabasePatch(path string, body []byte) error {
	base := os.Getenv("SUPABASE_URL")
	key := os.Getenv("SUPABASE_API_KEY")
	if base == "" || key == "" {
		return fmt.Errorf("Supabase not configured")
	}
	req, err := http.NewRequest("PATCH", base+"/rest/v1"+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func supabasePost(path string, body interface{}) error {
	base := os.Getenv("SUPABASE_URL")
	key := os.Getenv("SUPABASE_API_KEY")
	if base == "" || key == "" {
		return fmt.Errorf("Supabase not configured")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", base+"/rest/v1"+path, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ── paginated helpers (added: fetch ALL recordings/previews, not just 500) ──

// paginatedRecordings fetches ALL recordings by paginating 1000 at a time.
func paginatedRecordings() ([]recordingRow, error) {
	var all []recordingRow
	offset := 0
	pageSize := 1000
	for {
		var page []recordingRow
		path := fmt.Sprintf("/recordings?order=timestamp.desc,filename.asc&limit=%d&offset=%d", pageSize, offset)
		data, err := supabaseGet(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return all, nil
}

// paginatedPreviews fetches ALL preview_images by paginating 1000 at a time.
func paginatedPreviews() ([]previewRow, error) {
	var all []previewRow
	offset := 0
	pageSize := 1000
	for {
		var page []previewRow
		path := fmt.Sprintf("/preview_images?limit=%d&offset=%d", pageSize, offset)
		data, err := supabaseGet(path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return all, nil
}

func downloadWithYtDlp(pageURL, workDir, filename string) (string, error) {
	if _, lookErr := exec.LookPath("yt-dlp"); lookErr != nil {
		return "", fmt.Errorf("yt-dlp not found in PATH")
	}
	destPath := filepath.Join(workDir, filename)
	maxAttempts := 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Printf("  downloading (attempt %d/%d) with yt-dlp: %s", attempt, maxAttempts, pageURL)
		cmd := exec.Command("yt-dlp",
			"-o", destPath,
			"--no-playlist",
			"--no-warnings",
			pageURL,
		)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err == nil {
			fi, fiErr := os.Stat(destPath)
			if fiErr == nil && fi.Size() > 0 {
				log.Printf("  downloaded %d bytes to %s", fi.Size(), destPath)
				return destPath, nil
			}
			os.Remove(destPath)
			return "", fmt.Errorf("downloaded file empty or missing")
		}
		if attempt < maxAttempts {
			delay := time.Duration(attempt*10) * time.Second
			log.Printf("  attempt %d failed (%v), retrying in %.0fs...", attempt, err, delay.Seconds())
			time.Sleep(delay)
		} else {
			return "", fmt.Errorf("yt-dlp: %w", err)
		}
	}
	return "", fmt.Errorf("yt-dlp: all attempts failed")
}

func checkFFmpeg() {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("ffmpeg not found in PATH. Thumbnail generation requires ffmpeg.\nInstall it from https://ffmpeg.org/download.html or via your package manager.")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		log.Fatal("ffprobe not found in PATH. Thumbnail generation requires ffprobe.")
	}
	log.Println("ffmpeg/ffprobe found")
}

// ── Local thumbnail generation (replaces missing channel package) ───────────
//
// generateThumbnailFromVideo extracts a single frame via ffmpeg and uploads it
// to Pixhost.to via the existing ThumbnailUploader.  Returns the public URL.

func generateThumbnailFromVideo(videoPath string) (string, error) {
	workDir := filepath.Dir(videoPath)
	stem := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
	thumbJpg := filepath.Join(workDir, stem+"_thumb.jpg")

	// Probe duration to pick a sane seek point (~1/3 into the video)
	durStr := "10"
	if b, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", videoPath).Output(); err == nil {
		durStr = strings.TrimSpace(string(b))
	}

	var seekSec string
	var dur float64
	if _, err := fmt.Sscanf(durStr, "%f", &dur); err == nil && dur > 3 {
		seekSec = fmt.Sprintf("%.0f", dur/3)
	} else {
		seekSec = "1"
	}

	log.Printf("  extracting thumbnail frame at %ss (duration=%ss)", seekSec, durStr)
	cmd := exec.Command("ffmpeg", "-y", "-ss", seekSec, "-i", videoPath,
		"-vframes", "1", "-q:v", "3", thumbJpg)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg thumbnail: %w", err)
	}

	defer os.Remove(thumbJpg)

	log.Printf("  uploading thumbnail to Pixhost.to...")
	thumbUploader := uploader.NewThumbnailUploader("")
	url, err := thumbUploader.Upload(thumbJpg)
	if err != nil {
		return "", fmt.Errorf("Pixhost upload: %w", err)
	}
	log.Printf("  thumbnail uploaded: %s", url)
	return url, nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("=== Generate Missing Previews ===")

	checkFFmpeg()
	loadDotEnv(".env")

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY must be set in .env")
	}

	server.Config = &entity.Config{
		SupabaseURL:    supabaseURL,
		SupabaseAPIKey: supabaseKey,
		GitHubToken:    os.Getenv("GITHUB_ACCESS_TOKEN"),
		GitHubRepo:     os.Getenv("GITHUB_REPO"),
		GitHubBranch:   os.Getenv("GITHUB_BRANCH"),
	}

	dbClient := server.GetDBClient()
	if dbClient == nil {
		log.Fatal("could not init database client")
	}
	if err := dbClient.HealthCheck(); err != nil {
		log.Printf("WARN: Supabase health check: %v", err)
	}

	// ── Fetch ALL recordings (paginated) instead of just 500 ────────────────
	log.Println("Fetching ALL recordings from Supabase (paginated)...")
	recordings, err := paginatedRecordings()
	if err != nil {
		log.Fatalf("failed to fetch recordings: %v", err)
	}
	log.Printf("Found %d recordings total", len(recordings))

	// ── Fetch ALL existing preview images (paginated) ───────────────────────
	log.Println("Fetching ALL existing preview images (paginated)...")
	previews, err := paginatedPreviews()
	if err != nil {
		log.Printf("WARN: could not fetch preview images: %v", err)
		previews = nil
	}
	log.Printf("Found %d existing preview records", len(previews))

	// ── Phase 1: fix recordings table for recordings that already have preview images
	hasPreview := map[string]bool{}
	for _, p := range previews {
		if p.ThumbnailURL != "" || p.SpriteURL != "" {
			hasPreview[p.Filename] = true
		}
	}

	for _, p := range previews {
		if p.ThumbnailURL == "" && p.SpriteURL == "" {
			continue
		}
		for _, r := range recordings {
			if r.Filename == p.Filename {
				if r.ThumbnailURL != "" && r.SpriteURL != "" {
					continue
				}
				log.Printf("  fixing recordings table for %s (thumb=%s, sprite=%s)",
					p.Filename, p.ThumbnailURL, p.SpriteURL)
				if err := server.UpdateRecordingThumbnails(p.Filename, p.ThumbnailURL, p.SpriteURL, p.PreviewURL); err != nil {
					log.Printf("  WARN: failed to update %s: %v", p.Filename, err)
				} else {
					log.Printf("  DONE: updated %s", p.Filename)
				}
				break
			}
		}
	}

	// ── Determine which recordings still need work ──────────────────────────
	var todo []recordingRow
	for _, r := range recordings {
		if hasPreview[r.Filename] {
			continue
		}
		todo = append(todo, r)
	}
	log.Printf("Recordings still needing preview generation: %d", len(todo))

	// ── Shard the work across matrix jobs ───────────────────────────────────
	// Each shard handles every Nth recording so load is evenly distributed.
	if *flagShards > 1 {
		s := *flagShard
		if s < 0 {
			s = 0
		}
		if s >= *flagShards {
			s = *flagShards - 1
		}
		var sliced []recordingRow
		for i := s; i < len(todo); i += *flagShards {
			sliced = append(sliced, todo[i])
		}
		todo = sliced
		log.Printf("Shard %d/%d — processing %d of %d pending recordings", s, *flagShards, len(todo), len(recordings))
	}

	// ── Apply optional limit ────────────────────────────────────────────────
	if *flagLimit > 0 && len(todo) > *flagLimit {
		log.Printf("Limiting to %d recordings (--limit flag)", *flagLimit)
		todo = todo[:*flagLimit]
	}

	// ── Phase 2: download + generate for recordings still missing previews ──
	workDir := filepath.Join("videos", ".preview_work")

	for _, r := range todo {
		if hasPreview[r.Filename] {
			continue
		}

		log.Printf("\nProcessing: %s (username: %s)", r.Filename, r.Username)

		linkData, err := supabaseGet(fmt.Sprintf("/upload_links?recording_id=eq.%s&limit=20", r.ID))
		if err != nil {
			log.Printf("  SKIP: could not fetch upload links: %v", err)
			continue
		}
		var links []uploadLinkRow
		if err := json.Unmarshal(linkData, &links); err != nil {
			log.Printf("  SKIP: could not parse upload links: %v", err)
			continue
		}
		if len(links) == 0 {
			log.Printf("  SKIP: no upload links found")
			continue
		}
		log.Printf("  found %d upload links", len(links))
		for _, l := range links {
			log.Printf("    %s: %s", l.Host, l.URL)
		}

		if err := os.MkdirAll(workDir, 0755); err != nil {
			log.Printf("  SKIP: failed to create work dir: %v", err)
			continue
		}

		var localPath string

		// Try yt-dlp for any host (fallback)
		if localPath == "" {
			for _, l := range links {
				localPath, err = downloadWithYtDlp(l.URL, workDir, r.Filename)
				if err != nil {
					log.Printf("  yt-dlp failed for %s (%s): %v", l.Host, l.URL, err)
					continue
				}
				break
			}
		}

		if localPath == "" {
			log.Printf("  SKIP: could not download from any host")
			continue
		}

		log.Printf("  generating thumbnail for %s...", localPath)
		thumbURL, err := generateThumbnailFromVideo(localPath)
		if err != nil {
			log.Printf("  WARN: thumbnail generation failed: %v", err)
			os.Remove(localPath)
			continue
		}

		log.Printf("  thumbnail URL: %s", thumbURL)

		log.Printf("  updating recordings table with thumbnail...")
		if err := server.UpdateRecordingThumbnails(r.Filename, thumbURL, "", ""); err != nil {
			log.Printf("  WARN: UpdateRecordingThumbnails failed: %v", err)
		}

		log.Printf("  saving to preview_images table...")
		if err := server.SavePreviewLinks(r.Filename, thumbURL, "", ""); err != nil {
			log.Printf("  WARN: SavePreviewLinks failed: %v", err)
		}

		os.Remove(localPath)
		log.Printf("  DONE: %s", r.Filename)
	}

	log.Println("\n=== All done! ===")
}
