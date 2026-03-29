// Package downloader handles HTTP downloads with retry, rate limiting, and
// atomic writes.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

// Downloader performs HTTP downloads with configurable retry logic, shared
// rate limiting, and atomic writes (temp-file + rename).
type Downloader struct {
	cfg       *config.Config
	client    *http.Client
	limiter   *ratelimit.Limiter        // may be nil (unlimited)
	backoffFn func(attempt int) time.Duration // injectable for testing
}

// Option configures a Downloader.
type Option func(*Downloader)

// WithLimiter attaches a shared rate limiter. When a 429 Retry-After is
// received, the limiter is paused so all concurrent workers wait together.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(d *Downloader) { d.limiter = l }
}

// WithBackoffFn replaces the default exponential-backoff function.
// Inject a zero-duration function in tests to avoid real sleeps.
func WithBackoffFn(fn func(attempt int) time.Duration) Option {
	return func(d *Downloader) { d.backoffFn = fn }
}

// WithHTTPClient replaces the internal HTTP client. Used in tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(d *Downloader) { d.client = hc }
}

// New creates a new Downloader.
func New(cfg *config.Config, opts ...Option) *Downloader {
	d := &Downloader{
		cfg:       cfg,
		client:    &http.Client{Timeout: cfg.Timeout},
		backoffFn: defaultBackoff,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// defaultBackoff is exponential with ±25% jitter: 2 s, 4 s, 8 s, 16 s, 32 s (cap).
func defaultBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	exp := attempt - 1
	if exp > 4 {
		exp = 4 // base cap 32 s
	}
	base := time.Duration(1<<uint(exp)) * 2 * time.Second // 2,4,8,16,32
	// Jitter: ±25% of base
	half := int64(base / 4)
	if half < 1 {
		half = 1
	}
	jitter := time.Duration(rand.Int63n(2*half)) - time.Duration(half)
	d := base + jitter
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Download fetches archiveURL and writes the content atomically to destPath.
// Parent directories are created as needed.
// It honours the shared rate limiter on every attempt and interprets
// Retry-After headers from 429 responses, pausing all workers accordingly.
func (d *Downloader) Download(ctx context.Context, archiveURL, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
		// Backoff before retries (not before the first attempt).
		if attempt > 0 {
			wait := d.backoffFn(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		// Acquire rate-limiter token before every request (including first).
		if d.limiter != nil {
			if err := d.limiter.Wait(ctx); err != nil {
				return err
			}
		}

		err := d.downloadOnce(ctx, archiveURL, destPath)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			break
		}
	}
	return fmt.Errorf("download failed after %d attempt(s): %w", d.cfg.Retries+1, lastErr)
}

func (d *Downloader) downloadOnce(ctx context.Context, archiveURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")
	// Do NOT set Accept-Encoding — Go's transport adds gzip automatically
	// and decompresses the response body transparently.

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Handle rate-limit response: parse Retry-After and pause all workers.
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := ratelimit.ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second)
		if d.limiter != nil {
			d.limiter.SetPause(wait)
		}
		fmt.Fprintf(os.Stderr,"[WAIT] download rate-limited — pausing %.0fs (Retry-After)\n", wait.Seconds())
		return &httpStatusError{code: resp.StatusCode}
	}

	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{code: resp.StatusCode}
	}

	// Write to a sibling .part file; rename atomically on success.
	tmp := destPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()

	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write content: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", closeErr)
	}

	if err := os.Rename(tmp, destPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename to final path: %w", err)
	}
	return nil
}

// httpStatusError carries an HTTP status code so isRetryable can make
// decisions without inspecting error strings.
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// isRetryable returns true for errors that are worth retrying.
func isRetryable(err error) bool {
	var hse *httpStatusError
	if errors.As(err, &hse) {
		// 429 (rate limit) and 5xx (server errors) are transient.
		// 4xx other than 429 (e.g. 404, 403, 410) are permanent — do not retry.
		return hse.code == http.StatusTooManyRequests ||
			(hse.code >= 500 && hse.code < 600)
	}
	// Network / transport errors (timeouts, connection resets) are retryable.
	return true
}
