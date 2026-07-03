package uploader

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProgressReaderUsesLocalCallback(t *testing.T) {
	var globalCalls int32
	SetProgressCallback(func(string, int64, int64) {
		atomic.AddInt32(&globalCalls, 1)
	})
	defer ClearProgressCallback()

	var localCalls int32
	var finalCurrent int64
	reader := NewProgressReaderWithCallback(strings.NewReader("abcdef"), 6, "Local", func(_ string, current, _ int64) {
		atomic.AddInt32(&localCalls, 1)
		finalCurrent = current
	})

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("copy progress reader: %v", err)
	}
	if globalCalls != 0 {
		t.Fatalf("global callback was called %d time(s)", globalCalls)
	}
	if localCalls == 0 {
		t.Fatal("local callback was not called")
	}
	if finalCurrent != 6 {
		t.Fatalf("final progress = %d, want 6", finalCurrent)
	}
}

func TestMultiHostUploaderProgressIsInstanceScoped(t *testing.T) {
	u := &MultiHostUploader{
		log: &nilLogger{},
		hosts: map[string]uploaderFunc{
			"TestHost": func(_ string, progress ProgressFunc) (string, error) {
				if progress != nil {
					progress("TestHost", 7, 9)
				}
				return "https://example.test/video", nil
			},
		},
	}

	var gotHost string
	var gotCurrent int64
	var gotTotal int64
	u.SetProgressCallback(func(host string, current, total int64) {
		gotHost = host
		gotCurrent = current
		gotTotal = total
	})

	results := u.UploadSelected("unused.mp4", []string{"TestHost"})
	if len(results) != 1 || results[0].Error != nil {
		t.Fatalf("UploadSelected results = %#v", results)
	}
	if gotHost != "TestHost" || gotCurrent != 7 || gotTotal != 9 {
		t.Fatalf("progress = (%q, %d, %d), want (TestHost, 7, 9)", gotHost, gotCurrent, gotTotal)
	}
}
