//go:build ignore

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/teacat/chaturbate-dvr/channel"
)

func main() {
	videoDir := "videos"

	entries, err := os.ReadDir(videoDir)
	if err != nil {
		log.Fatalf("read dir %s: %v", videoDir, err)
	}

	videoExts := map[string]bool{".mp4": true, ".mkv": true, ".ts": true}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !videoExts[ext] {
			continue
		}

		videoPath := filepath.Join(videoDir, entry.Name())
		fi, err := entry.Info()
		if err != nil {
			log.Printf("SKIP %s: stat error: %v", entry.Name(), err)
			continue
		}
		if fi.Size() < 100*1024 {
			log.Printf("SKIP %s: too small (%d bytes)", entry.Name(), fi.Size())
			continue
		}

		log.Printf("=== Processing: %s (%s) ===", entry.Name(), formatSize(fi.Size()))

		thumbURL, spriteURL, previewURL := channel.GenerateThumbnailForFile(videoPath)

		fmt.Println()
		fmt.Printf("  Video:      %s\n", entry.Name())
		fmt.Printf("  Thumbnail:  %s\n", orNone(thumbURL))
		fmt.Printf("  Sprite:     %s\n", orNone(spriteURL))
		fmt.Printf("  Preview:    %s\n", orNone(previewURL))
		fmt.Println()
	}

	log.Println("Done.")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
}
