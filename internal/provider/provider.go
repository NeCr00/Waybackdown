// Package provider defines the archive provider interface and shared types.
// New archive sources (e.g. Common Crawl, Google Cache, archive.ph) can be
// added by implementing Provider without touching any other package.
package provider

import (
	"context"
	"time"
)

// Snapshot represents one archived capture of a URL.
type Snapshot struct {
	OriginalURL string    // URL that was archived
	ArchiveURL  string    // URL to retrieve the archived content
	Timestamp   time.Time // when the snapshot was taken
	StatusCode  string    // HTTP status code at capture time
	MIMEType    string    // MIME type reported at capture time
	Digest      string    // content digest (SHA-1 or similar) for deduplication
}

// Provider is the interface that all archive source implementations must satisfy.
// Each method must honour context cancellation.
type Provider interface {
	// Name returns a short identifier for the provider (e.g. "wayback").
	Name() string

	// FetchSnapshots returns all snapshots available for the given URL.
	// Filtering (e.g. by status code) is provider-specific and controlled
	// through the Config passed at construction time.
	// Returns nil, nil when no snapshots exist (not an error).
	FetchSnapshots(ctx context.Context, url string) ([]Snapshot, error)
}
