// Package normalize provides URL validation and normalization.
package normalize

import (
	"fmt"
	"net/url"
	"strings"
)

// URL validates and normalizes a raw URL string.
// If the URL has no scheme, "https://" is assumed.
// Returns the canonical URL string or an error if the URL is unusable.
func URL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty URL")
	}

	// Assume https when scheme is missing
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}

	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported scheme %q (only http/https are supported)", u.Scheme)
	}

	// Strip fragment — fragments are client-side navigation hints and are not
	// part of the resource identity.  Including them in archive queries would
	// return no results (archive services never store fragments).
	u.Fragment = ""
	u.RawFragment = ""

	return u.String(), nil
}

// ToggleScheme returns the same URL with the scheme swapped (http↔https).
// Used to retry queries against alternative scheme archives.
func ToggleScheme(rawURL string) string {
	if strings.HasPrefix(rawURL, "https://") {
		return "http://" + rawURL[8:]
	}
	if strings.HasPrefix(rawURL, "http://") {
		return "https://" + rawURL[7:]
	}
	return rawURL
}

// Host extracts the lowercased canonical host (hostname + non-standard port)
// from a URL, suitable for use as a CDX query key.
//
// Standard ports (80 for http, 443 for https) are omitted.  The "www." prefix
// is stripped to match CDX SURT normalisation: archive services index
// http://www.X.com/ and http://X.com/ under the same SURT key, so both should
// produce the same CDX query (url=X.com/*).
func Host(rawURL string) (string, error) {
	n, err := URL(rawURL)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(n)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasPrefix(host, "www.") {
		host = host[4:]
	}
	port := u.Port()
	if port != "" && port != "80" && port != "443" {
		host += ":" + port
	}
	return host, nil
}

// URLKey returns a scheme-free canonical key for matching a user-supplied URL
// against archive CDX inventory entries.  The following normalisations are
// applied so that user queries and CDX OriginalURL fields produce identical
// keys regardless of how the URL was originally crawled:
//
//   - Scheme stripped (http/https treated as equivalent)
//   - Host lowercased
//   - "www." prefix stripped — CDX SURT normalization indexes
//     http://www.X.com/ and http://X.com/ under the same key, so querying
//     url=X.com/* returns entries with original="http://www.X.com:80/".
//     Without stripping www., those entries would never match a user query
//     for X.com (e.g. oldest-mode snapshots of neurosoft.gr were captured
//     as www.neurosoft.gr:80 and were silently missed before this fix).
//   - Standard ports (80/443) stripped
//   - Empty path normalised to "/" — CDX always records root URLs with a
//     trailing slash; bare-host inputs must produce the same key.
//
// Examples:
//
//	"https://Example.COM/path?q=1"        → "example.com/path?q=1"
//	"https://example.com"                 → "example.com/"
//	"https://example.com/"                → "example.com/"
//	"http://www.example.com:80/"          → "example.com/"
//	"https://www.example.com/about"       → "example.com/about"
func URLKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.ToLower(rawURL)
	}
	host := strings.ToLower(u.Hostname())
	// Strip www. prefix to align with CDX SURT normalization.
	host = strings.TrimPrefix(host, "www.")
	port := u.Port()
	if port != "" && port != "80" && port != "443" {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		// CDX archives always record the root URL with a trailing slash.
		// Normalise bare-host inputs to "/" so they match inventory entries.
		path = "/"
	}
	key := host + path
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}
