package selector

import (
	"testing"
	"time"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
)

func ts(s string) time.Time {
	t, _ := time.Parse("20060102150405", s)
	return t
}

var testSnaps = []provider.Snapshot{
	{Timestamp: ts("20100101000000"), Digest: "aaa", OriginalURL: "http://example.com"},
	{Timestamp: ts("20150601000000"), Digest: "bbb", OriginalURL: "http://example.com"},
	{Timestamp: ts("20200101000000"), Digest: "ccc", OriginalURL: "http://example.com"},
	{Timestamp: ts("20230601000000"), Digest: "bbb", OriginalURL: "http://example.com"}, // dup of bbb
	{Timestamp: ts("20231201000000"), Digest: "ddd", OriginalURL: "http://example.com"},
}

func TestSelectOldest(t *testing.T) {
	got := Select(testSnaps, config.ModeOldest, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(got))
	}
	if !got[0].Timestamp.Equal(ts("20100101000000")) {
		t.Errorf("unexpected oldest timestamp: %v", got[0].Timestamp)
	}
}

func TestSelectNewest(t *testing.T) {
	got := Select(testSnaps, config.ModeNewest, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(got))
	}
	if !got[0].Timestamp.Equal(ts("20231201000000")) {
		t.Errorf("unexpected newest timestamp: %v", got[0].Timestamp)
	}
}

func TestSelectAll_Dedup(t *testing.T) {
	got := Select(testSnaps, config.ModeAll, 0)
	// 5 snapshots, but 2 share digest "bbb" → 4 unique
	if len(got) != 4 {
		t.Errorf("expected 4 deduplicated snapshots, got %d", len(got))
	}
}

func TestSelectAll_Max(t *testing.T) {
	got := Select(testSnaps, config.ModeAll, 2)
	if len(got) != 2 {
		t.Errorf("expected 2 snapshots (max applied), got %d", len(got))
	}
}

func TestSelectEmpty(t *testing.T) {
	got := Select(nil, config.ModeNewest, 0)
	if got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestDedup_EmptyDigest(t *testing.T) {
	snaps := []provider.Snapshot{
		{Timestamp: ts("20200101000000"), Digest: ""},
		{Timestamp: ts("20210101000000"), Digest: ""},
	}
	// Empty digests are always kept
	got := dedup(snaps)
	if len(got) != 2 {
		t.Errorf("expected 2 (empty digests kept), got %d", len(got))
	}
}
