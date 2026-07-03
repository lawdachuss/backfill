package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	supabaseURL    string
	supabaseAPIKey string
	httpClient     = &http.Client{Timeout: 60 * time.Second}
)

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func main() {
	loadEnv(".env")
	supabaseURL = os.Getenv("SUPABASE_URL")
	supabaseAPIKey = os.Getenv("SUPABASE_API_KEY")

	maxDur := flag.Int("max-duration", 1200, "Delete recordings shorter than this (seconds)")
	deleteLocal := flag.Bool("delete-local", false, "Also remove the local video file from disk")
	dryRun := flag.Bool("dry-run", true, "Only list matching recordings, do not delete")
	flag.Parse()

	if supabaseURL == "" || supabaseAPIKey == "" {
		fmt.Println("ERROR: SUPABASE_URL and SUPABASE_API_KEY must be set in environment or .env")
		os.Exit(1)
	}

	recordings, err := fetchShortRecordings(*maxDur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// Filter out zero-duration (unset) recordings
	var toDelete []recording
	for _, r := range recordings {
		if r.Duration > 0 {
			toDelete = append(toDelete, r)
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("No short recordings found.")
		return
	}

	fmt.Printf("Found %d recording(s) with duration <= %ds and > 0:\n", len(toDelete), *maxDur)
	for _, r := range toDelete {
		fmt.Printf("  %-50s  %.1fs  %s\n", r.Filename, r.Duration, r.Timestamp)
	}

	if *dryRun {
		fmt.Println("\nDry-run mode — nothing deleted. Pass -dry-run=false to actually delete.")
		return
	}

	fmt.Println("\nDeleting...")
	var deleted, errors int
	for _, r := range toDelete {
		if *deleteLocal {
			removeLocalFile(r.Filename)
		}
		if err := deleteRecordingCompletely(r); err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL: %s — %v\n", r.Filename, err)
			errors++
		} else {
			fmt.Printf("  DELETED: %s (%.1fs)\n", r.Filename, r.Duration)
			deleted++
		}
	}

	fmt.Printf("\nDone: %d deleted, %d errors\n", deleted, errors)
}

type recording struct {
	ID        string  `json:"id"`
	Filename  string  `json:"filename"`
	Duration  float64 `json:"duration"`
	Timestamp string  `json:"timestamp"`
}

func supabaseRequest(method, path string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, supabaseURL+"/rest/v1"+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("apikey", supabaseAPIKey)
	req.Header.Set("Authorization", "Bearer "+supabaseAPIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")
	}
	return httpClient.Do(req)
}

func fetchShortRecordings(maxDur int) ([]recording, error) {
	path := fmt.Sprintf("/recordings?duration=lte.%d&order=timestamp.desc&limit=50000", maxDur)
	resp, err := supabaseRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	var recs []recording
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return recs, nil
}

func deleteRecordingCompletely(r recording) error {
	// Delete upload links
	if r.ID != "" {
		path := fmt.Sprintf("/upload_links?recording_id=eq.%s", url.QueryEscape(r.ID))
		resp, err := supabaseRequest("DELETE", path, nil)
		if err != nil {
			return fmt.Errorf("delete upload links: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("delete upload links HTTP %d: %s", resp.StatusCode, string(b))
		}
	}

	// Delete preview images
	path := fmt.Sprintf("/preview_images?filename=eq.%s", url.QueryEscape(r.Filename))
	resp, err := supabaseRequest("DELETE", path, nil)
	if err != nil {
		return fmt.Errorf("delete preview images: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete preview images HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Delete recording
	path = fmt.Sprintf("/recordings?filename=eq.%s", url.QueryEscape(r.Filename))
	resp, err = supabaseRequest("DELETE", path, nil)
	if err != nil {
		return fmt.Errorf("delete recording: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete recording HTTP %d: %s", resp.StatusCode, string(b))
	}

	return nil
}

func removeLocalFile(filename string) {
	dirs := []string{"videos"}
	if d := os.Getenv("OUTPUT_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	for _, dir := range dirs {
		p := dir + string(os.PathSeparator) + filename
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if err := os.Remove(p); err != nil {
				fmt.Fprintf(os.Stderr, "  WARN: failed to remove %s: %v\n", p, err)
			} else {
				fmt.Printf("  REMOVED: %s\n", p)
			}
			return
		}
	}
}
