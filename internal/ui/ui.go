// Package ui handles all terminal output with a sticky bottom panel:
//
//   - TTY mode:   a stats block + progress bar are anchored at the bottom of
//     the terminal. Result lines scroll above them. The panel refreshes every
//     120 ms so it stays live during silent fetches.
//   - Plain mode: result lines go to stdout; a status line goes to stderr
//     every 5 s as a fallback for piped/redirected output.
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
	eraseLine = "\r\033[2K" // carriage-return + erase entire line
	cursorUp1 = "\033[1A"   // move cursor up one line
)

var spinFrames = []string{"|", "/", "-", "\\"}

// panelHeight is the FIXED number of terminal rows used by the stats panel
// (9 rows) plus the progress bar (1 row). It must never change at runtime —
// the cursor-movement math in erasePanel depends on it being constant.
//
//	row 1  ──────────────────────── (separator)
//	row 2    URLs processed : N
//	row 3    Succeeded      : N
//	row 4    Skipped        : N
//	row 5    Files new      : N
//	row 6    Files cached   : N
//	row 7    Download errors: N
//	row 8    Output dir     : path
//	row 9  ──────────────────────── (separator)
//	row 10 | ████░░░░  X/Y          (progress bar — no trailing \n)
const panelHeight = 10

// Display manages all terminal output.
type Display struct {
	total     int
	outputDir string
	color     bool
	mu        sync.Mutex
	stopped   bool
	panelUp   bool // true once the panel has been drawn for the first time
	tickWg    sync.WaitGroup
	stopTick  chan struct{}
	spinIdx   int // protected by mu

	// live counters (protected by mu)
	doneURLs    int
	okURLs      int
	failURLs    int
	skipURLs    int
	newFiles    int
	cachedFiles int
	dlFailed    int
}

// New creates a Display for total URLs writing output to outputDir.
func New(total int, outputDir string) *Display {
	return &Display{
		total:     total,
		outputDir: outputDir,
		color:     isTTY(),
	}
}

func (d *Display) c(code, s string) string {
	if !d.color {
		return s
	}
	return code + s + reset
}

// erasePanel moves the cursor from the end of the progress bar (current
// position) to column 1 of the top separator line, erasing every panel row
// on the way up. Must be called with mu held.
func (d *Display) erasePanel() {
	for i := 0; i < panelHeight; i++ {
		fmt.Print(eraseLine)
		if i < panelHeight-1 {
			fmt.Print(cursorUp1)
		}
	}
	// cursor is now at column 1 of the top separator line
}

// drawPanel prints all panelHeight rows starting from the current cursor
// position. The last row (progress bar) has no trailing \n so the cursor
// remains on it, ready for the next erasePanel call. Must be called with mu held.
func (d *Display) drawPanel() {
	sep := strings.Repeat("─", 54)
	d.spinIdx++
	spin := d.c(cyan, spinFrames[d.spinIdx%len(spinFrames)])
	bar := makeBar(d.doneURLs, d.total, 20)

	dlStr := fmt.Sprintf("%d", d.dlFailed)
	if d.dlFailed > 0 {
		dlStr = d.c(yellow, dlStr)
	}

	fmt.Printf("%s%s\n", eraseLine, d.c(bold, sep))
	fmt.Printf("%s  URLs processed  : %d\n", eraseLine, d.total)
	fmt.Printf("%s  Succeeded       : %s\n", eraseLine, d.c(boldGreen, fmt.Sprintf("%d", d.okURLs)))
	fmt.Printf("%s  Skipped         : %s\n", eraseLine, d.c(gray, fmt.Sprintf("%d (no archive found)", d.skipURLs)))
	fmt.Printf("%s  Files new       : %s\n", eraseLine, d.c(green, fmt.Sprintf("%d", d.newFiles)))
	fmt.Printf("%s  Files cached    : %s\n", eraseLine, d.c(cyan, fmt.Sprintf("%d", d.cachedFiles)))
	fmt.Printf("%s  Download errors : %s\n", eraseLine, dlStr)
	fmt.Printf("%s  Output dir      : %s\n", eraseLine, d.outputDir)
	fmt.Printf("%s%s\n", eraseLine, d.c(bold, sep))
	// Progress bar — no trailing newline; cursor stays here for erasePanel.
	fmt.Printf("%s%s %s  %s%d/%d%s",
		eraseLine, spin, bar,
		bold, d.doneURLs, d.total, reset,
	)
}

func makeBar(done, total, width int) string {
	if total <= 0 {
		return "\033[90m" + strings.Repeat("░", width) + reset
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	return "\033[32m" + strings.Repeat("█", filled) +
		"\033[90m" + strings.Repeat("░", width-filled) + reset
}

// printContent prints one or more lines of content above the panel (TTY) or
// directly to stdout (plain). Must be called with mu held.
func (d *Display) printContent(content string) {
	if d.color {
		if d.panelUp {
			d.erasePanel()
		}
		fmt.Print(content)
		d.drawPanel()
		d.panelUp = true
	} else {
		fmt.Print(content)
	}
}

// stderrStatus writes a plain-text progress line to stderr (plain-mode fallback).
func (d *Display) stderrStatus() {
	fmt.Fprintf(os.Stderr,
		"[status] %d/%d URLs  ↓ %d new  ✓ %d cached  ✗ %d failed  ◌ %d skipped\n",
		d.doneURLs, d.total,
		d.newFiles, d.cachedFiles,
		d.dlFailed+d.failURLs, d.skipURLs,
	)
}

// ── public API ────────────────────────────────────────────────────────────────

// Banner prints the one-time startup header above the panel.
func (d *Display) Banner(mode, providers string) {
	fmt.Printf("\n%s  %d URL(s)  ·  mode: %s  ·  providers: %s\n",
		d.c(bold, "waybackdown"), d.total, mode, providers)
	fmt.Println(d.c(gray, strings.Repeat("─", 72)))
	fmt.Println()
}

// Start draws the initial panel and launches the background refresh goroutine.
func (d *Display) Start() {
	d.stopTick = make(chan struct{})

	if d.color {
		d.mu.Lock()
		d.drawPanel()
		d.panelUp = true
		d.mu.Unlock()

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
					d.erasePanel()
					d.drawPanel()
					d.mu.Unlock()
				}
			}
		}()
	} else {
		// Plain mode: emit a status line to stderr every 5 s.
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
}

// Stop halts the background ticker and blocks until it exits so that Summary
// is never overwritten by a stray redraw.
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
	var line string
	switch {
	case newF == 0 && cached == 0 && dlFailed > 0:
		line = fmt.Sprintf("%s  %s  %s\n", d.c(yellow, "[!]"), url,
			d.c(yellow, fmt.Sprintf("%d found, all downloads failed", dlFailed)))
	case newF == 0 && cached > 0:
		line = fmt.Sprintf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d cached", cached)))
	case cached > 0:
		line = fmt.Sprintf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d new, %d cached", newF, cached)))
	default:
		line = fmt.Sprintf("%s  %s  %s\n", d.c(boldGreen, "[✓]"), url,
			d.c(gray, fmt.Sprintf("%d snapshot(s)", newF)))
	}
	d.printContent(line)
}

// Fail records and prints a URL that could not be processed.
func (d *Display) Fail(url string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doneURLs++
	d.failURLs++
	d.printContent(fmt.Sprintf("%s  %s\n    %s\n",
		d.c(boldRed, "[✗]"), url, d.c(red, err.Error())))
}

// Skip records and prints a URL for which no archive was found.
func (d *Display) Skip(url, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.doneURLs++
	d.skipURLs++
	d.printContent(fmt.Sprintf("%s  %s  %s\n",
		d.c(gray, "[~]"), url, d.c(gray, reason)))
}

// Info prints a verbose informational line (goroutine-safe).
func (d *Display) Info(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.printContent(fmt.Sprintf(d.c(cyan, "[·]")+"  "+format+"\n", args...))
}

// Down prints a verbose download line (goroutine-safe).
func (d *Display) Down(archiveURL, destPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.printContent(fmt.Sprintf("%s  %s\n    %s %s\n",
		d.c(cyan, "[↓]"), archiveURL, d.c(gray, "→"), destPath))
}

// DlWarn prints a per-snapshot download failure (always visible, goroutine-safe).
func (d *Display) DlWarn(normalized, ts string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.printContent(fmt.Sprintf("%s  %s (%s): %s\n",
		d.c(yellow, "[!]"), normalized, ts, err.Error()))
}

// Interrupted prints a notice that the run was cut short by a signal.
// Call after Stop() and before Summary().
func (d *Display) Interrupted() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.printContent(fmt.Sprintf("\n%s\n\n",
		d.c(boldRed, "  interrupted — partial results saved")))
}

// Summary erases the live panel and prints the final stats block.
// Call Stop() before Summary().
func (d *Display) Summary() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.color && d.panelUp {
		d.erasePanel()
	}
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
	fmt.Printf("  Output dir      : %s\n", d.outputDir)
	fmt.Printf("%s\n", d.c(bold, sep))
}
