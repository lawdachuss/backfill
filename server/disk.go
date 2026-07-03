package server

import (
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/teacat/chaturbate-dvr/entity"
)

func GetDiskInfo() *entity.DiskInfo {
	// Check the actual recordings directory first (bind mount), then fall back.
	candidates := []string{}

	videoDir := "/usr/src/app/videos"
	if _, err := os.Stat(videoDir); err == nil {
		candidates = append(candidates, videoDir)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if Config != nil && Config.OutputDir != "" {
		candidates = append(candidates, Config.OutputDir)
	}
	if runtime.GOOS == "windows" {
		if sys := os.Getenv("SystemDrive"); sys != "" {
			candidates = append(candidates, sys+"\\")
		}
	}
	candidates = append(candidates, "/")

	for _, path := range candidates {
		if path == "" {
			continue
		}
		total, free, err := getDiskUsage(path)
		if err != nil {
			log.Printf("disk: %s: %v", path, err)
			continue
		}
		if total == 0 {
			log.Printf("disk: %s: total is 0, skipping", path)
			continue
		}
		log.Printf("disk: %s: total=%d free=%d", path, total, free)
		return calculateDiskInfo(total, free)
	}

	log.Printf("disk: all candidates failed")
	return &entity.DiskInfo{
		Total:   "N/A",
		Used:    "N/A",
		Free:    "N/A",
		Percent: 0,
	}
}

func calculateDiskInfo(total, free uint64) *entity.DiskInfo {
	used := total - free
	const div = 1024 * 1024 * 1024
	percent := 0
	if total > 0 {
		percent = int(used * 100 / total)
	}
	return &entity.DiskInfo{
		Total:   fmt.Sprintf("%.1f GB", float64(total)/div),
		Used:    fmt.Sprintf("%.1f GB", float64(used)/div),
		Free:    fmt.Sprintf("%.1f GB", float64(free)/div),
		Percent: percent,
		UsedGB:  float64(used) / div,
		TotalGB: float64(total) / div,
	}
}
