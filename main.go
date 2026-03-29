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

// urlJob represents one user-supplied URL to be resolved and downloaded.
type urlJob struct {
	input      string // original user input (for display)
	normalized string // after normalize.URL()
	key        string // scheme-stripped key for CDX inventory matching
	host       string // lowercased hostname (+ non-standard port)
}

// contentFetcher is an optional interface for providers that use custom
// download logic (e.g. Common Crawl WARC byte-range extraction).
type contentFetcher interface {
	FetchContent(ctx context.Context, snap provider.Snapshot, destPath string) error
}

func main() {
	cfg := config.Parse()

	rawURLs, err := collectURLs(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	jobs := buildJobs(rawURLs)
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid URLs to process")
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating output directory %q: %v\n", cfg.OutputDir, err)
		os.Exit(1)
	}

	tr := transport.New()

	var limiter *ratelimit.Limiter
	if cfg.RPS > 0 {
		limiter = ratelimit.New(cfg.RPS, cfg.BurstSize)
	}
	var ccLimiter *ratelimit.Limiter
	if cfg.CCRPS > 0 {
		ccLimiter = ratelimit.New(cfg.CCRPS, cfg.CCBurst)
	}

	providers := buildProviders(cfg, limiter, ccLimiter, tr)
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "error: no valid providers specified (check -providers flag)")
		os.Exit(1)
	}

	disp := ui.New(len(jobs), cfg.OutputDir)
	disp.Banner(cfg.Mode, cfg.Providers)
	disp.Start()

	if cfg.Verbose {
		disp.Info("host-first strategy: %d unique host(s) from %d URL(s)", countHosts(jobs), len(jobs))
		disp.Info("rate limiter: %.1f req/s burst %d | cc-rps: %.1f burst %d",
			cfg.RPS, cfg.BurstSize, cfg.CCRPS, cfg.CCBurst)
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = p.Name()
		}
		disp.Info("providers: %s", strings.Join(names, " → "))
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

	// Process providers in priority order.
	// Each provider receives only the URLs not yet resolved by previous providers.
	pending := make([]*urlJob, len(jobs))
	copy(pending, jobs)

	for _, p := range providers {
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			goto interrupted
		default:
		}

		if cfg.Verbose {
			disp.Info("[%s] processing %d unresolved URL(s)", p.Name(), len(pending))
		}
		pending = resolveAndDownload(ctx, p, pending, cfg, dl, disp)
	}

interrupted:
	// Anything still pending was not found in any archive.
	for _, j := range pending {
		disp.Skip(j.input, "not found in any archive")
	}

	disp.Stop()
	disp.Summary()
}

// buildJobs normalises each raw URL, computes its URLKey and host, and
// deduplicates by URLKey (scheme-agnostic).  Invalid URLs are silently dropped
// after logging to stderr.
func buildJobs(rawURLs []string) []*urlJob {
	seen := make(map[string]bool, len(rawURLs))
	jobs := make([]*urlJob, 0, len(rawURLs))

	for _, raw := range rawURLs {
		normalized, err := normalize.URL(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: skipping %q: %v\n", raw, err)
			continue
		}
		key := normalize.URLKey(normalized)
		if seen[key] {
			continue // deduplicate scheme-agnostic duplicates
		}
		seen[key] = true

		host, err := normalize.Host(normalized)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: skipping %q: cannot extract host: %v\n", raw, err)
			continue
		}

		jobs = append(jobs, &urlJob{
			input:      raw,
			normalized: normalized,
			key:        key,
			host:       host,
		})
	}
	return jobs
}

// countHosts returns the number of distinct hosts across all jobs.
func countHosts(jobs []*urlJob) int {
	seen := make(map[string]bool)
	for _, j := range jobs {
		seen[j.host] = true
	}
	return len(seen)
}

// resolveAndDownload dispatches jobs to the provider:
//   - If the provider implements HostInventoryFetcher: one CDX query per unique
//     host retrieves the full archive inventory; results are matched against
//     the user URL list and matching snapshots are downloaded.
//   - Otherwise: each pending URL is queried individually (per-URL fallback).
//
// Returns the subset of jobs that were NOT resolved by this provider.
func resolveAndDownload(
	ctx context.Context,
	p provider.Provider,
	pending []*urlJob,
	cfg *config.Config,
	dl *downloader.Downloader,
	disp *ui.Display,
) []*urlJob {
	if hf, ok := p.(provider.HostInventoryFetcher); ok {
		return resolveByHost(ctx, hf, p, pending, cfg, dl, disp)
	}
	return resolvePerURL(ctx, p, pending, cfg, dl, disp)
}

// resolveByHost groups pending jobs by host, fetches the archive inventory for
// each host in parallel, matches against the user URL list, and downloads
// matching snapshots.  Returns jobs not found in the inventory.
func resolveByHost(
	ctx context.Context,
	hf provider.HostInventoryFetcher,
	p provider.Provider,
	pending []*urlJob,
	cfg *config.Config,
	dl *downloader.Downloader,
	disp *ui.Display,
) []*urlJob {
	// Group jobs by host.
	byHost := make(map[string][]*urlJob, len(pending))
	for _, j := range pending {
		byHost[j.host] = append(byHost[j.host], j)
	}

	type hostResult struct {
		resolvedKeys map[string]bool
	}

	results := make(chan hostResult, len(byHost))
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	fetcher, hasFetcher := p.(contentFetcher)

	for host, jobs := range byHost {
		host, jobs := host, jobs
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			inventory, err := hf.FetchHostInventory(ctx, host)
			if err != nil {
				if cfg.Verbose && ctx.Err() == nil {
					disp.Info("%s: inventory error for %s: %v", p.Name(), host, err)
				}
				results <- hostResult{resolvedKeys: nil}
				return
			}

			// Build a URLKey → []Snapshot index from the inventory.
			index := make(map[string][]provider.Snapshot, len(inventory))
			for _, snap := range inventory {
				k := normalize.URLKey(snap.OriginalURL)
				index[k] = append(index[k], snap)
			}

			if cfg.Verbose {
				disp.Info("%s: %s → %d unique URL(s) in archive", p.Name(), host, len(index))
			}

			resolvedKeys := make(map[string]bool)
			var dlWg sync.WaitGroup

			for _, job := range jobs {
				snaps, found := index[job.key]
				if !found {
					continue
				}
				resolvedKeys[job.key] = true

				selected := selector.Select(snaps, cfg.Mode, cfg.MaxSnapshots)
				if cfg.Verbose {
					disp.Info("%s: %s → %d snapshot(s) selected", p.Name(), job.normalized, len(selected))
				}

				job, selected := job, selected
				dlWg.Add(1)
				go func() {
					defer dlWg.Done()
					nNew, nCached, nFailed := downloadAll(ctx, selected, cfg, dl, fetcher, hasFetcher, job.normalized, disp)
					disp.Ok(job.input, nNew, nCached, nFailed)
				}()
			}
			dlWg.Wait()

			results <- hostResult{resolvedKeys: resolvedKeys}
		}()
	}

	wg.Wait()
	close(results)

	// Collect all resolved URL keys.
	allResolved := make(map[string]bool)
	for r := range results {
		for k := range r.resolvedKeys {
			allResolved[k] = true
		}
	}

	// Return jobs not resolved by this provider.
	var stillPending []*urlJob
	for _, j := range pending {
		if !allResolved[j.key] {
			stillPending = append(stillPending, j)
		}
	}
	return stillPending
}

// resolvePerURL is the per-URL fallback for providers that do not implement
// HostInventoryFetcher (e.g. archive.ph).  Each pending job is queried
// individually in parallel.
func resolvePerURL(
	ctx context.Context,
	p provider.Provider,
	pending []*urlJob,
	cfg *config.Config,
	dl *downloader.Downloader,
	disp *ui.Display,
) []*urlJob {
	sem := make(chan struct{}, cfg.Concurrency)
	var mu sync.Mutex
	var stillPending []*urlJob
	var wg sync.WaitGroup

	fetcher, hasFetcher := p.(contentFetcher)

	for _, job := range pending {
		job := job
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if ctx.Err() != nil {
				mu.Lock()
				stillPending = append(stillPending, job)
				mu.Unlock()
				return
			}

			snaps, err := p.FetchSnapshots(ctx, job.normalized)
			if err != nil {
				if cfg.Verbose {
					disp.Info("%s: %s: %v", p.Name(), job.normalized, err)
				}
				mu.Lock()
				stillPending = append(stillPending, job)
				mu.Unlock()
				return
			}
			if len(snaps) == 0 {
				mu.Lock()
				stillPending = append(stillPending, job)
				mu.Unlock()
				return
			}

			selected := selector.Select(snaps, cfg.Mode, cfg.MaxSnapshots)
			nNew, nCached, nFailed := downloadAll(ctx, selected, cfg, dl, fetcher, hasFetcher, job.normalized, disp)
			disp.Ok(job.input, nNew, nCached, nFailed)
		}()
	}

	wg.Wait()
	return stillPending
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

	seenPaths := make(map[string]bool)
	var jobs []job
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
		// Skip duplicate destination paths within the same batch (e.g. CDX
		// returning both http://www.X.com/ and https://X.com/ at the same
		// timestamp — after normalization both map to the same output file;
		// dispatching both races two workers to the same file and double-counts).
		if seenPaths[destPath] {
			continue
		}
		seenPaths[destPath] = true
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

// buildProviders constructs the ordered provider slice.  All providers share
// the tuned transport tr; each gets an appropriate timeout and rate limiter.
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

// collectURLs reads URLs from -u, -l, and/or stdin, deduplicates, strips comments.
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

	for _, u := range cfg.URLs {
		add(u)
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

	// Read from stdin when it is piped/redirected.
	if cfg.Stdin {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			add(sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	}

	return urls, nil
}
