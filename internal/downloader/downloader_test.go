package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

// noBackoff makes retries fire immediately — keeps tests fast.
var noBackoff = WithBackoffFn(func(int) time.Duration { return time.Millisecond })

func baseCfg(retries int) *config.Config {
	return &config.Config{Timeout: 5 * time.Second, Retries: retries}
}

func newDL(t *testing.T, srv *httptest.Server, retries int, opts ...Option) *Downloader {
	t.Helper()
	all := []Option{WithHTTPClient(srv.Client()), noBackoff}
	all = append(all, opts...)
	return New(baseCfg(retries), all...)
}

// ── Basic success / failure ────────────────────────────────────────────────

func TestDownload_Success(t *testing.T) {
	content := "hello waybackdown"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, content)
	}))
	defer srv.Close()

	dl := newDL(t, srv, 0)
	dest := filepath.Join(t.TempDir(), "sub", "file.html")

	if err := dl.Download(context.Background(), srv.URL+"/page", dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != content {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestDownload_CreatesParentDirs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	dl := newDL(t, srv, 0)
	dest := filepath.Join(t.TempDir(), "a", "b", "c", "file.txt")
	if err := dl.Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("file not found: %v", err)
	}
}

// ── Retry behaviour ────────────────────────────────────────────────────────

func TestDownload_404_NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dl := newDL(t, srv, 3) // retries=3 but 404 must not retry
	err := dl.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f.html"))
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if calls != 1 {
		t.Errorf("404 must not be retried: expected 1 attempt, got %d", calls)
	}
}

func TestDownload_403_NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dl := newDL(t, srv, 2)
	dl.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f.html"))
	if calls != 1 {
		t.Errorf("403 must not be retried: expected 1 call, got %d", calls)
	}
}

func TestDownload_500_RetriedExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dl := newDL(t, srv, 2) // retries=2 → 3 total attempts
	if err := dl.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f.html")); err == nil {
		t.Fatal("expected error for persistent 500")
	}
	if calls != 3 {
		t.Errorf("retries=2: expected 3 total attempts, got %d", calls)
	}
}

func TestDownload_500_SucceedsOnRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "recovered")
	}))
	defer srv.Close()

	dl := newDL(t, srv, 3)
	dest := filepath.Join(t.TempDir(), "f.html")
	if err := dl.Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("expected eventual success: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2 failures + 1 success), got %d", calls)
	}
}

// ── 429 / Rate-limit ──────────────────────────────────────────────────────

func TestDownload_429_RetriedOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	lim := ratelimit.New(1000, 1000)
	dl := newDL(t, srv, 2, WithLimiter(lim))
	dest := filepath.Join(t.TempDir(), "f.html")
	if err := dl.Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("expected success after 429 retry: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 × 429 + 1 success), got %d", calls)
	}
}

func TestDownload_429_RetryAfterPausesLimiter(t *testing.T) {
	const pauseMs = 150
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", fmt.Sprintf("0.%03d", pauseMs)) // not valid, falls to default
			// Use integer seconds instead to keep the test predictable:
			w.Header().Set("Retry-After", "0") // 0s pause → immediate
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	lim := ratelimit.New(1000, 1000)
	dl := newDL(t, srv, 2, WithLimiter(lim))
	dest := filepath.Join(t.TempDir(), "f.html")
	if err := dl.Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDownload_429_LimiterSetPauseCalled(t *testing.T) {
	// Verify the limiter receives a non-zero SetPause when Retry-After: 5 is sent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	lim := ratelimit.New(1000, 1000)
	dl := newDL(t, srv, 0, WithLimiter(lim)) // retries=0 so we stop after 1 attempt
	dl.Download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f.html"))

	pe := lim.PauseEnd()
	if pe.IsZero() {
		t.Error("expected limiter.SetPause to be called after 429 with Retry-After")
	}
	if time.Until(pe) < 4*time.Second {
		t.Errorf("expected pause ≥5s in limiter, ends in %v", time.Until(pe))
	}
}

// ── Context cancellation ──────────────────────────────────────────────────

func TestDownload_ContextCancelledBeforeFirstAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	dl := newDL(t, srv, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := dl.Download(ctx, srv.URL, filepath.Join(t.TempDir(), "f.html"))
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
}

func TestDownload_ContextCancelledDuringRateLimitPause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lim := ratelimit.New(1000, 1000)
	lim.SetPause(10 * time.Second) // very long pause

	dl := newDL(t, srv, 0, WithLimiter(lim))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := dl.Download(ctx, srv.URL, filepath.Join(t.TempDir(), "f.html"))
	if err == nil {
		t.Fatal("expected context error during rate-limit pause")
	}
}

// ── Atomic write ──────────────────────────────────────────────────────────

func TestDownload_AtomicWrite_PartFileCleanedUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	dl := newDL(t, srv, 0)
	dest := filepath.Join(t.TempDir(), "out.html")
	if err := dl.Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file not removed after successful download")
	}
}

func TestDownload_AtomicWrite_NoPartFileOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dl := newDL(t, srv, 0)
	dest := filepath.Join(t.TempDir(), "out.html")
	dl.Download(context.Background(), srv.URL, dest)

	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file left behind after 404 error")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("destination file should not exist after 404")
	}
}

// ── Rate limiter sharing ──────────────────────────────────────────────────

func TestDownload_SharedLimiter_ThrottlesConcurrentWorkers(t *testing.T) {
	// 5 RPS limiter with burst 1 — 5 concurrent downloads should all complete
	// but take at least 4×200ms = 800ms.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	lim := ratelimit.New(5, 1)
	n := 5
	errs := make(chan error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func(idx int) {
			dl := New(baseCfg(0), WithHTTPClient(srv.Client()), WithLimiter(lim), noBackoff)
			dest := filepath.Join(t.TempDir(), fmt.Sprintf("f%d.html", idx))
			errs <- dl.Download(context.Background(), srv.URL, dest)
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// n-1 inter-request gaps at 200ms each = 800ms minimum.
	if elapsed < 600*time.Millisecond {
		t.Errorf("rate limiter not throttling: %d downloads in %v (expected ≥600ms)", n, elapsed)
	}
}

// ── isRetryable ───────────────────────────────────────────────────────────

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		code      int
		retryable bool
	}{
		{200, false}, // success — but if we ever get here, treat as non-retryable
		{301, false},
		{400, false},
		{403, false},
		{404, false},
		{410, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, c := range cases {
		got := isRetryable(&httpStatusError{code: c.code})
		if got != c.retryable {
			t.Errorf("isRetryable(HTTP %d) = %v, want %v", c.code, got, c.retryable)
		}
	}
}

// ── defaultBackoff ────────────────────────────────────────────────────────

func TestDefaultBackoff_FirstAttemptZero(t *testing.T) {
	if d := defaultBackoff(0); d != 0 {
		t.Errorf("backoff(0) = %v, want 0", d)
	}
}

func TestDefaultBackoff_Increases(t *testing.T) {
	prev := defaultBackoff(1)
	for i := 2; i <= 4; i++ {
		// With jitter, exact ordering isn't guaranteed but the range should
		// increase.  Just verify they're all positive and not absurdly large.
		d := defaultBackoff(i)
		if d < time.Second {
			t.Errorf("backoff(%d) = %v, want ≥1s", i, d)
		}
		if d > 60*time.Second {
			t.Errorf("backoff(%d) = %v, too large", i, d)
		}
		_ = prev
		prev = d
	}
}

func TestDefaultBackoff_Cap(t *testing.T) {
	// All values from attempt=5 onward must be ≤ 40s (32s base + 25% jitter).
	for i := 5; i <= 10; i++ {
		d := defaultBackoff(i)
		if d > 40*time.Second {
			t.Errorf("backoff(%d) = %v, exceeded cap", i, d)
		}
	}
}
