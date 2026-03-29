// Package commoncrawl implements the provider.Provider and
// provider.ContentFetcher interfaces for the Common Crawl open web corpus.
//
// Snapshot metadata is retrieved via the Common Crawl CDX API (one query per
// collection, up to cfg.CCMaxCollections newest collections).  Content is
// retrieved via HTTP Range requests against data.commoncrawl.org, where each
// byte range contains a single gzip-compressed WARC record holding the
// original HTTP response.
package commoncrawl

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

const (
	defaultCollInfoURL = "https://index.commoncrawl.org/collinfo.json"
	defaultDataBase    = "https://data.commoncrawl.org"
	timestampLayout    = "20060102150405"
)

// ccBackoff returns jittered exponential back-off before retry attempt (1-based).
// Caps at ~8 s: ~1 s, ~2 s, ~4 s, ~8 s.
func ccBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	exp := attempt - 1
	if exp > 3 {
		exp = 3
	}
	base := time.Duration(1<<uint(exp)) * time.Second
	jitter := time.Duration(rand.Int63n(int64(base) + 1))
	return base + jitter
}

// ccHTTPError carries an HTTP status code for CC CDX or WARC requests so
// retry decisions can be made without inspecting error strings.
type ccHTTPError struct{ code int }

func (e *ccHTTPError) Error() string { return fmt.Sprintf("CC HTTP %d", e.code) }

// isCCRetryable returns true for errors that are worth retrying.
func isCCRetryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var he *ccHTTPError
	if errors.As(err, &he) {
		return he.code == http.StatusTooManyRequests ||
			(he.code >= 500 && he.code < 600)
	}
	// Network / transport errors (timeouts, resets) are retryable.
	return true
}

// collection represents one Common Crawl index collection entry.
type collection struct {
	ID     string `json:"id"`
	CDXAPI string `json:"cdx-api"`
	Name   string `json:"name"`
}

// cdxRecord is one NDJSON line from the Common Crawl CDX API.
type cdxRecord struct {
	URL       string `json:"url"`
	Timestamp string `json:"timestamp"`
	MIME      string `json:"mime"`
	Status    string `json:"status"`
	Digest    string `json:"digest"`
	Filename  string `json:"filename"`
	Offset    string `json:"offset"`
	Length    string `json:"length"`
}

// Client is a Common Crawl archive provider.
type Client struct {
	cfg         *config.Config
	client      *http.Client
	limiter     *ratelimit.Limiter
	collInfoURL string // overridable in tests
	dataBase    string // overridable in tests

	// Cached collection list (fetched once per Client instance).
	collOnce sync.Once
	colls    []collection
	collErr  error
}

// Option configures a Client.
type Option func(*Client)

// WithLimiter attaches a shared rate limiter.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(c *Client) { c.limiter = l }
}

// WithCollInfoURL overrides the collinfo.json endpoint. Used in tests.
func WithCollInfoURL(u string) Option {
	return func(c *Client) { c.collInfoURL = u }
}

// WithDataBase overrides the WARC data base URL. Used in tests.
func WithDataBase(base string) Option {
	return func(c *Client) { c.dataBase = base }
}

// WithHTTPClient replaces the internal HTTP client. Used in tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.client = hc }
}

// New returns a new Common Crawl Client.
func New(cfg *config.Config, opts ...Option) *Client {
	c := &Client{
		cfg:         cfg,
		collInfoURL: defaultCollInfoURL,
		dataBase:    defaultDataBase,
		client:      &http.Client{Timeout: cfg.Timeout * 3},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "commoncrawl" }

// logf routes a verbose message through cfg.LogVerbose or stderr fallback.
func (c *Client) logf(format string, args ...any) {
	if c.cfg.LogVerbose != nil {
		c.cfg.LogVerbose(format, args...)
	} else {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// FetchSnapshots queries up to cfg.CCMaxCollections Common Crawl collections
// for snapshots of rawURL.  All collections are queried in parallel; for
// oldest/newest mode the remaining queries are cancelled after the first
// non-empty result is received.
func (c *Client) FetchSnapshots(ctx context.Context, rawURL string) ([]provider.Snapshot, error) {
	colls, err := c.getCollections(ctx)
	if err != nil {
		return nil, fmt.Errorf("get collections: %w", err)
	}
	if len(colls) == 0 {
		return nil, nil
	}

	maxColls := c.cfg.CCMaxCollections
	if maxColls <= 0 || maxColls > len(colls) {
		maxColls = len(colls)
	}

	type collResult struct {
		id    string
		snaps []provider.Snapshot
	}

	ch := make(chan collResult, maxColls)
	collCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < maxColls; i++ {
		i := i
		go func() {
			snaps, qErr := c.queryCDX(collCtx, colls[i].CDXAPI, rawURL)
			if qErr != nil {
				if c.cfg.Verbose && collCtx.Err() == nil {
					c.logf("[commoncrawl] collection %s error: %v", colls[i].ID, qErr)
				}
				ch <- collResult{id: colls[i].ID}
				return
			}
			ch <- collResult{id: colls[i].ID, snaps: snaps}
			// For oldest/newest mode, cancel remaining queries as soon as we
			// have results — their responses would only add duplicates.
			if len(snaps) > 0 && c.cfg.Mode != config.ModeAll {
				cancel()
			}
		}()
	}

	seen := make(map[string]struct{})
	var snaps []provider.Snapshot

	for i := 0; i < maxColls; i++ {
		r := <-ch
		for _, s := range r.snaps {
			key := s.Digest
			if key == "" {
				key = s.ArchiveURL
			}
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				snaps = append(snaps, s)
			}
		}
	}

	return snaps, nil
}

// FetchHostInventory implements provider.HostInventoryFetcher.
// It queries up to cfg.CCMaxCollections CC index collections in parallel
// using url=host/* and merges the results, deduplicating by digest.
func (c *Client) FetchHostInventory(ctx context.Context, host string) ([]provider.Snapshot, error) {
	colls, err := c.getCollections(ctx)
	if err != nil {
		return nil, fmt.Errorf("get collections: %w", err)
	}
	if len(colls) == 0 {
		return nil, nil
	}

	maxColls := c.cfg.CCMaxCollections
	if maxColls <= 0 || maxColls > len(colls) {
		maxColls = len(colls)
	}

	type collResult struct {
		snaps []provider.Snapshot
	}

	ch := make(chan collResult, maxColls)
	collCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < maxColls; i++ {
		i := i
		go func() {
			snaps, qErr := c.queryHostCDX(collCtx, colls[i].CDXAPI, host)
			if qErr != nil {
				if c.cfg.Verbose && collCtx.Err() == nil {
					c.logf("[commoncrawl] host CDX %s error: %v", colls[i].ID, qErr)
				}
				ch <- collResult{}
				return
			}
			ch <- collResult{snaps: snaps}
		}()
	}

	seen := make(map[string]struct{})
	var all []provider.Snapshot

	for i := 0; i < maxColls; i++ {
		r := <-ch
		for _, s := range r.snaps {
			key := s.Digest
			if key == "" {
				key = s.ArchiveURL
			}
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				all = append(all, s)
			}
		}
	}

	return all, nil
}

// queryHostCDX fetches all snapshots for url=host/* from one CC CDX endpoint.
// It retries on 429 (honoring Retry-After via the shared limiter) and 5xx.
func (c *Client) queryHostCDX(ctx context.Context, cdxAPI, host string) ([]provider.Snapshot, error) {
	apiURL := c.buildHostCDXURL(cdxAPI, host)
	if c.cfg.Verbose {
		c.logf("[commoncrawl] host CDX: %s", apiURL)
	}

	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			wait := ccBackoff(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")

		resp, err := c.client.Do(req)
		if err != nil {
			if attempt < c.cfg.Retries {
				continue
			}
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := ratelimit.ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second)
			if c.limiter != nil {
				c.limiter.SetPause(wait)
			}
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			continue // limiter.Wait on next iteration will observe the pause
		}
		if resp.StatusCode == http.StatusNotFound {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			he := &ccHTTPError{code: resp.StatusCode}
			if isCCRetryable(he) && attempt < c.cfg.Retries {
				continue
			}
			return nil, he
		}

		snaps, err := c.parseCDXNDJSON(resp.Body, host)
		resp.Body.Close()
		return snaps, err
	}
	return nil, fmt.Errorf("host CDX failed after %d attempt(s)", c.cfg.Retries+1)
}

func (c *Client) buildHostCDXURL(cdxAPI, host string) string {
	p := url.Values{}
	p.Set("url", host+"/*")
	p.Set("output", "json")
	p.Set("fl", "timestamp,status,mime,digest,filename,offset,length,url")

	if c.cfg.StatusFilter != "" {
		p.Set("filter", "status:"+c.cfg.StatusFilter)
	}
	if c.cfg.HostQueryLimit > 0 {
		p.Set("limit", strconv.Itoa(c.cfg.HostQueryLimit))
	}

	return cdxAPI + "?" + p.Encode()
}

// getCollections returns the cached collection list, fetching it on first call.
func (c *Client) getCollections(ctx context.Context) ([]collection, error) {
	c.collOnce.Do(func() {
		c.colls, c.collErr = c.fetchCollections(ctx)
	})
	return c.colls, c.collErr
}

func (c *Client) fetchCollections(ctx context.Context) ([]collection, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.collInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil, fmt.Errorf("collinfo HTTP %d", resp.StatusCode)
	}

	var colls []collection
	if err := json.NewDecoder(resp.Body).Decode(&colls); err != nil {
		return nil, fmt.Errorf("parse collinfo JSON: %w", err)
	}
	return colls, nil
}

func (c *Client) queryCDX(ctx context.Context, cdxAPI, targetURL string) ([]provider.Snapshot, error) {
	apiURL := c.buildCDXURL(cdxAPI, targetURL)
	if c.cfg.Verbose {
		c.logf("[commoncrawl] CDX: %s", apiURL)
	}

	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			wait := ccBackoff(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")

		resp, err := c.client.Do(req)
		if err != nil {
			if attempt < c.cfg.Retries {
				continue
			}
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := ratelimit.ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second)
			if c.limiter != nil {
				c.limiter.SetPause(wait)
			}
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			he := &ccHTTPError{code: resp.StatusCode}
			if isCCRetryable(he) && attempt < c.cfg.Retries {
				continue
			}
			return nil, he
		}

		snaps, err := c.parseCDXNDJSON(resp.Body, targetURL)
		resp.Body.Close()
		return snaps, err
	}
	return nil, fmt.Errorf("CDX failed after %d attempt(s)", c.cfg.Retries+1)
}

func (c *Client) buildCDXURL(cdxAPI, targetURL string) string {
	p := url.Values{}
	p.Set("url", targetURL)
	p.Set("output", "json")
	p.Set("fl", "timestamp,status,mime,digest,filename,offset,length,url")

	if c.cfg.StatusFilter != "" {
		p.Set("filter", "status:"+c.cfg.StatusFilter)
	}

	switch c.cfg.Mode {
	case config.ModeOldest:
		p.Set("limit", "1")
	case config.ModeNewest:
		// pywb-compatible: negative limit returns most-recent entries.
		p.Set("limit", "-1")
	case config.ModeAll:
		if c.cfg.MaxSnapshots > 0 {
			p.Set("limit", strconv.Itoa(c.cfg.MaxSnapshots))
		} else {
			p.Set("limit", "10") // conservative cap for CC to avoid huge responses
		}
	}

	return cdxAPI + "?" + p.Encode()
}

// parseCDXNDJSON parses a Common Crawl NDJSON CDX response.
// fallbackURL is used as OriginalURL when the CDX record omits the "url" field.
func (c *Client) parseCDXNDJSON(r io.Reader, fallbackURL string) ([]provider.Snapshot, error) {
	var snaps []provider.Snapshot
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec cdxRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // skip malformed lines silently
		}
		if rec.Timestamp == "" || rec.Filename == "" {
			continue // WARC metadata required for content retrieval
		}
		ts, err := time.Parse(timestampLayout, rec.Timestamp)
		if err != nil {
			continue
		}
		orig := rec.URL
		if orig == "" {
			orig = fallbackURL // CDX sometimes omits the url field
		}
		// Encode WARC byte-range metadata directly in ArchiveURL as synthetic
		// query params (_warcOffset, _warcLength).  FetchContent strips them
		// before making the actual Range request so the S3 server never sees them.
		archiveURL := fmt.Sprintf("%s/%s?_warcOffset=%s&_warcLength=%s",
			c.dataBase, rec.Filename, rec.Offset, rec.Length)
		snaps = append(snaps, provider.Snapshot{
			OriginalURL: orig,
			ArchiveURL:  archiveURL,
			Timestamp:   ts,
			StatusCode:  rec.Status,
			MIMEType:    rec.MIME,
			Digest:      rec.Digest,
		})
	}
	return snaps, scanner.Err()
}

// FetchContent fetches the WARC record via an HTTP Range request, decompresses
// the gzip stream, strips the WARC envelope, and writes the HTTP response body
// atomically to destPath.
//
// WARC byte-range metadata (_warcOffset, _warcLength) is read from the
// synthetic query params that parseCDXNDJSON embedded in snap.ArchiveURL.
// Transient HTTP 5xx and network errors are retried up to cfg.Retries times.
func (c *Client) FetchContent(ctx context.Context, snap provider.Snapshot, destPath string) error {
	// Parse WARC metadata once — these are permanent errors if they fail.
	u, parseErr := url.Parse(snap.ArchiveURL)
	if parseErr != nil {
		return fmt.Errorf("parse archive URL: %w", parseErr)
	}
	q := u.Query()
	offsetStr := q.Get("_warcOffset")
	lengthStr := q.Get("_warcLength")
	if offsetStr == "" || lengthStr == "" {
		return fmt.Errorf("WARC metadata missing from archive URL %q", snap.ArchiveURL)
	}
	offset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil || offset < 0 {
		return fmt.Errorf("invalid WARC offset %q", offsetStr)
	}
	length, err := strconv.ParseInt(lengthStr, 10, 64)
	if err != nil || length <= 0 {
		return fmt.Errorf("invalid WARC length %q", lengthStr)
	}
	// Strip synthetic params to get the real S3 data URL.
	q.Del("_warcOffset")
	q.Del("_warcLength")
	u.RawQuery = q.Encode()
	dataURL := u.String()

	if c.cfg.Verbose {
		c.logf("[commoncrawl] WARC %s bytes=%d-%d", dataURL, offset, offset+length-1)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			wait := ccBackoff(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err := c.fetchWARCRecord(ctx, dataURL, offset, length, destPath); err == nil {
			return nil
		} else {
			lastErr = err
			if !isCCRetryable(err) {
				break
			}
		}
	}
	return fmt.Errorf("WARC fetch failed after %d attempt(s): %w", c.cfg.Retries+1, lastErr)
}

// fetchWARCRecord performs a single attempt of the WARC byte-range download:
// rate-limit wait → Range GET → gzip decompress → WARC header skip →
// HTTP response parse → atomic write to destPath.
func (c *Client) fetchWARCRecord(ctx context.Context, dataURL string, offset, length int64, destPath string) error {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return &ccHTTPError{code: resp.StatusCode}
	}

	// Decompress the gzip-compressed WARC record.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("open gzip reader: %w", err)
	}
	defer gz.Close()

	br := bufio.NewReader(gz)

	// Skip the WARC header section (ends at the blank line \r\n).
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read WARC header: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Parse the HTTP response embedded in the WARC payload.
	httpResp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("parse HTTP response in WARC: %w", err)
	}
	defer httpResp.Body.Close()

	// Atomic write: temp file + rename.
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	tmp := destPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	_, copyErr := io.Copy(f, httpResp.Body)
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
