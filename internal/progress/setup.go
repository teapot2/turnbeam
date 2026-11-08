package progress

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/setup"
)

const (
	barWidth     = 24
	redrawPeriod = 100 * time.Millisecond
	indent       = "    "
)

var setupSteps = [...]string{
	setup.StepCheck:         "Checking existing installation",
	setup.StepVerify:        "Verifying installation",
	setup.StepCompatibility: "Checking Metal compatibility",
}

// Setup plugs directly into setup.Manager.OnProgress. Each download prints a
// heading and its pinned source URL; on a terminal a progress bar follows and
// redraws at most every 100 ms, otherwise one line reports completion. Color is
// used only on terminals and is disabled by NO_COLOR. Quiet mode prints nothing.
func (r *Reporter) Setup(event setup.Progress) error {
	download := event.Step == setup.StepDownload
	if event.Step > setup.StepCompatibility || event.Total < 0 || event.Done < 0 || event.Done > event.Total || event.Attempt < 0 || event.Attempt > setup.DownloadAttempts ||
		download && (!printable(event.Title, 120) || !pinnedURL(event.Source)) ||
		!download && (event.Title != "" || event.Source != "") {
		return ErrEvent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return io.ErrClosedPipe
	}
	if r.err != nil || r.opts.Quiet {
		return nil
	}
	if !download {
		return r.line(r.heading(setupSteps[event.Step]))
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	complete := event.Done == event.Total
	first := event.Title != r.downloadTitle
	note := ""
	switch {
	case first:
		r.downloadTitle, r.downloadAttempt = event.Title, event.Attempt
		r.line(r.heading(event.Title))
		r.line(indent + event.Source)
		if complete && event.Attempt == 0 {
			note = " (already downloaded)"
		} else if event.Done > 0 {
			r.line(fmt.Sprintf("%sresuming at %s of %s", indent, size(event.Done), size(event.Total)))
		}
	case event.Attempt > r.downloadAttempt:
		r.downloadAttempt = event.Attempt
		message := fmt.Sprintf("%sconnection problem; retrying (attempt %d of %d)", indent, event.Attempt, setup.DownloadAttempts)
		if event.Done > 0 {
			message += ", resuming at " + size(event.Done)
		}
		r.line(message)
		first = true
	}
	if first {
		r.downloadStart, r.downloadBase = now, event.Done
	}
	if !r.tty {
		if complete && note != "" {
			return r.line(indent + "Already downloaded")
		}
		if complete {
			return r.line(indent + "Downloaded " + size(event.Total))
		}
		return nil
	}
	if !first && !complete && now.Sub(r.drawn) < redrawPeriod {
		return nil
	}
	r.drawn = now
	percent := 100
	if event.Total > 0 {
		percent = int(event.Done * 100 / event.Total)
	}
	message := fmt.Sprintf("%s%s %3d%%  ", indent, r.bar(event.Done, event.Total, complete), percent)
	if complete {
		message += size(event.Total) + note
	} else {
		message += size(event.Done) + " / " + size(event.Total)
		if elapsed := now.Sub(r.downloadStart); elapsed >= time.Second {
			message += fmt.Sprintf("  %.1f MB/s", float64(event.Done-r.downloadBase)/1e6/elapsed.Seconds())
		}
	}
	r.write("\r\x1b[2K" + message)
	r.open = true
	if complete && r.err == nil {
		r.write("\n")
		r.open = false
	}
	return nil
}

func (r *Reporter) heading(text string) string {
	if r.color {
		return "\x1b[1;34m==>\x1b[0m \x1b[1m" + text + "\x1b[0m"
	}
	return "==> " + text
}

func (r *Reporter) bar(done, total int64, complete bool) string {
	filled := barWidth
	if total > 0 {
		filled = int(float64(done) / float64(total) * barWidth)
	}
	full, empty := strings.Repeat("█", filled), strings.Repeat("░", barWidth-filled)
	if !r.color {
		return full + empty
	}
	tint := "\x1b[36m"
	if complete {
		tint = "\x1b[32m"
	}
	return tint + full + "\x1b[0m\x1b[2m" + empty + "\x1b[0m"
}

// size formats decimal units: KB below one megabyte, MB otherwise.
func size(bytes int64) string {
	if bytes < 1_000_000 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1e3)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/1e6)
}

func printable(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// pinnedURL accepts plain HTTPS URLs without credentials or query strings, so a
// signed redirect target can never be displayed.
func pinnedURL(s string) bool {
	return printable(s, 512) && strings.HasPrefix(s, "https://") && !strings.ContainsAny(s, " @?#")
}
