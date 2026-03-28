// Package ui handles all terminal output: colored result lines and a live
// progress bar. All methods are safe for concurrent use.
//
// TTY mode  (stdout is a terminal): animated ANSI progress bar redrawn in place.
// Plain mode (stdout is piped/redirected): plain-text result lines to stdout +
// periodic status lines to stderr every 5 s so progress is always visible.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	reset     = "\033[0m"
	bold      = "\033[1m"
	red       = "\033[31m"
	green     = "\033[32m"
	yellow    = "\033[33m"
	cyan      = "\033[36m"
	gray      = "\033[90m"
	boldRed   = "\033[1;31m"
	boldGreen = "\033[1;32m"
)

var spinFrames = []string{"|", "/", "-", "\\"}

// Display manages colored output and a live progress bar.
type Display struct {
	total   int
	color   bool       // true  → TTY mode (ANSI bar)
	stopped bool       // set by Stop(); prevents ticker from drawing after summary
	tickWg  sync.WaitGroup
	mu      sync.Mutex

	stopTick chan struct{}
	spinIdx  int // protected by mu

	// live counters (protected by mu)
	doneURLs    int
	okURLs      int
	failURLs    int
	skipURLs    int
	newFiles    int
	cachedFiles int
	dlFailed    int
}

// New creates a Display for total URLs.
func New(total int) *Display {
	return &Display{
		total: total,
		color: isTTY(), // platform-specific: tty_unix.go / tty_windows.go
	}
}

// ── internal helpers ─────────────────────────────────────────────────────────

func (d *Display) c(code, s string) string {
	if !d.color {
		return s
	}
	return code + s + reset
}

// clearLine erases the progress bar line (TTY mode, must be called with mu held).
func (d *Display) clearLine() {
	if d.color {
		fmt.Print("\r\033[K")
	}
}

// redraw repaints the progress bar in place (TTY mode, must be called with mu held).
func (d *Display) redraw() {
	if !d.color {
		return
	}
	d.spinIdx++
	spin := d.c(cyan, spinFrames[d.spinIdx%len(spinFrames)])
	bar := makeBar(d.doneURLs, d.total, 18)
	fmt.Printf("%s %s  %s%d/%d%s  ↓ %s  ✓ %s  ✗ %s  ◌ %s",
		spin, bar,
		bold, d.doneURLs, d.total, reset,
		d.c(green, fmt.Sprintf("%d new", d.newFiles)),
		d.c(cyan, fmt.Sprintf("%d cached", d.cachedFiles)),
		d.c(red, fmt.Sprintf("%d failed", d.dlFailed+d.failURLs)),
		d.c(gray, fmt.Sprintf("%d skipped", d.skipURLs)),
	)
}

// stderrStatus prints a plain-text status line to stderr (plain mode).
// Must be called with mu held.
func (d *Display) stderrStatus() {
	fmt.Fprintf(os.Stderr, "[status] %d/%d URLs  ↓ %d new  ✓ %d cached  ✗ %d failed  ◌ %d skipped\n",
		d.doneURLs, d.total,
		d.newFiles, d.cachedFiles,
		d.dlFailed+d.failURLs, d.skipURLs,
	)
}

func makeBar(done, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	return "\033[32m" + strings.Repeat("█", filled) +
		"\033[90m" + strings.Repeat("░", width-filled) + reset
}

// ── public API ────────────────────────────────────────────────────────────────

// Banner prints the startup header.
func (d *Display) Banner(mode, providers string) {
	fmt.Printf("\n%s  %d URL(s)  ·  mode: %s  ·  providers: %s\n",
		d.c(bold, "waybackdown"), d.total, mode, providers)
	fmt.Println(d.c(gray, strings.Repeat("─", 72)))
	fmt.Println()
}

// Start draws the initial progress bar and launches background goroutines that
// keep stats visible for the entire duration of the run:
//
//   - TTY mode:   ANSI bar on stdout redrawn every 120 ms with a spinning indicator.
//   - Always:     plain status line written to stderr every 5 s so progress is
//     visible even when stdout is piped, redirected, or the terminal does not
//     support ANSI (e.g. tmux with unusual settings, script(1), CI runners).
func (d *Display) Start() {
	d.stopTick = make(chan struct{})

	if d.color {
		// Draw the initial bar immediately.
		d.mu.Lock()
		d.clearLine()
		d.redraw()
		d.mu.Unlock()

		// TTY ticker: redraws the animated bar every 120 ms.
		d.tickWg.Add(1)
		go func() {
			defer d.tickWg.Done()
			t := time.NewTicker(120 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-d.stopTick:
					return
				case <-t.C:
					d.mu.Lock()
					if d.stopped {
						d.mu.Unlock()
						return
					}
					d.clearLine()
					d.redraw()
					d.mu.Unlock()
				}
			}
		}()
	}

	// Stderr ticker: always runs, writes a plain status line every 5 s.
	// This is the safety net that ensures the user always sees activity.
	d.tickWg.Add(1)
	go func() {
		defer d.tickWg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-d.stopTick:
				return
			case <-t.C:
				d.mu.Lock()
				if d.stopped {
					d.mu.Unlock()
					return
				}
				d.stderrStatus()
				d.mu.Unlock()
			}
		}
	}()
}

// Stop halts the background ticker and blocks until it has fully exited,
// guaranteeing that Summary() is never overwritten by a stray redraw.
func (d *Display) Stop() {
	if d.stopTick == nil {
		return
	}
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()

	close(d.stopTick)
	d.tickWg.Wait()
	d.stopTick = nil
}

// Ok records and prints a successfully-processed URL.
func (d *Display) Ok(url string, newF, cached, dlFailed int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doneURLs++
	d.okURLs++
	d.newFiles += newF
	d.cachedFiles += cached
	d.dlFailed += dlFailed
	d.clearLine()
	switch {
	case newF == 0 && cached == 0 && dlFailed > 0:
		fmt.Printf("%s  %s  %s\n", d.c(yellow, "[!]"), url,
			d.c(yellow, fmt.Sprintf("%d found, all downloads failed", dlFailed)))
	case newF == 0 && cached > 0:
		fmt.Printf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d cached", cached)))
	case cached > 0:
		fmt.Printf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d new, %d cached", newF, cached)))
	default:
		fmt.Printf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d snapshot(s)", newF)))
	}
	d.redraw()
}

// Fail records and prints a URL that could not be processed.
func (d *Display) Fail(url string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doneURLs++
	d.failURLs++
	d.clearLine()
	fmt.Printf("%s  %s\n    %s\n", d.c(boldRed, "[✗]"), url, d.c(red, err.Error()))
	d.redraw()
}

// Skip records and prints a skipped URL (no archive found or invalid URL).
// Always printed — this is a result, not a verbose diagnostic.
func (d *Display) Skip(url, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doneURLs++
	d.skipURLs++
	d.clearLine()
	fmt.Printf("%s  %s  %s\n", d.c(gray, "[~]"), url, d.c(gray, reason))
	d.redraw()
}

// Info prints a verbose informational line (goroutine-safe).
func (d *Display) Info(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearLine()
	fmt.Printf(d.c(cyan, "[·]")+"  "+format+"\n", args...)
	d.redraw()
}

// Down prints a verbose download-in-progress line (goroutine-safe).
func (d *Display) Down(archiveURL, destPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearLine()
	fmt.Printf("%s  %s\n    %s %s\n", d.c(cyan, "[↓]"), archiveURL, d.c(gray, "→"), destPath)
	d.redraw()
}

// DlWarn prints a per-snapshot download failure (always visible, goroutine-safe).
func (d *Display) DlWarn(normalized, ts string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearLine()
	fmt.Printf("%s  %s (%s): %s\n", d.c(yellow, "[!]"), normalized, ts, err.Error())
	d.redraw()
}

// Summary clears the progress bar and prints the final stats block.
func (d *Display) Summary(outputDir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearLine()
	sep := strings.Repeat("─", 54)
	fmt.Printf("\n%s\n", d.c(bold, sep))
	fmt.Printf("  URLs processed  : %d\n", d.total)
	fmt.Printf("  Succeeded       : %s\n", d.c(boldGreen, fmt.Sprintf("%d", d.okURLs)))
	if d.skipURLs > 0 {
		fmt.Printf("  Skipped         : %s\n", d.c(gray, fmt.Sprintf("%d (no archive found)", d.skipURLs)))
	}
	if d.failURLs > 0 {
		fmt.Printf("  Failed          : %s\n", d.c(boldRed, fmt.Sprintf("%d", d.failURLs)))
	}
	fmt.Printf("  Files new       : %s\n", d.c(green, fmt.Sprintf("%d", d.newFiles)))
	fmt.Printf("  Files cached    : %s\n", d.c(cyan, fmt.Sprintf("%d", d.cachedFiles)))
	if d.dlFailed > 0 {
		fmt.Printf("  Download errors : %s\n", d.c(yellow, fmt.Sprintf("%d", d.dlFailed)))
	}
	fmt.Printf("  Output dir      : %s\n", outputDir)
	fmt.Printf("%s\n", d.c(bold, sep))
}
