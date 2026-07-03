//go:build ignore

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/teacat/chaturbate-dvr/uploader"
)

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
		v = strings.Trim(v, "'\"")
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return s.Err()
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("loading .env: %v", err)
	}

	testFile := "videos/357054_medium.mp4"
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		log.Fatalf("test file not found: %s", testFile)
	}

	// Test Catbox
	log.Println("=== Testing Catbox ===")
	catbox := uploader.NewCatboxUploader()
	if url, err := catbox.Upload(testFile); err != nil {
		log.Printf("Catbox FAILED: %v", err)
	} else {
		log.Printf("Catbox OK: %s", url)
	}

	// Test PixelDrain
	apiKey := os.Getenv("PIXELDRAIN_API_KEY")
	if apiKey != "" {
		log.Println("=== Testing PixelDrain ===")
		pd := uploader.NewPixelDrainUploader(apiKey)
		if url, err := pd.Upload(testFile); err != nil {
			log.Printf("PixelDrain FAILED: %v", err)
		} else {
			log.Printf("PixelDrain OK: %s", url)
		}
	} else {
		log.Println("PixelDrain: no API key, skipping")
	}

	// Test LobFile (requires LOBFILE_API_KEY env var)
	lobfileAPIKey := os.Getenv("LOBFILE_API_KEY")
	if lobfileAPIKey != "" {
		log.Println("=== Testing LobFile ===")
		lf := uploader.NewLobFileUploader(lobfileAPIKey)
		if url, err := lf.Upload(testFile); err != nil {
			log.Printf("LobFile FAILED: %v", err)
		} else {
			log.Printf("LobFile OK: %s", url)
		}
	} else {
		log.Println("LobFile: no API key, skipping")
	}

	fmt.Println("\nDone.")
}
