//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	supabaseURL    string
	supabaseAPIKey string
	httpClient     = &http.Client{Timeout: 120 * time.Second}
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

func countMatches(path string) (int, error) {
	resp, err := supabaseRequest("GET", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	var rows []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	return len(rows), nil
}

func clearPreviews(table string, filter string) (int, error) {
	path := fmt.Sprintf("/%s?%s", table, filter)
	body := []byte(`{"preview_url": ""}`)
	resp, err := supabaseRequest("PATCH", path, body)
	if err != nil {
		return 0, fmt.Errorf("patch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	// Count affected rows from response
	var result []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
		return len(result), nil
	}
	return -1, nil
}

func main() {
	loadEnv(".env")
	supabaseURL = os.Getenv("SUPABASE_URL")
	supabaseAPIKey = os.Getenv("SUPABASE_API_KEY")

	if supabaseURL == "" || supabaseAPIKey == "" {
		fmt.Println("ERROR: SUPABASE_URL and SUPABASE_API_KEY must be set in environment or .env")
		os.Exit(1)
	}

	gifFilter := "preview_url=ilike.*.gif&select=filename,preview_url"

	// ---- recordings table ----
	fmt.Println("=== recordings table ===")
	recCount, err := countMatches("/recordings?" + gifFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR querying recordings: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Found %d recording(s) with GIF preview_url\n", recCount)
	if recCount > 0 {
		// Use batch PATCH with filter
		body := []byte(`{"preview_url": ""}`)
		resp, err := supabaseRequest("PATCH", "/recordings?preview_url=ilike.*.gif", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR patching recordings: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "ERROR patching recordings: HTTP %d: %s\n", resp.StatusCode, string(b))
			os.Exit(1)
		}
		fmt.Printf("  Cleared %d GIF preview_url(s) in recordings\n", recCount)
	}

	// ---- preview_images table ----
	fmt.Println("=== preview_images table ===")
	piCount, err := countMatches("/preview_images?" + gifFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR querying preview_images: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Found %d preview_image(s) with GIF preview_url\n", piCount)
	if piCount > 0 {
		body := []byte(`{"preview_url": ""}`)
		resp, err := supabaseRequest("PATCH", "/preview_images?preview_url=ilike.*.gif", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR patching preview_images: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "ERROR patching preview_images: HTTP %d: %s\n", resp.StatusCode, string(b))
			os.Exit(1)
		}
		fmt.Printf("  Cleared %d GIF preview_url(s) in preview_images\n", piCount)
	}

	// ---- pipeline_states table ----
	fmt.Println("=== pipeline_states table ===")
	psFilter := "preview_url=ilike.*.gif&select=file_hash,preview_url"
	psCount, err := countMatches("/pipeline_states?" + psFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR querying pipeline_states: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Found %d pipeline_state(s) with GIF preview_url\n", psCount)
	if psCount > 0 {
		body := []byte(`{"preview_url": ""}`)
		resp, err := supabaseRequest("PATCH", "/pipeline_states?preview_url=ilike.*.gif", body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR patching pipeline_states: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(os.Stderr, "ERROR patching pipeline_states: HTTP %d: %s\n", resp.StatusCode, string(b))
			os.Exit(1)
		}
		fmt.Printf("  Cleared %d GIF preview_url(s) in pipeline_states\n", psCount)
	}

	fmt.Println("\nDone.")
}
