package wayback

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

// ── Test helpers ──────────────────────────────────────────────────────────

func testCfg(mode string, retries int) *config.Config {
	return &config.Config{
		Mode:         mode,
		Timeout:      3 * time.Second,
		Retries:      retries,
		StatusFilter: "200",
	}
}

// newTestClient creates a Client wired to the given test server.
// The CDX endpoint is pointed at srv.URL so all CDX requests go there.
func newTestClient(cfg *config.Config, srv *httptest.Server, opts ...Option) *Client {
	all := []Option{
		WithCDXEndpoint(srv.URL),
		WithHTTPClient(srv.Client()),
	}
	return New(cfg, append(all, opts...)...)
}

// cdxRows encodes a slice of header+data rows as the CDX JSON format.
func cdxRows(rows [][]string) []byte {
	b, _ := json.Marshal(rows)
	return b
}

// serveCDX is a convenience handler that writes a valid CDX JSON response.
func serveCDX(rows [][]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(cdxRows(rows))
	}
}

// ── parseCDX ──────────────────────────────────────────────────────────────

func TestParseCDX_Normal(t *testing.T) {
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20230101120000", "200", "text/html", "https://example.com/", "AAAA"},
		{"20230601120000", "200", "text/html", "https://example.com/", "BBBB"},
	}
	snaps, err := parseCDX(cdxRows(rows), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}
	if snaps[0].Digest != "AAAA" {
		t.Errorf("first digest = %q, want AAAA", snaps[0].Digest)
	}
	want := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	if !snaps[0].Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", snaps[0].Timestamp, want)
	}
	if snaps[0].StatusCode != "200" {
		t.Errorf("status = %q, want 200", snaps[0].StatusCode)
	}
}

func TestParseCDX_Empty(t *testing.T) {
	snaps, err := parseCDX([]byte("[]"), "https://example.com/")
	if err != nil || snaps != nil {
		t.Errorf("empty body: got (%v, %v), want (nil, nil)", snaps, err)
	}
}

func TestParseCDX_Blank(t *testing.T) {
	snaps, err := parseCDX([]byte("  "), "https://example.com/")
	if err != nil || snaps != nil {
		t.Errorf("blank body: got (%v, %v), want (nil, nil)", snaps, err)
	}
}

func TestParseCDX_HeaderOnly(t *testing.T) {
	rows := [][]string{{"timestamp", "statuscode", "mimetype", "original", "digest"}}
	snaps, err := parseCDX(cdxRows(rows), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Errorf("header-only: expected 0 snapshots, got %d", len(snaps))
	}
}

func TestParseCDX_MalformedTimestampSkipped(t *testing.T) {
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"INVALID_TS", "200", "text/html", "https://example.com/", "AAAA"},
		{"20230601120000", "200", "text/html", "https://example.com/", "BBBB"},
	}
	snaps, err := parseCDX(cdxRows(rows), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 valid snapshot, got %d", len(snaps))
	}
}

func TestParseCDX_MissingOriginalFallsBackToInputURL(t *testing.T) {
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20230601120000", "200", "text/html", "", "AAAA"},
	}
	snaps, _ := parseCDX(cdxRows(rows), "https://fallback.com/")
	if len(snaps) != 1 {
		t.Fatal("expected 1 snapshot")
	}
	if snaps[0].OriginalURL != "https://fallback.com/" {
		t.Errorf("OriginalURL = %q, want fallback URL", snaps[0].OriginalURL)
	}
}

func TestParseCDX_InvalidJSON(t *testing.T) {
	_, err := parseCDX([]byte(`not json`), "https://example.com/")
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestParseCDX_FieldOrderIndependent(t *testing.T) {
	// Fields in a different order from the default — parseCDX uses the header
	// row to map names to indices so order must not matter.
	rows := [][]string{
		{"digest", "original", "statuscode", "timestamp", "mimetype"},
		{"ZZZZ", "https://example.com/", "200", "20230601120000", "text/html"},
	}
	snaps, err := parseCDX(cdxRows(rows), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].Digest != "ZZZZ" {
		t.Errorf("Digest = %q, want ZZZZ", snaps[0].Digest)
	}
}

// ── ArchiveURL format ─────────────────────────────────────────────────────

func TestParseCDX_ArchiveURLFormat(t *testing.T) {
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20230601120000", "200", "text/html", "https://example.com/", "AAAA"},
	}
	snaps, _ := parseCDX(cdxRows(rows), "https://example.com/")
	want := "https://web.archive.org/web/20230601120000id_/https://example.com/"
	if snaps[0].ArchiveURL != want {
		t.Errorf("ArchiveURL = %q\n           want %q", snaps[0].ArchiveURL, want)
	}
}

// ── buildCDXURL ───────────────────────────────────────────────────────────

func TestBuildCDXURL_OldestLimit1(t *testing.T) {
	c := New(testCfg(config.ModeOldest, 0))
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "limit=1") {
		t.Errorf("oldest mode: expected limit=1 in %q", u)
	}
}

func TestBuildCDXURL_NewestLimitMinus1(t *testing.T) {
	c := New(testCfg(config.ModeNewest, 0))
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "limit=-1") {
		t.Errorf("newest mode: expected limit=-1 in %q", u)
	}
}

func TestBuildCDXURL_AllCollapseDigest(t *testing.T) {
	c := New(testCfg(config.ModeAll, 0))
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "collapse=digest") {
		t.Errorf("all mode: expected collapse=digest in %q", u)
	}
}

func TestBuildCDXURL_AllMaxSnapshots(t *testing.T) {
	cfg := testCfg(config.ModeAll, 0)
	cfg.MaxSnapshots = 50
	c := New(cfg)
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "limit=50") {
		t.Errorf("all+max mode: expected limit=50 in %q", u)
	}
}

func TestBuildCDXURL_StatusFilter(t *testing.T) {
	c := New(testCfg(config.ModeNewest, 0)) // StatusFilter = "200"
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "filter=") {
		t.Errorf("expected filter param in %q", u)
	}
}

func TestBuildCDXURL_NoStatusFilter(t *testing.T) {
	cfg := testCfg(config.ModeNewest, 0)
	cfg.StatusFilter = ""
	c := New(cfg)
	u := c.buildCDXURL("https://example.com")
	if contains(u, "filter=") {
		t.Errorf("empty StatusFilter should not add filter param, got %q", u)
	}
}

func TestBuildCDXURL_UsesConfiguredEndpoint(t *testing.T) {
	c := New(testCfg(config.ModeNewest, 0), WithCDXEndpoint("http://localhost:9999"))
	u := c.buildCDXURL("https://example.com")
	if !contains(u, "localhost:9999") {
		t.Errorf("expected custom endpoint in URL, got %q", u)
	}
}

// ── FetchSnapshots via test server ────────────────────────────────────────

func TestFetchSnapshots_Success(t *testing.T) {
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20230601120000", "200", "text/html", "https://example.com/", "AAAA"},
	}
	srv := httptest.NewServer(serveCDX(rows))
	t.Cleanup(srv.Close)

	c := newTestClient(testCfg(config.ModeNewest, 0), srv)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(snaps))
	}
}

func TestFetchSnapshots_NoResults_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(testCfg(config.ModeNewest, 0), srv)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snaps != nil {
		t.Errorf("expected nil snapshots, got %v", snaps)
	}
}

func TestFetchSnapshots_HTTP500_ReturnsError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// retries=0 → exactly 1 attempt
	c := newTestClient(testCfg(config.ModeNewest, 0), srv)
	_, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err == nil {
		t.Fatal("expected error from CDX 500, got nil")
	}
	// Verifies the test server is actually used (not the real CDX endpoint).
	if calls != 1 {
		t.Errorf("expected 1 CDX call, got %d (test server may not be wired correctly)", calls)
	}
}

func TestFetchSnapshots_HTTP500_Retried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Third attempt succeeds.
		rows := [][]string{
			{"timestamp", "statuscode", "mimetype", "original", "digest"},
			{"20230601120000", "200", "text/html", "https://example.com/", "AAAA"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cdxRows(rows))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(testCfg(config.ModeNewest, 3), srv)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(snaps))
	}
	if calls != 3 {
		t.Errorf("expected 3 CDX calls (2 failures + 1 success), got %d", calls)
	}
}

func TestFetchSnapshots_HTTP404_NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(testCfg(config.ModeNewest, 3), srv)
	_, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if calls != 1 {
		t.Errorf("404 must not be retried: expected 1 call, got %d", calls)
	}
}

// ── 429 / Retry-After handling ────────────────────────────────────────────

func TestFetchSnapshots_CDX429_LimiterPaused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	lim := ratelimit.New(1000, 1000)
	c := newTestClient(testCfg(config.ModeNewest, 0), srv, WithLimiter(lim))
	c.FetchSnapshots(context.Background(), "https://example.com/")

	if lim.PauseEnd().IsZero() {
		t.Error("expected limiter.SetPause to be called after CDX 429")
	}
	if time.Until(lim.PauseEnd()) < 55*time.Second {
		t.Errorf("expected pause ≥60s in limiter, but pause ends in %v", time.Until(lim.PauseEnd()))
	}
}

func TestFetchSnapshots_CDX429_RetriesAfterPause(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // 0s pause for test speed
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rows := [][]string{
			{"timestamp", "statuscode", "mimetype", "original", "digest"},
			{"20230601120000", "200", "text/html", "https://example.com/", "AAAA"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cdxRows(rows))
	}))
	t.Cleanup(srv.Close)

	lim := ratelimit.New(1000, 1000)
	c := newTestClient(testCfg(config.ModeNewest, 2), srv, WithLimiter(lim))
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("expected success after 429 retry: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 snapshot after retry, got %d", len(snaps))
	}
	if calls != 2 {
		t.Errorf("expected 2 CDX calls (1 × 429 + 1 success), got %d", calls)
	}
}

// ── Alt-scheme fallback ───────────────────────────────────────────────────

func TestFetchSnapshots_AltSchemeFallback(t *testing.T) {
	// First CDX query (https) returns empty. Second (http) returns a result.
	var calls int32
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20100101000000", "200", "text/html", "http://example.com/", "AAAA"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First call (https) → empty
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		// Second call (http fallback) → real result
		w.Header().Set("Content-Type", "application/json")
		w.Write(cdxRows(rows))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(testCfg(config.ModeNewest, 0), srv)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 snapshot from alt-scheme fallback, got %d", len(snaps))
	}
	if calls != 2 {
		t.Errorf("expected 2 CDX calls (https empty + http success), got %d", calls)
	}
}

// ── Context cancellation ──────────────────────────────────────────────────

func TestFetchSnapshots_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := newTestClient(testCfg(config.ModeNewest, 0), srv)
	_, err := c.FetchSnapshots(ctx, "https://example.com/")
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
}

// ── cdxBackoff ────────────────────────────────────────────────────────────

func TestCDXBackoff_FirstAttemptZero(t *testing.T) {
	if d := cdxBackoff(0); d != 0 {
		t.Errorf("cdxBackoff(0) = %v, want 0", d)
	}
}

func TestCDXBackoff_PositiveAndCapped(t *testing.T) {
	for i := 1; i <= 6; i++ {
		d := cdxBackoff(i)
		if d <= 0 {
			t.Errorf("cdxBackoff(%d) = %v, want >0", i, d)
		}
		if d > 20*time.Second {
			t.Errorf("cdxBackoff(%d) = %v, exceeds cap", i, d)
		}
	}
}

// ── isCDXRetryable ────────────────────────────────────────────────────────

func TestIsCDXRetryable(t *testing.T) {
	cases := []struct {
		code      int
		retryable bool
	}{
		{200, false},
		{400, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{503, true},
	}
	for _, c := range cases {
		got := isCDXRetryable(&cdxError{code: c.code})
		if got != c.retryable {
			t.Errorf("isCDXRetryable(HTTP %d) = %v, want %v", c.code, got, c.retryable)
		}
	}
}

// ── Concurrency: shared rate limiter ─────────────────────────────────────

func TestFetchSnapshots_SharedLimiter_ThrottlesConcurrentCDX(t *testing.T) {
	// 5 concurrent CDX queries against a 5-RPS limiter with burst=1.
	// All 5 should succeed but take ≥(n-1)×200ms = 800ms.
	rows := [][]string{
		{"timestamp", "statuscode", "mimetype", "original", "digest"},
		{"20230601120000", "200", "text/html", "https://example.com/", "AAAA"},
	}
	srv := httptest.NewServer(serveCDX(rows))
	t.Cleanup(srv.Close)

	lim := ratelimit.New(5, 1)
	n := 5
	errs := make(chan error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func(idx int) {
			c := newTestClient(testCfg(config.ModeNewest, 0), srv, WithLimiter(lim))
			_, err := c.FetchSnapshots(context.Background(), fmt.Sprintf("https://example%d.com/", idx))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("CDX worker: %v", err)
		}
	}
	elapsed := time.Since(start)
	// n-1 gaps at 200ms each = 800ms minimum.
	if elapsed < 600*time.Millisecond {
		t.Errorf("rate limiter not throttling CDX: %d queries in %v (expected ≥600ms)", n, elapsed)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// contains reports whether substr appears anywhere in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}()
}
