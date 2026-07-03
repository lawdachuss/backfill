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

	apiKey := os.Getenv("PIXELDRAIN_API_KEY")
	if apiKey == "" {
		log.Fatal("PIXELDRAIN_API_KEY not set")
	}

	pd := uploader.NewPixelDrainUploader(apiKey)
	url, err := pd.Upload("videos/357054_medium.mp4")
	if err != nil {
		log.Fatalf("PixelDrain FAILED: %v", err)
	}
	fmt.Println("PixelDrain URL:", url)
}
