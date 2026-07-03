package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const hashSampleSize = 64 * 1024 // 64 KB from head and tail

// FastFileHash computes a SHA-256 hash of the first 64KB, last 64KB, and file
// size of the given file.  This is fast (O(128KB) I/O) and unique enough for
// deduplication purposes — the probability of collision across two different
// video files is astronomically low.
func FastFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	size := stat.Size()

	h := sha256.New()

	// Read first 64 KB
	head := make([]byte, hashSampleSize)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read head: %w", err)
	}
	h.Write(head[:n])

	// Read last 64 KB (seek near the end)
	if size > hashSampleSize {
		tail := make([]byte, hashSampleSize)
		_, err := f.Seek(-hashSampleSize, io.SeekEnd)
		if err != nil {
			return "", fmt.Errorf("seek tail: %w", err)
		}
		n, err = f.Read(tail)
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read tail: %w", err)
		}
		h.Write(tail[:n])
	}

	// Include file size so different-sized files with identical head/tail
	// content yield different hashes (unlikely but defensive).
	fmt.Fprintf(h, "%d", size)

	return hex.EncodeToString(h.Sum(nil)), nil
}
