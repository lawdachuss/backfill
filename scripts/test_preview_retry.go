//go:build ignore

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	videoPath := flag.String("video", "", "Path to a video file to test with (searched in videos/ if empty)")
	flag.Parse()

	video := *videoPath
	if video == "" {
		video = findTestVideo("videos")
	}
	if video == "" {
		log.Fatal("no video found in videos/ — pass --video /path/to/video.mp4")
	}

	log.Printf("Using video: %s", video)
	log.Printf("File size: %s", formatSize(fileSize(video)))

	// ── Step 1: Generate a short preview clip via ffmpeg ────────────────
	// Uses the same simple approach as channel_thumbnail.go's fallback path:
	//   ffmpeg -y -ss <seek> -i <video> -t 6 -vf scale=320:-2
	//          -c:v libx264 -preset fast -crf 23 -movflags +faststart -an <output>
	previewPath := video + ".test_preview.mp4"
	defer os.Remove(previewPath)

	log.Println("Generating preview clip (ffmpeg)...")
	start := time.Now()

	// Use ffprobe to get duration for a reasonable seek point
	dur := probeDuration(video)
	seekPos := "00:00:03"
	if dur > 6 {
		seekPos = fmt.Sprintf("%.2f", dur*0.3)
	} else if dur > 0 {
		seekPos = fmt.Sprintf("%.2f", dur*0.5)
	}

	ffmpegArgs := []string{
		"-y",
		"-ss", seekPos,
		"-i", video,
		"-t", "6",
		"-vf", "scale=320:-2:flags=lanczos",
		"-c:v", "libx264",
		"-preset", "fast",
		"-crf", "23",
		"-movflags", "+faststart",
		"-an",
		previewPath,
	}

	cmd := runCmd("ffmpeg", ffmpegArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("ffmpeg failed: %v\n%s", err, string(output))
	}
	genTime := time.Since(start).Round(time.Millisecond)
	log.Printf("ffmpeg completed in %v", genTime)

	// ── Step 2: Immediately probe for the file with retry ───────────────
	// This replicates the waitForPreviewFile() logic added in
	// channel_thumbnail.go: poll fileExists with exponential backoff
	// (50, 100, 200, 400, 800 ms) for up to 5 attempts (~1.5s total).
	log.Println()
	log.Println("Testing waitForPreviewFile retry logic...")

	type attempt struct {
		n       int
		delay   time.Duration
		elapsed time.Duration
		found   bool
	}
	var attempts []attempt
	cumulative := time.Duration(0)

	found := false
	for i := 0; i < 5; i++ {
		delay := time.Duration(50*(1<<i)) * time.Millisecond // 50, 100, 200, 400, 800 ms
		if i > 0 {
			time.Sleep(delay)
		}
		cumulative += delay

		exists := fileExists(previewPath)
		attempts = append(attempts, attempt{
			n:       i + 1,
			delay:   delay,
			elapsed: cumulative,
			found:   exists,
		})
		if exists {
			found = true
			break
		}
	}

	// ── Step 3: Report results ──────────────────────────────────────────
	fmt.Println()
	fmt.Println("=== Results ===")
	fmt.Printf("  Preview file: %s\n", filepath.Base(previewPath))
	fmt.Printf("  ffmpeg time:  %v\n", genTime)
	fmt.Printf("  File exists on first check: %v\n", attempts[0].found)
	fmt.Printf("  File found with retry:      %v\n", found)
	fmt.Println()
	fmt.Println("  Retry attempts:")
	for _, a := range attempts {
		mark := "✗"
		if a.found {
			mark = "✓"
		}
		fmt.Printf("    %s attempt %d: delay=%v (cumulative=%v)\n", mark, a.n, a.delay, a.elapsed)
	}
	fmt.Println()

	if found {
		fi, _ := os.Stat(previewPath)
		if fi != nil {
			fmt.Printf("  Preview file size: %s\n", formatSize(fi.Size()))
		}
		fmt.Println()
		log.Println("PASS: Retry logic successfully found the preview file.")
	} else {
		fmt.Println()
		log.Println("FAIL: Preview file was not found even after 5 retry attempts.")
		log.Println("This indicates the ffmpeg command may not have produced output,")
		log.Println("or the file path is different from expected.")
		os.Exit(1)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// fileExists returns true if path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// probeDuration uses ffprobe to get the duration of a video file in seconds.
// Returns 0 on failure (caller handles this gracefully).
func probeDuration(videoPath string) float64 {
	cmd := runCmd("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var dur float64
	if _, parseErr := fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &dur); parseErr != nil {
		return 0
	}
	return dur
}

// formatSize returns a human-readable file size string.
func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}

func runCmd(prog string, args ...string) *exec.Cmd {
	return exec.Command(prog, args...)
}

// findTestVideo walks the videos/ directory and returns the first real video file.
func findTestVideo(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	videoExts := map[string]bool{".mp4": true, ".mkv": true, ".ts": true}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !videoExts[ext] {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil || fi.Size() < 100*1024 {
			continue
		}
		return path
	}
	return ""
}
