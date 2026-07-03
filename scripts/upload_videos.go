//go:build ignore

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teacat/chaturbate-dvr/channel"
	"github.com/teacat/chaturbate-dvr/entity"
	"github.com/teacat/chaturbate-dvr/server"
	"github.com/teacat/chaturbate-dvr/uploader"
)

// Simple logger that satisfies uploader.Logger
type scriptLogger struct{}

func (s *scriptLogger) Info(format string, a ...any) {
	log.Printf("INFO: "+format, a...)
}

func (s *scriptLogger) Error(format string, a ...any) {
	log.Printf("ERROR: "+format, a...)
}

var allowedExt = map[string]bool{
	".mp4":  true,
	".mkv":  true,
	".webm": true,
	".ts":   true,
	".avi":  true,
	".mov":  true,
	".flv":  true,
	".m4v":  true,
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// loadDotEnv parses a very small subset of .env (KEY=VALUE) and sets env vars.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
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
		// strip optional surrounding quotes
		v = strings.Trim(v, "'\"")
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return s.Err()
}

func extractUsername(filename string) string {
	if idx := strings.Index(filename, "_20"); idx > 0 {
		return filename[:idx]
	}
	return ""
}

func main() {
	moveAfter := flag.Bool("move", false, "move successfully uploaded files into videos/completed")
	baseDir := flag.String("dir", "videos", "base videos directory to scan")
	flag.Parse()

	// Load .env if present
	if _, err := os.Stat(".env"); err == nil {
		if err := loadDotEnv(".env"); err != nil {
			log.Printf("warning: failed to load .env: %v", err)
		}
	}

	// Build server.Config from env
	server.Config = &entity.Config{
		SupabaseURL:      os.Getenv("SUPABASE_URL"),
		SupabaseAPIKey:   os.Getenv("SUPABASE_API_KEY"),
		VoeSXAPIKey:      os.Getenv("VOESX_API_KEY"),
		StreamtapeLogin:  os.Getenv("STREAMTAPE_LOGIN"),
		StreamtapeKey:    os.Getenv("STREAMTAPE_KEY"),
		MixdropEmail:     os.Getenv("MIXDROP_EMAIL"),
		MixdropToken:     firstNonEmpty(os.Getenv("MIXDROP_TOKEN"), os.Getenv("MIXDROP_KEY")),
		SeekStreamingKey: os.Getenv("SEEKSTREAMING_KEY"),
	}

	// Log presence (masked) of uploader credentials to help debugging without leaking secrets.
	if server.Config != nil {
		mask := func(s string) string {
			if s == "" {
				return "<empty>"
			}
			return fmt.Sprintf("<len=%d>", len(s))
		}
		log.Printf("uploader creds: MixdropEmail=%t MixdropToken=%s StreamtapeLogin=%t StreamtapeKey=%s SeekStreaming=%s",
			server.Config.MixdropEmail != "", mask(server.Config.MixdropToken),
			server.Config.StreamtapeLogin != "", mask(server.Config.StreamtapeKey),
			mask(server.Config.SeekStreamingKey),
		)
	}

	dbClient := server.GetDBClient()
	if dbClient == nil {
		log.Printf("warning: Supabase not configured (SUPABASE_URL/SUPABASE_API_KEY). metadata saves will fail")
	} else {
		if err := dbClient.HealthCheck(); err != nil {
			log.Printf("warning: Supabase health check failed: %v", err)
		}
	}

	dirs := []string{*baseDir, filepath.Join(*baseDir, "completed")}
	var files []string
	for _, d := range dirs {
		if _, err := os.Stat(d); os.IsNotExist(err) {
			continue
		}
		_ = filepath.WalkDir(d, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if de.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if allowedExt[ext] {
				files = append(files, path)
			}
			return nil
		})
	}

	if len(files) == 0 {
		log.Println("no video files found in videos or videos/completed")
		return
	}

	log.Printf("found %d video(s)", len(files))

	for _, p := range files {
		log.Printf("processing: %s", p)
		filename := filepath.Base(p)

		// Skip if already in DB
		if dbClient != nil {
			if _, err := dbClient.GetRecording(filename); err == nil {
				log.Printf("skipping %s \u2014 already present in Supabase", filename)
				continue
			}
		}

		// Construct uploader using server.Config
		// Generate thumbnails/sprite and save preview links (requires ffmpeg/ffprobe in PATH).
		thumbURL, spriteURL, previewURL := channel.GenerateThumbnailForFile(p)
		if thumbURL != "" || spriteURL != "" || previewURL != "" {
			if err := server.SavePreviewLinks(filename, thumbURL, spriteURL, previewURL); err != nil {
				log.Printf("warning: failed to save preview links for %s: %v", filename, err)
			} else {
				log.Printf("saved previews for %s", filename)
			}
		}

		upl := uploader.NewMultiHostUploader(
			server.Config.VoeSXAPIKey,
			server.Config.StreamtapeLogin,
			server.Config.StreamtapeKey,
			server.Config.MixdropEmail,
			server.Config.MixdropToken,
			server.Config.SeekStreamingKey,
			&scriptLogger{},
		)

		results := upl.UploadToAll(p)
		success := uploader.GetSuccessfulUploads(results)

		links := map[string]string{}
		var embedURL string
		for _, r := range success {
			links[r.Host] = r.DownloadLink
			if embedURL == "" {
				embedURL = r.DownloadLink
			}
		}

		fi, _ := os.Stat(p)
		var filesize int64
		var timestamp string
		if fi != nil {
			filesize = fi.Size()
			timestamp = fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
		} else {
			timestamp = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		}

		username := extractUsername(filename)

		dur, probeErr := channel.VideoDurationSeconds(p)
		if probeErr != nil {
			log.Printf("could not probe duration for %s: %v", filename, probeErr)
		}
		if err := server.SaveRecordingWithLinks(username, filename, timestamp, "", nil, 0, "", 0, filesize, dur, "", embedURL, thumbURL, spriteURL, "", links); err != nil {
			log.Printf("failed saving metadata for %s: %v", filename, err)
		} else {
			log.Printf("saved metadata for %s (links: %d)", filename, len(links))

			if *moveAfter {
				destDir := filepath.Join(*baseDir, "completed")
				_ = os.MkdirAll(destDir, 0755)
				dest := filepath.Join(destDir, filename)
				if err := os.Rename(p, dest); err != nil {
					log.Printf("failed moving %s -> %s: %v", p, dest, err)
				} else {
					log.Printf("moved %s -> %s", p, dest)
				}
			}
		}
	}

	log.Println("done")
}
