package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/NeCr00/Waybackdown/internal/config"
	"github.com/NeCr00/Waybackdown/internal/downloader"
	"github.com/NeCr00/Waybackdown/internal/normalize"
	"github.com/NeCr00/Waybackdown/internal/output"
	"github.com/NeCr00/Waybackdown/internal/provider"
	"github.com/NeCr00/Waybackdown/internal/provider/archiveph"
	"github.com/NeCr00/Waybackdown/internal/provider/arquivo"
	"github.com/NeCr00/Waybackdown/internal/provider/commoncrawl"
	"github.com/NeCr00/Waybackdown/internal/provider/wayback"
	"github.com/NeCr00/Waybackdown/internal/ratelimit"
	"github.com/NeCr00/Waybackdown/internal/selector"
	"github.com/NeCr00/Waybackdown/internal/ui"
)

func main() {
	cfg := config.Parse()

	urls, err := collectURLs(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "error: no URLs to process")
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output directory %q: %v\n", cfg.OutputDir, err)
		os.Exit(1)
	}

	var limiter *ratelimit.Limiter
	if cfg.RPS > 0 {
		limiter = ratelimit.New(cfg.RPS, cfg.BurstSize)
	}

	providers := buildProviders(cfg, limiter)
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid providers specified (check -providers flag)")
		os.Exit(1)
	}

	disp := ui.New(len(urls))
	disp.Banner(cfg.Mode, cfg.Providers)
	disp.Start()

	if cfg.Verbose {
		disp.Info("rate limiter: %.1f req/s, burst %d", cfg.RPS, cfg.BurstSize)
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name()
		}
		disp.Info("providers: %s", strings.Join(names, " → "))
	}

	dl := downloader.New(cfg, downloader.WithLimiter(limiter))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	type jobResult struct {
		url      string
		newFiles int
		cached   int
		dlFailed int
		skipped  bool
		reason   string
		err      error
	}

	results := make(chan jobResult, len(urls))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

submitLoop:
	for _, u := range urls {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "\ninterrupted — waiting for in-flight downloads to finish...")
			break submitLoop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(rawURL string) {
			defer wg.Done()
			defer func() { <-sem }()

			nNew, nCached, nFailed, skipped, reason, err := processURL(ctx, rawURL, cfg, providers, dl, disp)
			results <- jobResult{
				url:      rawURL,
				newFiles: nNew,
				cached:   nCached,
				dlFailed: nFailed,
				skipped:  skipped,
				reason:   reason,
				err:      err,
			}
		}(u)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		switch {
		case r.err != nil:
			disp.Fail(r.url, r.err)
		case r.skipped:
			disp.Skip(r.url, r.reason)
		default:
			disp.Ok(r.url, r.newFiles, r.cached, r.dlFailed)
		}
	}

	disp.Stop()
	disp.Summary(cfg.OutputDir)
}

// contentFetcher is an optional interface for providers that use custom
// download logic (e.g. Common Crawl WARC byte-range extraction).
type contentFetcher interface {
	FetchContent(ctx context.Context, snap provider.Snapshot, destPath string) error
}

// buildProviders constructs the ordered provider slice from cfg.Providers.
func buildProviders(cfg *config.Config, limiter *ratelimit.Limiter) []provider.Provider {
	var providers []provider.Provider
	for _, name := range strings.Split(cfg.Providers, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		switch name {
		case "wayback":
			providers = append(providers, wayback.New(cfg, wayback.WithLimiter(limiter)))
		case "archiveph":
			providers = append(providers, archiveph.New(cfg, archiveph.WithLimiter(limiter)))
		case "commoncrawl":
			providers = append(providers, commoncrawl.New(cfg, commoncrawl.WithLimiter(limiter)))
		case "arquivo":
			providers = append(providers, arquivo.New(cfg, arquivo.WithLimiter(limiter)))
		}
	}
	return providers
}

// processURL normalises, queries providers in fallback order, selects
// snapshots, and downloads them.
func processURL(
	ctx context.Context,
	rawURL string,
	cfg *config.Config,
	providers []provider.Provider,
	dl *downloader.Downloader,
	disp *ui.Display,
) (newFiles, cached, dlFailed int, skipped bool, reason string, err error) {

	normalized, normErr := normalize.URL(rawURL)
	if normErr != nil {
		return 0, 0, 0, true, fmt.Sprintf("invalid URL: %v", normErr), nil
	}

	if cfg.Verbose {
		disp.Info("querying %s", normalized)
	}

	var snapshots []provider.Snapshot
	var usedProvider provider.Provider
	for _, p := range providers {
		snaps, fetchErr := p.FetchSnapshots(ctx, normalized)
		if fetchErr != nil {
			if cfg.Verbose {
				disp.Info("%s: fetch failed: %v", p.Name(), fetchErr)
			}
			continue
		}
		if len(snaps) > 0 {
			snapshots = snaps
			usedProvider = p
			if cfg.Verbose {
				disp.Info("%s: %d snapshot(s) found", p.Name(), len(snaps))
			}
			break
		}
		if cfg.Verbose {
			disp.Info("%s: no snapshots — trying next provider", p.Name())
		}
	}

	if len(snapshots) == 0 {
		return 0, 0, 0, true, "no snapshots found in any archive", nil
	}

	selected := selector.Select(snapshots, cfg.Mode, cfg.MaxSnapshots)
	if cfg.Verbose {
		disp.Info("%s → %d in archive, %d selected (%s) via %s",
			normalized, len(snapshots), len(selected), cfg.Mode, usedProvider.Name())
	}

	fetcher, hasFetcher := usedProvider.(contentFetcher)

	for _, snap := range selected {
		if ctx.Err() != nil {
			return newFiles, cached, dlFailed, false, "", ctx.Err()
		}

		destPath, pathErr := output.FilePath(cfg.OutputDir, snap)
		if pathErr != nil {
			if cfg.Verbose {
				disp.Info("cannot compute path for %s: %v", snap.ArchiveURL, pathErr)
			}
			continue
		}

		if _, statErr := os.Stat(destPath); statErr == nil {
			if cfg.Verbose {
				disp.Info("cached: %s", destPath)
			}
			cached++
			continue
		}

		if cfg.Verbose {
			disp.Down(snap.ArchiveURL, destPath)
		}

		var dlErr error
		if hasFetcher {
			dlErr = fetcher.FetchContent(ctx, snap, destPath)
		} else {
			dlErr = dl.Download(ctx, snap.ArchiveURL, destPath)
		}

		if dlErr != nil {
			disp.DlWarn(normalized, snap.Timestamp.Format("20060102"), dlErr)
			dlFailed++
			continue
		}
		newFiles++
	}

	return newFiles, cached, dlFailed, false, "", nil
}

// collectURLs reads URLs from -u and/or -l, deduplicates, strips comments.
func collectURLs(cfg *config.Config) ([]string, error) {
	seen := make(map[string]struct{})
	var urls []string

	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			return
		}
		if _, exists := seen[line]; !exists {
			seen[line] = struct{}{}
			urls = append(urls, line)
		}
	}

	if cfg.SingleURL != "" {
		add(cfg.SingleURL)
	}

	if cfg.URLFile != "" {
		f, err := os.Open(cfg.URLFile)
		if err != nil {
			return nil, fmt.Errorf("open URL file %q: %w", cfg.URLFile, err)
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			add(sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read URL file: %w", err)
		}
	}

	return urls, nil
}
