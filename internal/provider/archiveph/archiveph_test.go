package archiveph

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

// ── parseTimemap unit tests ───────────────────────────────────────────────

const sampleTimemap = `<https://archive.ph/timemap/https://example.com>; rel="self timemap"; type="application/link-format"; from="Tue, 20 Oct 2009 03:32:35 GMT",
<https://example.com>; rel="original",
<https://archive.ph/timemap/https://example.com>; rel="timemap"; type="application/link-format",
<http://archive.md/20091020033235/http://www.example.com/>; rel="first memento"; datetime="Tue, 20 Oct 2009 03:32:35 GMT",
<http://archive.md/20200121204124/https://example.com/>; rel="memento"; datetime="Tue, 21 Jan 2020 20:41:24 GMT",
<http://archive.md/20260223033330/https://example.com/>; rel="last memento"; datetime="Mon, 23 Feb 2026 03:33:30 GMT"`

func TestParseTimemap_ReturnsThreeMementos(t *testing.T) {
	snaps, err := parseTimemap(strings.NewReader(sampleTimemap), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(snaps))
	}
}

func TestParseTimemap_SkipsOriginalAndSelf(t *testing.T) {
	snaps, err := parseTimemap(strings.NewReader(sampleTimemap), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range snaps {
		if strings.Contains(s.ArchiveURL, "timemap") {
			t.Errorf("timemap self-link should not be in snapshots: %s", s.ArchiveURL)
		}
	}
}

func TestParseTimemap_OriginalURLPreserved(t *testing.T) {
	snaps, err := parseTimemap(strings.NewReader(sampleTimemap), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range snaps {
		if s.OriginalURL != "https://example.com" {
			t.Errorf("OriginalURL mismatch: got %q, want %q", s.OriginalURL, "https://example.com")
		}
	}
}

// ── parseLinkEntry unit tests ─────────────────────────────────────────────

func TestParseLinkEntry_TimestampFromURL(t *testing.T) {
	entry := `<http://archive.md/20260223033330/https://example.com/>; rel="memento"; datetime="Mon, 23 Feb 2026 03:33:30 GMT"`
	snap, ok := parseLinkEntry(entry, "https://example.com")
	if !ok {
		t.Fatal("expected parseLinkEntry to return ok=true")
	}
	want := time.Date(2026, 2, 23, 3, 33, 30, 0, time.UTC)
	if !snap.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", snap.Timestamp, want)
	}
	if snap.ArchiveURL != "http://archive.md/20260223033330/https://example.com/" {
		t.Errorf("ArchiveURL = %q", snap.ArchiveURL)
	}
}

func TestParseLinkEntry_FallbackToDatetime(t *testing.T) {
	// URL without a 14-digit segment — must fall back to datetime= attribute.
	entry := `<http://archive.ph/https://example.com>; rel="memento"; datetime="Tue, 20 Oct 2009 03:32:35 GMT"`
	snap, ok := parseLinkEntry(entry, "https://example.com")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if snap.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp from datetime= fallback")
	}
}

func TestParseLinkEntry_NonMementoSkipped(t *testing.T) {
	entry := `<https://example.com>; rel="original"`
	_, ok := parseLinkEntry(entry, "https://example.com")
	if ok {
		t.Error("original link should not be parsed as a snapshot")
	}
}

func TestParseLinkEntry_MissingURL(t *testing.T) {
	entry := `rel="memento"; datetime="Mon, 23 Feb 2026 03:33:30 GMT"`
	_, ok := parseLinkEntry(entry, "https://example.com")
	if ok {
		t.Error("entry without URL should not be parsed")
	}
}

// ── HTTP integration tests ────────────────────────────────────────────────

func TestFetchSnapshots_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/link-format")
		fmt.Fprint(w, sampleTimemap)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(snaps))
	}
}

func TestFetchSnapshots_NotFound_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com")
	if err != nil {
		t.Errorf("404 should not be an error, got: %v", err)
	}
	if snaps != nil {
		t.Errorf("404 should return nil snapshots")
	}
}

func TestFetchSnapshots_403_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com")
	if err != nil {
		t.Errorf("403 should not be an error, got: %v", err)
	}
	if snaps != nil {
		t.Errorf("403 should return nil snapshots (not an error)")
	}
}

func TestFetchSnapshots_500_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	_, err := c.FetchSnapshots(context.Background(), "https://example.com")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestFetchSnapshots_EmptyBody_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// empty body
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots for empty body, got %d", len(snaps))
	}
}

func TestFetchSnapshots_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(baseCfg(), WithTimemapBase(srv.URL+"/"), WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.FetchSnapshots(ctx, "https://example.com")
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}
