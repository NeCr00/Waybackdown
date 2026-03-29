// Package wayback implements the provider.Provider interface for the
// Internet Archive Wayback Machine CDX API.
package wayback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/normalize"
	"github.com/NeCr00/Waybackdown/internal/provider"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

const (
	defaultCDXEndpoint = "https://web.archive.org/cdx/search/cdx"
	archiveBase        = "https://web.archive.org/web"
	timestampLayout    = "20060102150405"

	// id_ suppresses the Wayback toolbar and returns the raw archived content.
	archiveSuffix = "id_"
)

// Client is a Wayback Machine archive provider.
type Client struct {
	cfg         *config.Config
	client      *http.Client
	limiter     *ratelimit.Limiter // may be nil
	cdxEndpoint string             // overridable in tests
}

// Option configures a Client.
type Option func(*Client)

// WithLimiter attaches a shared rate limiter. All CDX requests honour it;
// a 429 Retry-After from CDX pauses the limiter for all workers.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(c *Client) { c.limiter = l }
}

// WithCDXEndpoint overrides the CDX API base URL. Used in tests so the
// client talks to a local httptest.Server instead of web.archive.org.
func WithCDXEndpoint(endpoint string) Option {
	return func(c *Client) { c.cdxEndpoint = endpoint }
}

// WithHTTPClient replaces the internal HTTP client. Used in tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.client = hc }
}

// New returns a new Wayback Machine Client.
func New(cfg *config.Config, opts ...Option) *Client {
	c := &Client{
		cfg:         cfg,
		cdxEndpoint: defaultCDXEndpoint,
		client: &http.Client{
			// CDX can return large JSON payloads; give it extra time.
			Timeout: cfg.Timeout * 3,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "wayback" }

// FetchSnapshots queries the CDX API for all snapshots of the given URL.
// If no results are found, it automatically retries with the alternate
// scheme (https→http or http→https) to cover old sites archived before HTTPS.
func (c *Client) FetchSnapshots(ctx context.Context, rawURL string) ([]provider.Snapshot, error) {
	snaps, err := c.fetchCDX(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	// Empty result? Try the other scheme before giving up.
	if len(snaps) == 0 {
		alt := normalize.ToggleScheme(rawURL)
		if alt != rawURL {
			if c.cfg.Verbose {
				fmt.Fprintf(os.Stderr,"[CDX]  no results for %s — retrying with %s\n", rawURL, alt)
			}
			snaps, err = c.fetchCDX(ctx, alt)
			if err != nil {
				// Non-fatal: log and return empty.
				if c.cfg.Verbose {
					fmt.Fprintf(os.Stderr,"[WARN] alt-scheme CDX query failed: %v\n", err)
				}
				return nil, nil
			}
		}
	}

	return snaps, nil
}

// fetchCDX performs the CDX API request with retry / back-off / rate-limit
// handling.
func (c *Client) fetchCDX(ctx context.Context, targetURL string) ([]provider.Snapshot, error) {
	apiURL := c.buildCDXURL(targetURL)
	if c.cfg.Verbose {
		fmt.Fprintf(os.Stderr,"[CDX]  %s\n", apiURL)
	}

	var (
		body    []byte
		lastErr error
	)

	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		// Back-off before retries (jittered exponential).
		if attempt > 0 {
			wait := cdxBackoff(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		// Acquire rate-limiter token before every request (including first).
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		var retryAfter time.Duration
		body, retryAfter, lastErr = c.get(ctx, apiURL)
		if lastErr == nil {
			break
		}

		// Rate-limited: pause the shared limiter so all workers wait.
		if retryAfter > 0 {
			if c.limiter != nil {
				c.limiter.SetPause(retryAfter)
			}
			fmt.Fprintf(os.Stderr,"[WAIT] CDX rate-limited — pausing %.0fs (Retry-After)\n",
				retryAfter.Seconds())
		}

		if !isCDXRetryable(lastErr) {
			break
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("CDX request failed after %d attempt(s): %w",
			c.cfg.Retries+1, lastErr)
	}

	return parseCDX(body, targetURL)
}

// buildCDXURL constructs the CDX API URL with the right query parameters
// for the configured mode.
func (c *Client) buildCDXURL(targetURL string) string {
	p := url.Values{}
	p.Set("url", targetURL)
	p.Set("output", "json")
	p.Set("fl", "timestamp,statuscode,mimetype,original,digest")

	if c.cfg.StatusFilter != "" {
		p.Set("filter", "statuscode:"+c.cfg.StatusFilter)
	}

	switch c.cfg.Mode {
	case config.ModeOldest:
		// Ask the CDX server for only 1 result (oldest) to minimise response size.
		p.Set("limit", "1")
	case config.ModeNewest:
		// Negative limit: CDX returns the last N entries (most recent first).
		p.Set("limit", "-1")
	case config.ModeAll:
		// Collapse by digest so only unique content versions are returned.
		p.Set("collapse", "digest")
		if c.cfg.MaxSnapshots > 0 {
			p.Set("limit", strconv.Itoa(c.cfg.MaxSnapshots))
		}
	}

	return c.cdxEndpoint + "?" + p.Encode()
}

// get performs a single GET to apiURL.
// Returns (body, retryAfterDuration, error).
// retryAfterDuration is non-zero only on a 429 response.
func (c *Client) get(ctx context.Context, apiURL string) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := ratelimit.ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second)
		return nil, wait, &cdxError{code: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, &cdxError{code: resp.StatusCode}
	}

	body, err := io.ReadAll(resp.Body)
	return body, 0, err
}

// cdxError carries an HTTP status code returned by the CDX API.
type cdxError struct{ code int }

func (e *cdxError) Error() string { return fmt.Sprintf("CDX API HTTP %d", e.code) }

// isCDXRetryable returns true for transient CDX errors.
func isCDXRetryable(err error) bool {
	var ce *cdxError
	if errors.As(err, &ce) {
		return ce.code == http.StatusTooManyRequests ||
			(ce.code >= 500 && ce.code < 600)
	}
	// Network errors are retryable.
	return true
}

// cdxBackoff returns the wait duration before CDX retry number attempt (1-based).
// Jittered exponential: ~1s, ~2s, ~4s, ~8s (capped).
func cdxBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	exp := attempt - 1
	if exp > 3 {
		exp = 3 // base cap 8s
	}
	base := time.Duration(1<<uint(exp)) * time.Second // 1, 2, 4, 8
	jitter := time.Duration(rand.Int63n(int64(base) + 1))
	return base + jitter
}

// parseCDX parses the CDX JSON array-of-arrays response.
//
// CDX format — first row is the header:
//
//	[["timestamp","statuscode","mimetype","original","digest"],
//	 ["20230101120000","200","text/html","https://example.com/","ABCD1234"],
//	 ...]
func parseCDX(data []byte, originalURL string) ([]provider.Snapshot, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}

	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse CDX JSON: %w", err)
	}
	if len(rows) < 2 {
		return nil, nil // header-only → no results
	}

	// Map header names to column indices for resilience against field reordering.
	colIdx := make(map[string]int, len(rows[0]))
	for i, name := range rows[0] {
		colIdx[name] = i
	}

	snapshots := make([]provider.Snapshot, 0, len(rows)-1)
	for _, row := range rows[1:] {
		tsStr := col(row, colIdx, "timestamp")
		ts, err := time.Parse(timestampLayout, tsStr)
		if err != nil {
			continue // skip malformed rows silently
		}

		orig := col(row, colIdx, "original")
		if orig == "" {
			orig = originalURL
		}

		archiveURL := fmt.Sprintf("%s/%s%s/%s", archiveBase, tsStr, archiveSuffix, orig)

		snapshots = append(snapshots, provider.Snapshot{
			OriginalURL: orig,
			ArchiveURL:  archiveURL,
			Timestamp:   ts,
			StatusCode:  col(row, colIdx, "statuscode"),
			MIMEType:    col(row, colIdx, "mimetype"),
			Digest:      col(row, colIdx, "digest"),
		})
	}

	return snapshots, nil
}

// col safely retrieves a field from a CDX row by column name.
func col(row []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(row) {
		return ""
	}
	return row[i]
}
