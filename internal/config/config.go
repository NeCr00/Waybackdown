package config

import (
	"flag"
	"fmt"
	"os"
	"time"
)

const (
	ModeOldest = "oldest"
	ModeNewest = "newest"
	ModeAll    = "all"
)

// Config holds all runtime configuration parsed from CLI flags.
type Config struct {
	SingleURL        string
	URLFile          string
	Mode             string
	OutputDir        string
	Concurrency      int
	Timeout          time.Duration
	Retries          int
	Verbose          bool
	MaxSnapshots     int
	StatusFilter     string  // e.g. "200" — empty means no filter
	RPS              float64 // max HTTP requests per second (0 = unlimited)
	BurstSize        int     // token-bucket burst capacity
	Providers        string  // comma-separated provider names in fallback order
	CCMaxCollections int     // max Common Crawl collections to query per URL
}

// Parse parses CLI flags and returns a validated Config.
func Parse() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.SingleURL, "u", "", "single URL to look up and download")
	flag.StringVar(&cfg.URLFile, "l", "", "path to a file containing one URL per line")
	flag.StringVar(&cfg.Mode, "mode", ModeNewest, "snapshot selection mode: oldest | newest | all")
	flag.StringVar(&cfg.OutputDir, "o", "waybackdown_output", "root output directory for downloaded files")
	flag.IntVar(&cfg.Concurrency, "c", 5, "number of URLs to process concurrently")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "HTTP request timeout per request")
	flag.IntVar(&cfg.Retries, "retries", 3, "max retries on transient HTTP/network failures")
	flag.BoolVar(&cfg.Verbose, "v", false, "verbose output (individual snapshot URLs and paths)")
	flag.IntVar(&cfg.MaxSnapshots, "max", 0, "max snapshots per URL in 'all' mode (0 = no limit)")
	flag.StringVar(&cfg.StatusFilter, "status", "",
		"filter CDX snapshots by HTTP status code (empty = all statuses; use '200' for successful captures only)")
	flag.Float64Var(&cfg.RPS, "rps", 2.0,
		"max HTTP requests/second across all workers (0 = unlimited; shared rate limiter)")
	flag.IntVar(&cfg.BurstSize, "burst", 10,
		"token-bucket burst size for the rate limiter (only used when -rps > 0)")
	flag.StringVar(&cfg.Providers, "providers", "wayback,archiveph,commoncrawl,arquivo",
		"comma-separated archive providers in fallback order (wayback,archiveph,commoncrawl,arquivo)")
	flag.IntVar(&cfg.CCMaxCollections, "cc-max", 3,
		"max Common Crawl index collections to query per URL (0 = all)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "waybackdown — download archived URL snapshots from multiple web archives\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u <url> [options]\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l <file> [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u https://target.com -mode all -v\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u https://target.com/login.php -mode all -status 200\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l urls.txt -mode newest -c 10 -o ./archives\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l urls.txt -mode all -rps 5 -burst 20\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l urls.txt -mode all -providers wayback,commoncrawl\n")
		fmt.Fprintf(os.Stderr, "\nNotes:\n")
		fmt.Fprintf(os.Stderr, "  -status \"\"   downloads ALL archived responses (404, 403, 302 etc.) [default]\n")
		fmt.Fprintf(os.Stderr, "  -status 200  downloads only successful 200 OK captures\n")
		fmt.Fprintf(os.Stderr, "  -mode all    downloads every unique content version \n")
	}

	flag.Parse()

	switch cfg.Mode {
	case ModeOldest, ModeNewest, ModeAll:
	default:
		fmt.Fprintf(os.Stderr, "error: invalid -mode %q — must be oldest, newest, or all\n", cfg.Mode)
		os.Exit(1)
	}

	if cfg.SingleURL == "" && cfg.URLFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.BurstSize < 1 {
		cfg.BurstSize = 1
	}

	return cfg
}
