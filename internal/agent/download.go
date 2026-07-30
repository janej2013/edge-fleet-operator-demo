package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	rand "math/rand/v2"
	"net/http"
	"os"
	"time"
)

// DownloadOptions tunes retry behavior; zero values get sane defaults so
// tests can shrink timeouts without production code paths changing.
type DownloadOptions struct {
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	AttemptTimeout time.Duration
}

func (o DownloadOptions) withDefaults() DownloadOptions {
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 5
	}
	if o.BaseBackoff == 0 {
		o.BaseBackoff = 500 * time.Millisecond
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 15 * time.Second
	}
	if o.AttemptTimeout == 0 {
		o.AttemptTimeout = 2 * time.Minute
	}
	return o
}

// DownloadWithResume fetches url into dest, surviving flaky links.
//
// Edge reality this models: cellular/wifi uplinks drop mid-transfer, and
// firmware images are large relative to bandwidth. Re-downloading from byte 0
// after every drop can make an upgrade *never* finish on a bad link, so on
// retry we keep the partial file and ask for the remainder with a Range
// header. The server is free to ignore Range (plain 200) — then we truncate
// and start over, correct either way.
func DownloadWithResume(ctx context.Context, httpc *http.Client, url, dest string, opts DownloadOptions) error {
	o := opts.withDefaults()
	var lastErr error
	for attempt := 0; attempt < o.MaxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff so a struggling server isn't hammered,
			// plus jitter so a fleet of N devices that lost connectivity
			// together doesn't reconnect as one synchronized stampede.
			backoff := min(o.BaseBackoff<<(attempt-1), o.MaxBackoff)
			sleep := backoff/2 + rand.N(backoff)
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := downloadOnce(ctx, httpc, url, dest, o.AttemptTimeout); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("download failed after %d attempts: %w", o.MaxAttempts, lastErr)
}

func downloadOnce(ctx context.Context, httpc *http.Client, url, dest string, timeout time.Duration) error {
	// Per-attempt deadline: a stalled TCP connection must fail fast enough
	// for the retry/backoff loop to take over, instead of hanging forever.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var offset int64
	if fi, err := os.Stat(dest); err == nil {
		offset = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var f *os.File
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206: server honored Range → append
		f, err = os.OpenFile(dest, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	case http.StatusOK: // 200: full body → discard any partial state
		f, err = os.Create(dest)
	case http.StatusRequestedRangeNotSatisfiable: // 416: our partial is bogus
		os.Remove(dest)
		return fmt.Errorf("server rejected range at offset %d, restarting", offset)
	default:
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if err != nil {
		return err
	}
	defer f.Close()

	// A partial copy is not wasted work: whatever landed on disk is the
	// resume point for the next attempt.
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("transfer interrupted: %w", err)
	}
	return nil
}

// VerifySHA256 streams the file through sha256 and compares digests.
// This is the brick-safety line: nothing unverified is ever flashed.
func VerifySHA256(path, wantHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantHex {
		return fmt.Errorf("checksum mismatch: got %s want %s", got, wantHex)
	}
	return nil
}
