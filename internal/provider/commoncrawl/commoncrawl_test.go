package commoncrawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
)

func baseCfg() *config.Config {
	return &config.Config{
		Timeout:          5 * time.Second,
		Mode:             config.ModeAll,
		CCMaxCollections: 5,
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func makeCollInfoJSON(cdxAPIURL string) []byte {
	colls := []map[string]string{
		{"id": "CC-TEST-2024", "cdx-api": cdxAPIURL, "name": "Test Collection 2024"},
	}
	b, _ := json.Marshal(colls)
	return b
}

func makeNDJSONLine(filename, offset, length string) string {
	rec := cdxRecord{
		URL:       "https://example.com/",
		Timestamp: "20240101120000",
		MIME:      "text/html",
		Status:    "200",
		Digest:    "SHA1:ABCDEF",
		Filename:  filename,
		Offset:    offset,
		Length:    length,
	}
	b, _ := json.Marshal(rec)
	return string(b) + "\n"
}

// makeWARCGzip creates a minimal gzip-compressed WARC record containing an
// HTTP/1.1 200 response with the given body.
func makeWARCGzip(t *testing.T, body string) []byte {
	t.Helper()
	httpResponse := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	warcPayload := "WARC/1.0\r\n" +
		"WARC-Type: response\r\n" +
		"WARC-Target-URI: https://example.com/\r\n" +
		"Content-Type: application/http; msgtype=response\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(httpResponse)) +
		"\r\n" +
		httpResponse

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := fmt.Fprint(gz, warcPayload); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// snapWithMeta builds a provider.Snapshot whose ArchiveURL carries WARC
// metadata as synthetic query params (_warcOffset, _warcLength), matching
// what parseCDXNDJSON produces.  baseURL must be the test server URL so
// FetchContent sends its Range request to the right host.
func snapWithMeta(baseURL, filename, offset, length string) provider.Snapshot {
	archiveURL := fmt.Sprintf("%s/%s?_warcOffset=%s&_warcLength=%s",
		baseURL, filename, offset, length)
	return provider.Snapshot{
		OriginalURL: "https://example.com/",
		ArchiveURL:  archiveURL,
		Timestamp:   time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
	}
}

// ── FetchSnapshots ────────────────────────────────────────────────────────

func TestFetchSnapshots_Success(t *testing.T) {
	cdxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, makeNDJSONLine("crawl/warc/file.warc.gz", "0", "1000"))
	}))
	defer cdxSrv.Close()

	collinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(makeCollInfoJSON(cdxSrv.URL))
	}))
	defer collinfoSrv.Close()

	c := New(baseCfg(),
		WithCollInfoURL(collinfoSrv.URL),
		WithHTTPClient(collinfoSrv.Client()),
	)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	// WARC metadata is now encoded in ArchiveURL as query params.
	if !strings.Contains(snaps[0].ArchiveURL, "crawl/warc/file.warc.gz") {
		t.Errorf("ArchiveURL missing filename: %s", snaps[0].ArchiveURL)
	}
	if !strings.Contains(snaps[0].ArchiveURL, "_warcOffset=0") {
		t.Errorf("ArchiveURL missing _warcOffset: %s", snaps[0].ArchiveURL)
	}
	if !strings.Contains(snaps[0].ArchiveURL, "_warcLength=1000") {
		t.Errorf("ArchiveURL missing _warcLength: %s", snaps[0].ArchiveURL)
	}
}

func TestFetchSnapshots_CollInfoError_ReturnsError(t *testing.T) {
	collinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer collinfoSrv.Close()

	c := New(baseCfg(),
		WithCollInfoURL(collinfoSrv.URL),
		WithHTTPClient(collinfoSrv.Client()),
	)
	_, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err == nil {
		t.Error("expected error when collinfo returns 500")
	}
}

func TestFetchSnapshots_CDXError_SkipsCollection(t *testing.T) {
	cdxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cdxSrv.Close()

	collinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(makeCollInfoJSON(cdxSrv.URL))
	}))
	defer collinfoSrv.Close()

	c := New(baseCfg(),
		WithCollInfoURL(collinfoSrv.URL),
		WithHTTPClient(collinfoSrv.Client()),
	)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("CDX 500 should not propagate as error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots when CDX fails, got %d", len(snaps))
	}
}

func TestFetchSnapshots_DeduplicatesByDigest(t *testing.T) {
	cdxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, makeNDJSONLine("crawl/warc/file.warc.gz", "0", "1000"))
	}))
	defer cdxSrv.Close()

	colls := []map[string]string{
		{"id": "CC-2024", "cdx-api": cdxSrv.URL, "name": "Coll 1"},
		{"id": "CC-2023", "cdx-api": cdxSrv.URL, "name": "Coll 2"},
	}
	collInfoJSON, _ := json.Marshal(colls)

	collinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(collInfoJSON)
	}))
	defer collinfoSrv.Close()

	cfg := baseCfg()
	cfg.CCMaxCollections = 2
	c := New(cfg,
		WithCollInfoURL(collinfoSrv.URL),
		WithHTTPClient(collinfoSrv.Client()),
	)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 deduplicated snapshot, got %d", len(snaps))
	}
}

// TestFetchSnapshots_ParallelNewestReturnsResults verifies that FetchSnapshots
// returns valid snapshots in newest mode when multiple collections are queried
// in parallel.  (Previously this test checked for sequential early-exit; with
// parallel queries all collections fire simultaneously and cancellation is
// best-effort for in-flight requests.)
func TestFetchSnapshots_ParallelNewestReturnsResults(t *testing.T) {
	cdxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := cdxRecord{
			URL: "https://example.com/", Timestamp: "20240101120000",
			Filename: "f.warc.gz", Offset: "0", Length: "1000",
			Digest: "SHA1:ABCD",
		}
		b, _ := json.Marshal(rec)
		fmt.Fprintf(w, "%s\n", b)
	}))
	defer cdxSrv.Close()

	colls := []map[string]string{
		{"id": "CC-2024", "cdx-api": cdxSrv.URL},
		{"id": "CC-2023", "cdx-api": cdxSrv.URL},
	}
	collInfoJSON, _ := json.Marshal(colls)

	collinfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(collInfoJSON)
	}))
	defer collinfoSrv.Close()

	cfg := baseCfg()
	cfg.Mode = config.ModeNewest
	cfg.CCMaxCollections = 2
	c := New(cfg,
		WithCollInfoURL(collinfoSrv.URL),
		WithHTTPClient(collinfoSrv.Client()),
	)
	snaps, err := c.FetchSnapshots(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Deduplication by digest means identical records across collections collapse to 1.
	if len(snaps) == 0 {
		t.Error("expected at least one snapshot, got 0")
	}
}

// ── parseCDXNDJSON unit tests ─────────────────────────────────────────────

func TestParseCDXNDJSON_SkipsMalformedLines(t *testing.T) {
	body := "not json\n" + makeNDJSONLine("f.warc.gz", "0", "500")
	c := New(baseCfg())
	snaps, err := c.parseCDXNDJSON(strings.NewReader(body), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("expected 1 valid snapshot, got %d", len(snaps))
	}
}

func TestParseCDXNDJSON_SkipsRecordsWithoutFilename(t *testing.T) {
	rec := `{"url":"https://example.com/","timestamp":"20240101120000","mime":"text/html"}` + "\n"
	snaps, err := New(baseCfg()).parseCDXNDJSON(strings.NewReader(rec), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots without filename, got %d", len(snaps))
	}
}

func TestParseCDXNDJSON_EncodesWARCMetaInArchiveURL(t *testing.T) {
	body := makeNDJSONLine("path/to/file.warc.gz", "12345", "6789")
	c := New(baseCfg())
	snaps, err := c.parseCDXNDJSON(strings.NewReader(body), "https://example.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	u := snaps[0].ArchiveURL
	if !strings.Contains(u, "_warcOffset=12345") {
		t.Errorf("ArchiveURL missing _warcOffset=12345: %s", u)
	}
	if !strings.Contains(u, "_warcLength=6789") {
		t.Errorf("ArchiveURL missing _warcLength=6789: %s", u)
	}
}

// ── FetchContent ──────────────────────────────────────────────────────────

func TestFetchContent_Success(t *testing.T) {
	wantBody := "<html>hello from common crawl</html>"
	warcData := makeWARCGzip(t, wantBody)

	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("expected Range header in WARC request")
		}
		w.WriteHeader(http.StatusPartialContent)
		w.Write(warcData)
	}))
	defer dataSrv.Close()

	c := New(baseCfg(), WithHTTPClient(dataSrv.Client()))
	dest := filepath.Join(t.TempDir(), "out.html")

	// Build snap manually with test server URL so FetchContent hits dataSrv.
	snap := snapWithMeta(dataSrv.URL, "warc/file.warc.gz", "0", fmt.Sprintf("%d", len(warcData)))
	if err := c.FetchContent(context.Background(), snap, dest); err != nil {
		t.Fatalf("FetchContent error: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body mismatch: got %q, want %q", got, wantBody)
	}
}

func TestFetchContent_NoPartFileAfterSuccess(t *testing.T) {
	warcData := makeWARCGzip(t, "ok")
	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		w.Write(warcData)
	}))
	defer dataSrv.Close()

	c := New(baseCfg(), WithHTTPClient(dataSrv.Client()))
	dest := filepath.Join(t.TempDir(), "out.html")
	snap := snapWithMeta(dataSrv.URL, "warc/file.warc.gz", "0", fmt.Sprintf("%d", len(warcData)))
	c.FetchContent(context.Background(), snap, dest)

	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error(".part file not cleaned up after successful FetchContent")
	}
}

func TestFetchContent_MissingWARCParams_ReturnsError(t *testing.T) {
	c := New(baseCfg())
	dest := filepath.Join(t.TempDir(), "out.html")
	// ArchiveURL with no _warcOffset/_warcLength params → missing metadata error.
	snap := provider.Snapshot{ArchiveURL: "https://data.commoncrawl.org/file.warc.gz"}
	if err := c.FetchContent(context.Background(), snap, dest); err == nil {
		t.Error("expected error when WARC params are missing from ArchiveURL")
	}
}

func TestFetchContent_InvalidOffset_ReturnsError(t *testing.T) {
	c := New(baseCfg())
	dest := filepath.Join(t.TempDir(), "out.html")
	snap := provider.Snapshot{
		ArchiveURL: "https://data.commoncrawl.org/f.warc.gz?_warcOffset=notanumber&_warcLength=100",
	}
	if err := c.FetchContent(context.Background(), snap, dest); err == nil {
		t.Error("expected error for non-numeric offset")
	}
}

func TestFetchContent_InvalidLength_ReturnsError(t *testing.T) {
	c := New(baseCfg())
	dest := filepath.Join(t.TempDir(), "out.html")
	snap := provider.Snapshot{
		ArchiveURL: "https://data.commoncrawl.org/f.warc.gz?_warcOffset=0&_warcLength=-1",
	}
	if err := c.FetchContent(context.Background(), snap, dest); err == nil {
		t.Error("expected error for negative length")
	}
}

func TestFetchContent_ServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(baseCfg(), WithHTTPClient(srv.Client()))
	dest := filepath.Join(t.TempDir(), "out.html")
	snap := snapWithMeta(srv.URL, "warc/file.warc.gz", "0", "1000")
	if err := c.FetchContent(context.Background(), snap, dest); err == nil {
		t.Error("expected error for server 500")
	}
}
