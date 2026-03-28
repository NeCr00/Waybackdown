// Package archiveph implements the provider.Provider interface for archive.ph /
// archive.today using the Memento timemap protocol.
package archiveph

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

const (
	defaultTimemapBase = "https://archive.ph/timemap/"
	timestampLayout    = "20060102150405"
)

// timestampRe extracts a 14-digit timestamp from an archive.ph / archive.md URL path.
var timestampRe = regexp.MustCompile(`/(\d{14})/`)

// Client is an archive.ph archive provider.
type Client struct {
	cfg         *config.Config
	client      *http.Client
	limiter     *ratelimit.Limiter
	timemapBase string // overridable in tests
}

// Option configures a Client.
type Option func(*Client)

// WithLimiter attaches a shared rate limiter.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(c *Client) { c.limiter = l }
}

// WithTimemapBase overrides the timemap base URL. Used in tests.
func WithTimemapBase(base string) Option {
	return func(c *Client) { c.timemapBase = base }
}

// WithHTTPClient replaces the internal HTTP client. Used in tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.client = hc }
}

// New returns a new archive.ph Client.
func New(cfg *config.Config, opts ...Option) *Client {
	c := &Client{
		cfg:         cfg,
		timemapBase: defaultTimemapBase,
		client:      &http.Client{Timeout: cfg.Timeout * 2},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "archiveph" }

// FetchSnapshots queries the archive.ph timemap for all snapshots of the given URL.
// 4xx responses are treated as "no results" (archive.ph blocks bots aggressively).
func (c *Client) FetchSnapshots(ctx context.Context, rawURL string) ([]provider.Snapshot, error) {
	timemapURL := c.timemapBase + rawURL
	if c.cfg.Verbose {
		fmt.Printf("[archiveph] timemap: %s\n", timemapURL)
	}

	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, timemapURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "waybackdown/1.0 (archive downloader)")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Treat all 4xx as "no results" — archive.ph often returns 404 for
	// unmemoised URLs and 403/429 when blocking bots.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		if c.cfg.Verbose {
			fmt.Printf("[archiveph] HTTP %d — skipping\n", resp.StatusCode)
		}
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("archive.ph timemap HTTP %d", resp.StatusCode)
	}

	snaps, err := parseTimemap(resp.Body, rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse timemap: %w", err)
	}
	return snaps, nil
}

// parseTimemap parses a Memento application/link-format response body.
// Each entry is on a single line ending with a comma (except the last).
// Only entries whose rel attribute contains "memento" are returned as snapshots.
func parseTimemap(r io.Reader, originalURL string) ([]provider.Snapshot, error) {
	var snaps []provider.Snapshot
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Strip trailing comma (entry separator in link-format).
		line = strings.TrimSuffix(line, ",")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if snap, ok := parseLinkEntry(line, originalURL); ok {
			snaps = append(snaps, snap)
		}
	}
	return snaps, scanner.Err()
}

// parseLinkEntry parses a single link-format entry, e.g.:
//
//	<http://archive.md/20260223033330/https://example.com/>; rel="memento"; datetime="Mon, 23 Feb 2026 03:33:30 GMT"
func parseLinkEntry(entry, originalURL string) (provider.Snapshot, bool) {
	// Must be a memento entry (not the timemap self-link or original).
	if !strings.Contains(entry, "memento") {
		return provider.Snapshot{}, false
	}

	// Extract the URL between < and >.
	start := strings.IndexByte(entry, '<')
	end := strings.IndexByte(entry, '>')
	if start < 0 || end <= start {
		return provider.Snapshot{}, false
	}
	archiveURL := entry[start+1 : end]
	if archiveURL == "" {
		return provider.Snapshot{}, false
	}

	// Extract timestamp from the archive URL path (most reliable).
	var ts time.Time
	if m := timestampRe.FindStringSubmatch(archiveURL); len(m) >= 2 {
		if t, err := time.Parse(timestampLayout, m[1]); err == nil {
			ts = t
		}
	}
	// Fallback: parse the datetime= attribute value.
	if ts.IsZero() {
		const dtKey = `datetime="`
		if i := strings.Index(entry, dtKey); i >= 0 {
			rest := entry[i+len(dtKey):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				if t, err := http.ParseTime(rest[:j]); err == nil {
					ts = t
				}
			}
		}
	}
	if ts.IsZero() {
		return provider.Snapshot{}, false
	}

	return provider.Snapshot{
		OriginalURL: originalURL,
		ArchiveURL:  archiveURL,
		Timestamp:   ts,
		// StatusCode and MIMEType are not available in the timemap response.
	}, true
}
