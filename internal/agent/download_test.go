package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testOpts = DownloadOptions{
	MaxAttempts:    4,
	BaseBackoff:    time.Millisecond,
	MaxBackoff:     5 * time.Millisecond,
	AttemptTimeout: 5 * time.Second,
}

func TestDownloadRetriesOn500(t *testing.T) {
	payload := []byte("firmware-image-v2")
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "fw.bin")
	if err := DownloadWithResume(context.Background(), srv.Client(), srv.URL, dest, testOpts); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch: %q", got)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", calls)
	}
}

func TestDownloadResumesAfterMidStreamDisconnect(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	half := len(payload) / 2
	var mu sync.Mutex
	var rangeHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rangeHeaders = append(rangeHeaders, r.Header.Get("Range"))
		first := len(rangeHeaders) == 1
		mu.Unlock()
		if first {
			// Promise the full length, deliver half, kill the connection:
			// the classic flaky-uplink failure the agent must survive.
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:half])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		// Second attempt must arrive as a Range request for the remainder.
		want := fmt.Sprintf("bytes=%d-", half)
		if got := r.Header.Get("Range"); got != want {
			http.Error(w, "expected range "+want+" got "+got, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[half:])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "fw.bin")
	if err := DownloadWithResume(context.Background(), srv.Client(), srv.URL, dest, testOpts); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("resumed payload mismatch: %q", got)
	}
	if len(rangeHeaders) != 2 || rangeHeaders[0] != "" || !strings.HasPrefix(rangeHeaders[1], "bytes=") {
		t.Fatalf("expected full request then range request, got %v", rangeHeaders)
	}
}

func TestDownloadGivesUpAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "fw.bin")
	err := DownloadWithResume(context.Background(), srv.Client(), srv.URL, dest, testOpts)
	if err == nil {
		t.Fatal("expected failure after exhausting attempts")
	}
}

func TestVerifySHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	data := []byte("payload")
	os.WriteFile(p, data, 0o644)
	sum := sha256.Sum256(data)
	if err := VerifySHA256(p, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}
	if err := VerifySHA256(p, strings.Repeat("0", 64)); err == nil {
		t.Fatal("corrupted checksum accepted")
	}
}
