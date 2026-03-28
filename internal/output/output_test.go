package output

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/provider"
)

func snap(orig, mime, status string, t time.Time) provider.Snapshot {
	return provider.Snapshot{
		OriginalURL: orig,
		MIMEType:    mime,
		StatusCode:  status,
		Timestamp:   t,
	}
}

func ts(s string) time.Time {
	t, _ := time.Parse("20060102150405", s)
	return t
}

func TestFilePath_Structure(t *testing.T) {
	s := snap("https://example.com/about/team", "text/html", "200", ts("20230601120000"))
	path, err := FilePath("/out", s)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join("/out", "example.com", "about_team")
	wantFile := "20230601120000_200.html"
	if !strings.HasPrefix(path, wantDir) {
		t.Errorf("path %q does not start with %q", path, wantDir)
	}
	if !strings.HasSuffix(path, wantFile) {
		t.Errorf("path %q does not end with %q", path, wantFile)
	}
}

func TestFilePath_Root(t *testing.T) {
	s := snap("https://example.com/", "text/html", "200", ts("20230601120000"))
	path, err := FilePath("/out", s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "root") {
		t.Errorf("expected 'root' in path for root URL, got %q", path)
	}
}

func TestFilePath_ExtFromMIME(t *testing.T) {
	cases := []struct {
		mime string
		ext  string
	}{
		{"text/html", "html"},
		{"text/css", "css"},
		{"application/javascript", "js"},
		{"application/json", "json"},
		{"image/png", "png"},
		{"application/pdf", "pdf"},
		{"", "bin"}, // unknown → bin
	}
	for _, c := range cases {
		s := snap("https://example.com/file", c.mime, "200", ts("20230601120000"))
		path, _ := FilePath("/out", s)
		if !strings.HasSuffix(path, "."+c.ext) {
			t.Errorf("MIME %q → expected .%s suffix, got %q", c.mime, c.ext, path)
		}
	}
}

func TestFilePath_ExtFromURL(t *testing.T) {
	s := snap("https://example.com/style.css", "", "200", ts("20230601120000"))
	path, _ := FilePath("/out", s)
	if !strings.HasSuffix(path, ".css") {
		t.Errorf("expected .css from URL path, got %q", path)
	}
}

// TestFilePath_QueryStringDistinct verifies that two URLs with the same path
// but different query parameters land in different directories.
// Without this, ?id=1 would falsely appear "already cached" when ?id=2 was
// downloaded first — a critical bug for pentesting enumeration workflows.
func TestFilePath_QueryStringDistinct(t *testing.T) {
	s1 := snap("https://example.com/page?id=1", "text/html", "200", ts("20230601120000"))
	s2 := snap("https://example.com/page?id=2", "text/html", "200", ts("20230601120000"))
	p1, err1 := FilePath("/out", s1)
	p2, err2 := FilePath("/out", s2)
	if err1 != nil || err2 != nil {
		t.Fatalf("FilePath errors: %v, %v", err1, err2)
	}
	dir1 := filepath.Dir(p1)
	dir2 := filepath.Dir(p2)
	if dir1 == dir2 {
		t.Errorf("different query strings map to same directory: %q", dir1)
	}
}

// TestFilePath_SamePathSameDir verifies that the same URL always maps to
// the same directory (idempotency / resume support).
func TestFilePath_SamePathSameDir(t *testing.T) {
	s1 := snap("https://example.com/page?q=test", "text/html", "200", ts("20230601120000"))
	s2 := snap("https://example.com/page?q=test", "text/html", "200", ts("20230601120000"))
	p1, _ := FilePath("/out", s1)
	p2, _ := FilePath("/out", s2)
	if p1 != p2 {
		t.Errorf("same URL maps to different paths: %q vs %q", p1, p2)
	}
}

func TestSanitizePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/foo/bar/baz", "foo_bar_baz"},
		{"/", ""},
		{"", ""},
		{"/a b/c?d=e", "a_b_c_d_e"},
	}
	for _, c := range cases {
		got := sanitizePath(c.in)
		if got != c.want {
			t.Errorf("sanitizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtFromURLPath(t *testing.T) {
	cases := []struct {
		path string
		ext  string
	}{
		{"/file.html", "html"},
		{"/archive.tar.gz", "gz"},
		{"/no-ext", ""},
		{"/trailing.", ""},
		{"/toolong.abcdefgh", ""},
	}
	for _, c := range cases {
		got := extFromURLPath(c.path)
		if got != c.ext {
			t.Errorf("extFromURLPath(%q) = %q, want %q", c.path, got, c.ext)
		}
	}
}
