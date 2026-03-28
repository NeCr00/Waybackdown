package arquivo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
)

func baseCfg() *config.Config {
	return &config.Config{
		Timeout: 5 * time.Second,
		Mode:    config.ModeAll,
	}
}

// ── parseNDJSON unit tests ────────────────────────────────────────────────

const sampleNDJSON = `{"timestamp":"20240115120000","url":"https://exemplo.pt/","mime":"text/html","status":"200","digest":"SHA1:ABC"}
{"timestamp":"20230601090000","url":"https://exemplo.pt/","mime":"text/html","status":"200","digest":"SHA1:DEF"}
`

func TestParseNDJSON_ParsesTwoRecords(t *testing.T) {
	snaps, err := parseNDJSON(strings.NewReader(sampleNDJSON), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snaps))
	}
}

func TestParseNDJSON_ArchiveURLFormat(t *testing.T) {
	snaps, err := parseNDJSON(strings.NewReader(sampleNDJSON), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://arquivo.pt/noFrame/replay/20240115120000id_/https://exemplo.pt/"
	if snaps[0].ArchiveURL != want {
		t.Errorf("ArchiveURL = %q, want %q", snaps[0].ArchiveURL, want)
	}
}

func TestParseNDJSON_TimestampParsed(t *testing.T) {
	snaps, err := parseNDJSON(strings.NewReader(sampleNDJSON), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTS := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	if !snaps[0].Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", snaps[0].Timestamp, wantTS)
	}
}

func TestParseNDJSON_SkipsMalformedLines(t *testing.T) {
	body := "not json\n" + `{"timestamp":"20240101120000","url":"https://exemplo.pt/","mime":"text/html","status":"200","digest":"X"}` + "\n"
	snaps, err := parseNDJSON(strings.NewReader(body), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 valid snapshot after skipping malformed line, got %d", len(snaps))
	}
}

func TestParseNDJSON_SkipsMissingTimestamp(t *testing.T) {
	body := `{"url":"https://exemplo.pt/","mime":"text/html","status":"200"}` + "\n"
	snaps, err := parseNDJSON(strings.NewReader(body), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots for missing timestamp, got %d", len(snaps))
	}
}

func TestParseNDJSON_FallsBackToOriginalURL(t *testing.T) {
	body := `{"timestamp":"20240101120000","mime":"text/html","status":"200","digest":"X"}` + "\n"
	snaps, err := parseNDJSON(strings.NewReader(body), "https://fallback.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].OriginalURL != "https://fallback.pt/" {
		t.Errorf("OriginalURL = %q, want fallback.pt", snaps[0].OriginalURL)
	}
}

// ── buildCDXURL unit tests ────────────────────────────────────────────────

func TestBuildCDXURL_NewestMode(t *testing.T) {
	cfg := baseCfg()
	cfg.Mode = config.ModeNewest
	c := New(cfg)
	u := c.buildCDXURL("https://exemplo.pt/")
	if !strings.Contains(u, "sort=reverse") {
		t.Errorf("newest mode should include sort=reverse, got: %s", u)
	}
	if !strings.Contains(u, "limit=1") {
		t.Errorf("newest mode should include limit=1, got: %s", u)
	}
}

func TestBuildCDXURL_OldestMode(t *testing.T) {
	cfg := baseCfg()
	cfg.Mode = config.ModeOldest
	c := New(cfg)
	u := c.buildCDXURL("https://exemplo.pt/")
	if !strings.Contains(u, "limit=1") {
		t.Errorf("oldest mode should include limit=1, got: %s", u)
	}
	if strings.Contains(u, "sort=reverse") {
		t.Errorf("oldest mode should NOT include sort=reverse, got: %s", u)
	}
}

func TestBuildCDXURL_AllModeWithMax(t *testing.T) {
	cfg := baseCfg()
	cfg.Mode = config.ModeAll
	cfg.MaxSnapshots = 25
	c := New(cfg)
	u := c.buildCDXURL("https://exemplo.pt/")
	if !strings.Contains(u, "limit=25") {
		t.Errorf("all mode with max=25 should include limit=25, got: %s", u)
	}
}

func TestBuildCDXURL_StatusFilter(t *testing.T) {
	cfg := baseCfg()
	cfg.StatusFilter = "200"
	c := New(cfg)
	u := c.buildCDXURL("https://exemplo.pt/")
	if !strings.Contains(u, "filter=statuscode") {
		t.Errorf("status filter should appear in CDX URL, got: %s", u)
	}
}

// ── HTTP integration tests ────────────────────────────────────────────────

func TestFetchSnapshots_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, sampleNDJSON)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithCDXEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snaps))
	}
}

func TestFetchSnapshots_NotFound_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithCDXEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://exemplo.pt/")
	if err != nil {
		t.Errorf("404 should not be an error: %v", err)
	}
	if snaps != nil {
		t.Error("404 should return nil snapshots")
	}
}

func TestFetchSnapshots_HTTPError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithCDXEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	_, err := c.FetchSnapshots(context.Background(), "https://exemplo.pt/")
	if err == nil {
		t.Error("expected error for 503 response")
	}
}

func TestFetchSnapshots_EmptyBody_ReturnsNilSlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithCDXEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://exemplo.pt/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("empty body should yield 0 snapshots, got %d", len(snaps))
	}
}

func TestFetchSnapshots_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(baseCfg(), WithCDXEndpoint(srv.URL), WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FetchSnapshots(ctx, "https://exemplo.pt/")
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}
