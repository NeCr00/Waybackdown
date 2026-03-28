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
