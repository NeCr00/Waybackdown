package normalize

import "testing"

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
