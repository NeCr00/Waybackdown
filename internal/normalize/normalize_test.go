package normalize

import "testing"

func TestURLKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Scheme is stripped; case is normalised.
		{"https stripped", "https://example.com/path", "example.com/path"},
		{"http stripped", "http://example.com/path", "example.com/path"},
		{"case normalised", "https://EXAMPLE.COM/Path", "example.com/Path"},

		// Root-domain matching: bare host must produce the same key as host
		// with trailing slash, because CDX APIs always record root URLs with "/".
		{"bare host → slash", "https://example.com", "example.com/"},
		{"trailing slash", "https://example.com/", "example.com/"},
		{"http bare host → slash", "http://example.com", "example.com/"},

		// Query string is preserved.
		{"with query", "https://example.com/p?q=1&r=2", "example.com/p?q=1&r=2"},

		// Standard ports stripped; non-standard ports retained.
		{"standard https port stripped", "https://example.com:443/p", "example.com/p"},
		{"standard http port stripped", "http://example.com:80/p", "example.com/p"},
		{"non-standard port kept", "https://example.com:8443/p", "example.com:8443/p"},

		// http and https produce the same key (scheme-agnostic matching).
		{"http vs https same key", "http://example.com/path", "example.com/path"},

		// www. prefix normalization: CDX SURT indexes www.X.com under the same
		// key as X.com.  Queries for X.com/* return entries with original=
		// "http://www.X.com:80/"; those must match user queries for X.com/*.
		{"www stripped root", "http://www.example.com:80/", "example.com/"},
		{"www stripped path", "https://www.example.com/about", "example.com/about"},
		{"www stripped query", "http://www.example.com/?q=1", "example.com/?q=1"},
		{"www non-standard port kept", "https://www.example.com:8443/p", "example.com:8443/p"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := URLKey(tc.input)
			if got != tc.want {
				t.Errorf("URLKey(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}

	// Verify the key fix: bare host and host with trailing slash produce
	// identical keys so root-domain inventory entries always match.
	t.Run("bare host matches trailing slash", func(t *testing.T) {
		k1 := URLKey("https://example.com")
		k2 := URLKey("https://example.com/")
		if k1 != k2 {
			t.Errorf("URLKey with/without trailing slash differ: %q vs %q", k1, k2)
		}
	})
	t.Run("http and https same key", func(t *testing.T) {
		k1 := URLKey("https://example.com/path")
		k2 := URLKey("http://example.com/path")
		if k1 != k2 {
			t.Errorf("URLKey http vs https differ: %q vs %q", k1, k2)
		}
	})
	// Regression: CDX returns original="http://www.X.com:80/" for oldest entries;
	// that must match user query for X.com (the bug that caused oldest-mode to miss
	// root snapshots of neurosoft.gr when they were captured as www.neurosoft.gr:80).
	t.Run("www with explicit port 80 matches non-www", func(t *testing.T) {
		cdxKey := URLKey("http://www.neurosoft.gr:80/")
		userKey := URLKey("https://neurosoft.gr")
		if cdxKey != userKey {
			t.Errorf("www+port80 CDX key %q != user key %q", cdxKey, userKey)
		}
	})
}

func TestHost(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"simple https", "https://example.com/path", "example.com", false},
		{"uppercase normalised", "https://EXAMPLE.COM/", "example.com", false},
		{"standard https port stripped", "https://example.com:443/p", "example.com", false},
		{"standard http port stripped", "http://example.com:80/p", "example.com", false},
		{"non-standard port retained", "https://example.com:8443/p", "example.com:8443", false},
		{"no scheme gets https", "example.com/path", "example.com", false},
		{"www stripped", "https://www.example.com/path", "example.com", false},
		{"www stripped with port", "http://www.example.com:80/", "example.com", false},
		{"empty URL is error", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Host(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Host(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("Host(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"full https URL", "https://example.com/path", "https://example.com/path", false},
		{"full http URL", "http://example.com/", "http://example.com/", false},
		{"no scheme → https added", "example.com/path", "https://example.com/path", false},
		{"uppercase scheme normalised", "HTTPS://EXAMPLE.COM/", "https://example.com/", false},
		{"trailing space stripped", "  https://example.com  ", "https://example.com", false},
		// Fragment stripping: archives never store URL fragments; including them
		// in queries would return zero results.
		{"fragment stripped", "https://example.com/page#section", "https://example.com/page", false},
		{"fragment-only stripped", "https://example.com/#top", "https://example.com/", false},
		{"empty string", "", "", true},
		{"scheme only", "https://", "", true},
		{"unsupported scheme", "ftp://example.com", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := URL(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("URL(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestToggleScheme(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://example.com", "http://example.com"},
		{"http://example.com/path", "https://example.com/path"},
		{"ftp://example.com", "ftp://example.com"}, // unchanged
	}
	for _, tc := range tests {
		got := ToggleScheme(tc.input)
		if got != tc.want {
			t.Errorf("ToggleScheme(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
