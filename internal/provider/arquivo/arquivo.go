// Package arquivo implements the provider.Provider interface for Arquivo.pt,
// the Portuguese web archive.  The CDX API returns NDJSON; content is
// served via the noFrame replay endpoint.
package arquivo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
)

const (
	defaultCDXEndpoint = "https://arquivo.pt/wayback/cdx"
	replayBase         = "https://arquivo.pt/noFrame/replay"
	archiveSuffix      = "id_"
	timestampLayout    = "20060102150405"
)

// Client is an Arquivo.pt archive provider.
type Client struct {
	cfg         *config.Config
	client      *http.Client
	limiter     *ratelimit.Limiter
	cdxEndpoint string // overridable in tests
}

// Option configures a Client.
type Option func(*Client)

// WithLimiter attaches a shared rate limiter.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(c *Client) { c.limiter = l }
}

// WithCDXEndpoint overrides the CDX endpoint. Used in tests.
func WithCDXEndpoint(endpoint string) Option {
	return func(c *Client) { c.cdxEndpoint = endpoint }
}

// WithHTTPClient replaces the internal HTTP client. Used in tests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.client = hc }
}

// New returns a new Arquivo.pt Client.
func New(cfg *config.Config, opts ...Option) *Client {
	c := &Client{
		cfg:         cfg,
		cdxEndpoint: defaultCDXEndpoint,
		client:      &http.Client{Timeout: cfg.Timeout * 2},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "arquivo" }

// FetchSnapshots queries the Arquivo.pt CDX API for snapshots of rawURL.
func (c *Client) FetchSnapshots(ctx context.Context, rawURL string) ([]provider.Snapshot, error) {
	apiURL := c.buildCDXURL(rawURL)
	if c.cfg.Verbose {
		fmt.Fprintf(os.Stderr,"[arquivo] CDX: %s\n", apiURL)
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
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arquivo CDX HTTP %d", resp.StatusCode)
	}

	return parseNDJSON(resp.Body, rawURL)
}

func (c *Client) buildCDXURL(targetURL string) string {
	p := url.Values{}
	p.Set("url", targetURL)
	p.Set("output", "json")

	switch c.cfg.Mode {
	case config.ModeOldest:
		p.Set("limit", "1")
	case config.ModeNewest:
		p.Set("sort", "reverse")
		p.Set("limit", "1")
	case config.ModeAll:
		if c.cfg.MaxSnapshots > 0 {
			p.Set("limit", strconv.Itoa(c.cfg.MaxSnapshots))
		}
	}

	if c.cfg.StatusFilter != "" {
		p.Set("filter", "statuscode:"+c.cfg.StatusFilter)
	}

	return c.cdxEndpoint + "?" + p.Encode()
}

// arquivoCDXRecord is one NDJSON line returned by the Arquivo.pt CDX API.
type arquivoCDXRecord struct {
	Timestamp string `json:"timestamp"`
	URL       string `json:"url"`
	MIME      string `json:"mime"`
	Status    string `json:"status"`
	Digest    string `json:"digest"`
}

// parseNDJSON parses the Arquivo.pt NDJSON CDX response.
func parseNDJSON(r io.Reader, originalURL string) ([]provider.Snapshot, error) {
	var snaps []provider.Snapshot
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec arquivoCDXRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // skip malformed lines silently
		}
		if rec.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(timestampLayout, rec.Timestamp)
		if err != nil {
			continue
		}
		orig := rec.URL
		if orig == "" {
			orig = originalURL
		}
		// Build replay URL: https://arquivo.pt/noFrame/replay/{timestamp}id_/{url}
		archiveURL := fmt.Sprintf("%s/%s%s/%s", replayBase, rec.Timestamp, archiveSuffix, orig)
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
