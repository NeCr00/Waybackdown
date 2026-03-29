package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

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
	"github.com/NeCr00/Waybackdown/internal/transport"
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

	// Shared transport pools TCP+TLS connections across all providers and
	// the downloader, eliminating repeated handshake overhead.
	tr := transport.New()

	// Global rate limiter: governs downloads and non-CC CDX queries.
	var limiter *ratelimit.Limiter
	if cfg.RPS > 0 {
		limiter = ratelimit.New(cfg.RPS, cfg.BurstSize)
	}

	// Separate, more permissive rate limiter for Common Crawl CDX queries.
	var ccLimiter *ratelimit.Limiter
	if cfg.CCRPS > 0 {
		ccLimiter = ratelimit.New(cfg.CCRPS, cfg.CCBurst)
	}

	providers := buildProviders(cfg, limiter, ccLimiter, tr)
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid providers specified (check -providers flag)")
		os.Exit(1)
	}

	disp := ui.New(len(urls), cfg.OutputDir)
	disp.Banner(cfg.Mode, cfg.Providers)
	disp.Start()

	if cfg.Verbose {
		disp.Info("rate limiter: %.1f req/s, burst %d", cfg.RPS, cfg.BurstSize)
		disp.Info("CC rate limiter: %.1f req/s, burst %d", cfg.CCRPS, cfg.CCBurst)
		disp.Info("concurrency: %d URLs  dl-workers: %d/URL", cfg.Concurrency, cfg.DLWorkers)
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name()
		}
		disp.Info("providers (parallel fan-out): %s", strings.Join(names, " | "))
	}

	dl := downloader.New(cfg,
		downloader.WithLimiter(limiter),
		downloader.WithHTTPClient(&http.Client{
			Transport: tr,
			Timeout:   cfg.Timeout,
		}),
	)

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

	// Consumer runs concurrently with the submit loop so results appear in
	// real time instead of only after all URLs have been dispatched.
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
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
	}()

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

	// Wait for all workers, then signal the consumer to exit.
	wg.Wait()
	close(results)
	<-consumerDone

	disp.Stop()
	disp.Summary()
}

// contentFetcher is an optional interface for providers that use custom
// download logic (e.g. Common Crawl WARC byte-range extraction).
type contentFetcher interface {
	FetchContent(ctx context.Context, snap provider.Snapshot, destPath string) error
}

// buildProviders constructs the ordered provider slice from cfg.Providers.
// All providers share the tuned transport tr; each gets an appropriate
// per-timeout HTTP client and the relevant rate limiter.
func buildProviders(cfg *config.Config, limiter, ccLimiter *ratelimit.Limiter, tr *http.Transport) []provider.Provider {
	makeClient := func(mult time.Duration) *http.Client {
		return &http.Client{Transport: tr, Timeout: cfg.Timeout * mult}
	}

	var providers []provider.Provider
	for _, name := range strings.Split(cfg.Providers, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		switch name {
		case "wayback":
			providers = append(providers, wayback.New(cfg,
				wayback.WithLimiter(limiter),
				wayback.WithHTTPClient(makeClient(3))))
		case "archiveph":
			providers = append(providers, archiveph.New(cfg,
				archiveph.WithLimiter(limiter),
				archiveph.WithHTTPClient(makeClient(2))))
		case "commoncrawl":
			providers = append(providers, commoncrawl.New(cfg,
				commoncrawl.WithLimiter(ccLimiter),
				commoncrawl.WithHTTPClient(makeClient(3))))
		case "arquivo":
			providers = append(providers, arquivo.New(cfg,
				arquivo.WithLimiter(limiter),
				arquivo.WithHTTPClient(makeClient(2))))
		}
	}
	return providers
}

// fetchWithFallback queries all providers concurrently and returns the first
// non-empty result, honouring provider priority order (lower index = higher
// priority).  Once a winner is determined (all higher-priority providers have
// returned), the remaining goroutines are cancelled to avoid wasting tokens.
func fetchWithFallback(
	ctx context.Context,
	rawURL string,
	providers []provider.Provider,
	cfg *config.Config,
	disp *ui.Display,
) ([]provider.Snapshot, provider.Provider) {
	n := len(providers)
	if n == 0 {
		return nil, nil
	}

	type result struct {
		idx   int
		snaps []provider.Snapshot
		p     provider.Provider
	}

	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan result, n)
	for i, p := range providers {
		i, p := i, p
		go func() {
			snaps, err := p.FetchSnapshots(fanCtx, rawURL)
			if err != nil {
				if cfg.Verbose && fanCtx.Err() == nil {
					disp.Info("%s: fetch failed: %v", p.Name(), err)
				}
				ch <- result{idx: i, p: p}
				return
			}
			if cfg.Verbose {
				if len(snaps) > 0 {
					disp.Info("%s: %d snapshot(s) found", p.Name(), len(snaps))
				} else {
					disp.Info("%s: no snapshots", p.Name())
				}
			}
			ch <- result{idx: i, snaps: snaps, p: p}
		}()
	}

	// pending tracks which provider indices are still running.
	pending := make(map[int]bool, n)
	for i := range providers {
		pending[i] = true
	}

	bestIdx := n // sentinel: no winner yet
	var bestSnaps []provider.Snapshot
	var bestProvider provider.Provider

	for len(pending) > 0 {
		r := <-ch
		delete(pending, r.idx)

		if len(r.snaps) > 0 && r.idx < bestIdx {
			bestIdx = r.idx
			bestSnaps = r.snaps
			bestProvider = r.p
		}

		// Declare a winner once no pending provider has a higher priority
		// (lower index) than the current best.
		if bestIdx < n {
			higherPriorityPending := false
			for pendIdx := range pending {
				if pendIdx < bestIdx {
					higherPriorityPending = true
					break
				}
			}
			if !higherPriorityPending {
				cancel() // cancel remaining lower-priority queries
				return bestSnaps, bestProvider
			}
		}
	}

	return bestSnaps, bestProvider
}

// processURL normalises, queries all providers in parallel (fallback by
// priority), selects snapshots, and downloads them with parallel workers.
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

	snapshots, usedProvider := fetchWithFallback(ctx, normalized, providers, cfg, disp)

	if len(snapshots) == 0 {
		return 0, 0, 0, true, "no snapshots found in any archive", nil
	}

	selected := selector.Select(snapshots, cfg.Mode, cfg.MaxSnapshots)
	if cfg.Verbose {
		disp.Info("%s → %d in archive, %d selected (%s) via %s",
			normalized, len(snapshots), len(selected), cfg.Mode, usedProvider.Name())
	}

	fetcher, hasFetcher := usedProvider.(contentFetcher)

	newFiles, cached, dlFailed = downloadAll(ctx, selected, cfg, dl, fetcher, hasFetcher, normalized, disp)
	return newFiles, cached, dlFailed, false, "", nil
}

// downloadAll downloads all selected snapshots using a pool of cfg.DLWorkers
// goroutines.  Cache hits are detected synchronously before queuing.
func downloadAll(
	ctx context.Context,
	selected []provider.Snapshot,
	cfg *config.Config,
	dl *downloader.Downloader,
	fetcher contentFetcher,
	hasFetcher bool,
	normalized string,
	disp *ui.Display,
) (newFiles, cached, dlFailed int) {
	type job struct {
		snap     provider.Snapshot
		destPath string
	}

	jobs := make([]job, 0, len(selected))
	for _, snap := range selected {
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
		jobs = append(jobs, job{snap: snap, destPath: destPath})
	}

	if len(jobs) == 0 {
		return 0, cached, 0
	}

	workers := cfg.DLWorkers
	if workers > len(jobs) {
		workers = len(jobs)
	}

	jobCh := make(chan job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				if ctx.Err() != nil {
					return
				}
				var dlErr error
				if hasFetcher {
					dlErr = fetcher.FetchContent(ctx, j.snap, j.destPath)
				} else {
					dlErr = dl.Download(ctx, j.snap.ArchiveURL, j.destPath)
				}
				mu.Lock()
				if dlErr != nil {
					disp.DlWarn(normalized, j.snap.Timestamp.Format("20060102"), dlErr)
					dlFailed++
				} else {
					newFiles++
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return newFiles, cached, dlFailed
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
