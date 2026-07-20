//go:build ignore

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
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

// ── paginated helpers (fetch ALL recordings/previews, not just 500) ─────────

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

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("=== Sync Preview Images to Recordings ===")

	loadDotEnv(".env")

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY must be set in .env")
	}

	server.Config = &entity.Config{
		SupabaseURL:    supabaseURL,
		SupabaseAPIKey: supabaseKey,
	}

	dbClient := server.GetDBClient()
	if dbClient == nil {
		log.Fatal("could not init database client")
	}
	if err := dbClient.HealthCheck(); err != nil {
		log.Printf("WARN: Supabase health check: %v", err)
	}

	// ── Fetch ALL recordings (paginated) ────────────────────────────────────
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

	// ── Sync: fix recordings table for recordings that have preview images ──
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

	log.Println("\n=== All done! ===")
}
