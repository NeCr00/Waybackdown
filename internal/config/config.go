package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	ModeOldest = "oldest"
	ModeNewest = "newest"
	ModeAll    = "all"
)

// multiFlag implements flag.Value for a string flag that can be repeated.
// Each occurrence appends to the slice: -u a -u b → ["a", "b"].
type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ",") }
func (f *multiFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// Config holds all runtime configuration parsed from CLI flags.
type Config struct {
	URLs             []string // from one or more -u flags
	URLFile          string
	Stdin            bool    // true when stdin is a pipe/redirect
	Mode             string
	OutputDir        string
	Concurrency      int
	Timeout          time.Duration
	Retries          int
	Verbose          bool
	MaxSnapshots     int
	StatusFilter     string  // e.g. "200" — empty means no filter
	RPS              float64 // max HTTP requests per second for downloads + non-CC CDX
	BurstSize        int     // token-bucket burst capacity
	Providers        string  // comma-separated provider names in priority order
	CCMaxCollections int     // max Common Crawl collections to query per host
	CCRPS            float64 // Common Crawl CDX requests/second (independent of RPS)
	CCBurst          int     // burst size for the CC CDX rate limiter
	DLWorkers        int     // parallel download workers per URL in 'all' mode
	HostQueryLimit   int     // max CDX records per host inventory query (0 = no limit)

	// LogVerbose is called instead of fmt.Fprintf(os.Stderr,...) for verbose
	// informational messages when Verbose is true.  When nil, output falls back
	// to os.Stderr.  main.go sets this to disp.Info so that TTY-mode verbose
	// logs are serialized through the display lock and don't corrupt the panel.
	LogVerbose func(format string, args ...any)
}

// Parse parses CLI flags and returns a validated Config.
func Parse() *Config {
	cfg := &Config{}

	flag.Var((*multiFlag)(&cfg.URLs), "u", "URL to look up and download (may be repeated: -u url1 -u url2)")
	flag.StringVar(&cfg.URLFile, "l", "", "path to a file containing one URL per line")
	flag.StringVar(&cfg.Mode, "mode", ModeNewest, "snapshot selection mode: oldest | newest | all")
	flag.StringVar(&cfg.OutputDir, "o", "waybackdown_output", "root output directory for downloaded files")
	flag.IntVar(&cfg.Concurrency, "c", 10, "number of hosts to process concurrently per provider")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "HTTP request timeout per request")
	flag.IntVar(&cfg.Retries, "retries", 3, "max retries on transient HTTP/network failures")
	flag.BoolVar(&cfg.Verbose, "v", false, "verbose output (individual snapshot URLs and paths)")
	flag.IntVar(&cfg.MaxSnapshots, "max", 0, "max snapshots per URL in 'all' mode (0 = no limit)")
	flag.StringVar(&cfg.StatusFilter, "status", "",
		"filter CDX snapshots by HTTP status code (empty = all; '200' = successful only)")
	flag.Float64Var(&cfg.RPS, "rps", 5.0,
		"max HTTP requests/second for downloads + non-CC CDX queries (0 = unlimited)")
	flag.IntVar(&cfg.BurstSize, "burst", 20,
		"token-bucket burst size for the rate limiter (only used when -rps > 0)")
	flag.StringVar(&cfg.Providers, "providers", "wayback,archiveph,commoncrawl,arquivo",
		"comma-separated archive providers in priority order (wayback,archiveph,commoncrawl,arquivo)")
	flag.IntVar(&cfg.CCMaxCollections, "cc-max", 3,
		"max Common Crawl index collections to query per host (0 = all)")
	flag.Float64Var(&cfg.CCRPS, "cc-rps", 5.0,
		"Common Crawl CDX requests/second (independent of -rps; 0 = unlimited)")
	flag.IntVar(&cfg.CCBurst, "cc-burst", 20,
		"burst size for the Common Crawl CDX rate limiter")
	flag.IntVar(&cfg.DLWorkers, "dl-workers", 4,
		"parallel download workers per URL in 'all' mode")
	flag.IntVar(&cfg.HostQueryLimit, "host-limit", 100000,
		"max CDX records returned per host inventory query (0 = no server-side limit)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "waybackdown — download archived URL snapshots from multiple web archives\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u <url> [-u <url> ...] [options]\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l <file> [options]\n")
		fmt.Fprintf(os.Stderr, "  cat urls.txt | waybackdown [options]\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u https://target.com -mode all -v\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -u https://target.com/login.php -mode all -status 200\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l urls.txt -mode newest -c 10 -o ./archives\n")
		fmt.Fprintf(os.Stderr, "  waybackdown -l urls.txt -mode all -rps 5 -burst 20\n")
		fmt.Fprintf(os.Stderr, "  cat urls.txt | waybackdown -mode all -providers wayback,commoncrawl\n")
		fmt.Fprintf(os.Stderr, "\nNotes:\n")
		fmt.Fprintf(os.Stderr, "  -status \"\"   downloads ALL archived responses (404, 403, 302 etc.) [default]\n")
		fmt.Fprintf(os.Stderr, "  -status 200  downloads only successful 200 OK captures\n")
		fmt.Fprintf(os.Stderr, "  -mode all    downloads every unique content version\n")
		fmt.Fprintf(os.Stderr, "  Host-first:  providers are queried once per unique host, not once per URL\n")
	}

	flag.Parse()

	// Detect piped stdin.
	if fi, err := os.Stdin.Stat(); err == nil {
		cfg.Stdin = fi.Mode()&os.ModeCharDevice == 0
	}

	switch cfg.Mode {
	case ModeOldest, ModeNewest, ModeAll:
	default:
		fmt.Fprintf(os.Stderr, "error: invalid -mode %q — must be oldest, newest, or all\n", cfg.Mode)
		os.Exit(1)
	}

	if len(cfg.URLs) == 0 && cfg.URLFile == "" && !cfg.Stdin {
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
	if cfg.CCBurst < 1 {
		cfg.CCBurst = 1
	}
	if cfg.DLWorkers < 1 {
		cfg.DLWorkers = 1
	}

	return cfg
}
