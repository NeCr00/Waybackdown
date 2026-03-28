// Package selector picks snapshots from a list according to the chosen mode.
package selector

import (
	"sort"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/provider"
)

// Select returns the subset of snapshots chosen by mode:
//
//   - oldest  → the single earliest snapshot by timestamp
//   - newest  → the single latest snapshot by timestamp
//   - all     → all snapshots, deduplicated by content digest,
//     optionally capped at max (0 = no cap)
func Select(snapshots []provider.Snapshot, mode string, max int) []provider.Snapshot {
	if len(snapshots) == 0 {
		return nil
	}

	// Work on a sorted copy; CDX usually returns ascending order but we
	// sort explicitly for correctness when multiple providers are merged.
	sorted := make([]provider.Snapshot, len(snapshots))
	copy(sorted, snapshots)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	switch mode {
	case config.ModeOldest:
		return sorted[:1]

	case config.ModeNewest:
		return sorted[len(sorted)-1:]

	case config.ModeAll:
		result := dedup(sorted)
		if max > 0 && len(result) > max {
			result = result[:max]
		}
		return result

	default:
		return sorted[len(sorted)-1:]
	}
}

// dedup removes snapshots that share the same content digest, keeping the
// earliest occurrence of each.  Snapshots with an empty digest are kept as-is
// since we cannot determine whether their content is identical.
func dedup(snapshots []provider.Snapshot) []provider.Snapshot {
	seen := make(map[string]struct{}, len(snapshots))
	out := make([]provider.Snapshot, 0, len(snapshots))
	for _, s := range snapshots {
		if s.Digest == "" {
			out = append(out, s)
			continue
		}
		if _, exists := seen[s.Digest]; !exists {
			seen[s.Digest] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
