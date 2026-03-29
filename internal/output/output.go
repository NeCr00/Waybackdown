// Package output handles file-path generation and directory layout for
// downloaded snapshots.
package output

import (
	"fmt"
	"hash/crc32"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/NeCr00/Waybackdown/internal/provider"
)

// unsafeChars matches any character that should not appear in a file/dir name.
var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// FilePath returns the canonical destination path for a snapshot:
//
//	<outputDir>/<host>/<sanitized_path>/<timestamp>_<status>.<ext>
//
// Example:
//
//	waybackdown_output/example.com/about_us/20230101120000_200.html
func FilePath(outputDir string, snap provider.Snapshot) (string, error) {
	u, err := url.Parse(snap.OriginalURL)
	if err != nil {
		return "", fmt.Errorf("parse original URL: %w", err)
	}

	host := sanitize(u.Host)
	// Collapse any ".." sequences that survive sanitize (dots are allowed by the
	// regex) to prevent path traversal when filepath.Join resolves the host
	// component against the output directory.
	host = strings.ReplaceAll(host, "..", "_")
	if host == "" {
		host = "unknown_host"
	}

	// Include the query string so that URLs with identical paths but different
	// query parameters (e.g. ?id=1 vs ?id=2) land in distinct directories.
	// Without this, the second URL would be falsely reported as "already cached".
	pathRaw := u.Path
	if u.RawQuery != "" {
		pathRaw += "?" + u.RawQuery
	}
	pathPart := sanitizePath(pathRaw)
	if pathPart == "" {
		pathPart = "root"
	}
	// Guard against extremely long path components (filesystem limit ~255 chars).
	// Keep the first 160 chars readable, append an 8-hex-char CRC for uniqueness.
	if len(pathPart) > 200 {
		h := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(pathRaw)))
		pathPart = pathPart[:160] + "_" + h
	}

	ts := snap.Timestamp.Format("20060102150405")

	status := snap.StatusCode
	if status == "" {
		status = "000"
	}

	ext := extFromMIME(snap.MIMEType)
	if ext == "" {
		ext = extFromURLPath(u.Path)
	}
	if ext == "" {
		ext = "bin"
	}

	filename := fmt.Sprintf("%s_%s.%s", ts, status, ext)
	return filepath.Join(outputDir, host, pathPart, filename), nil
}

// sanitize replaces characters not safe in filenames with underscores.
func sanitize(s string) string {
	return unsafeChars.ReplaceAllString(strings.TrimSpace(s), "_")
}

// sanitizePath converts a URL path into a single safe directory component.
// Slashes become underscores; leading/trailing slashes are stripped.
func sanitizePath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	path = strings.ReplaceAll(path, "/", "_")
	return sanitize(path)
}

// extFromMIME maps common MIME types to file extensions.
func extFromMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i]) // strip parameters
	}
	switch strings.ToLower(mime) {
	case "text/html":
		return "html"
	case "text/plain":
		return "txt"
	case "text/css":
		return "css"
	case "application/javascript", "text/javascript":
		return "js"
	case "application/json":
		return "json"
	case "application/xml", "text/xml":
		return "xml"
	case "application/pdf":
		return "pdf"
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/svg+xml":
		return "svg"
	case "application/zip":
		return "zip"
	case "application/gzip", "application/x-gzip":
		return "gz"
	case "application/x-tar":
		return "tar"
	case "font/woff":
		return "woff"
	case "font/woff2":
		return "woff2"
	}
	return ""
}

// extFromURLPath extracts a file extension from the last segment of a URL
// path.  Only returns extensions that look plausible (1–5 alphanumeric chars).
func extFromURLPath(path string) string {
	base := filepath.Base(path)
	if base == "." || base == "/" {
		return ""
	}
	idx := strings.LastIndexByte(base, '.')
	if idx <= 0 || idx == len(base)-1 {
		return ""
	}
	ext := strings.ToLower(base[idx+1:])
	if len(ext) < 1 || len(ext) > 5 {
		return ""
	}
	for _, c := range ext {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return ""
		}
	}
	return ext
}
