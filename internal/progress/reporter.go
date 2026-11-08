// Package progress writes fixed-field, speech-free diagnostics to stderr.
package progress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ErrEvent rejects unknown activities and inconsistent counters before output.
var ErrEvent = errors.New("invalid progress event")

// Options controls presentation only, never inference or recovery compatibility.
type Options struct{ Quiet, Verbose bool }

// Reporter writes progress to stderr. On a terminal it keeps a log of finished
// steps and redraws one live line for the current step, ticking every 100 ms so
// long waits visibly progress; elsewhere it prints plain lines. A failed stderr
// write silences later output instead of failing paid work; Close reports it.
type Reporter struct {
	mu                sync.Mutex
	writer            io.Writer
	opts              Options
	tty, open, closed bool
	color             bool
	err               error
	// now is replaceable in tests.
	now func() time.Time
	// Download bar state.
	drawn, downloadStart time.Time
	// Pipeline step state; step is -1 when no step is active.
	step                 int
	stepStart            time.Time
	result, activity     string
	chunks, cached, done int
	active               int
	// The ticker redraws the live line; autoTick is false in tests.
	autoTick           bool
	stopTick, tickDone chan struct{}
}

// New writes to stderr (os.Stderr if nil) and detects interactive terminals.
func New(stderr io.Writer, opts Options) *Reporter {
	if stderr == nil {
		stderr = os.Stderr
	}
	tty := Interactive(stderr)
	return &Reporter{writer: stderr, opts: opts, tty: tty, color: tty && os.Getenv("NO_COLOR") == "", step: -1, autoTick: true}
}

// Interactive reports whether w is a terminal that can redraw lines.
func Interactive(w io.Writer) bool { return terminal(w) && os.Getenv("TERM") != "dumb" }

// Color reports whether w is an interactive terminal and NO_COLOR is unset.
func Color(w io.Writer) bool { return Interactive(w) && os.Getenv("NO_COLOR") == "" }

// WarnSpeakerLimit reports a fixed model limitation, even in quiet mode.
func (r *Reporter) WarnSpeakerLimit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.line("Warning: speaker model limit reached; additional participants may be merged.")
}

// line prints a persistent line. An open live step line is cleared first and
// redrawn below; an open setup bar is ended with a newline.
func (r *Reporter) line(message string) error {
	if r.closed {
		return io.ErrClosedPipe
	}
	if r.err != nil {
		return nil
	}
	live := r.tty && r.step >= 0
	if r.open {
		if live {
			r.write("\r\x1b[2K")
		} else {
			r.write("\n")
		}
		r.open = false
	}
	r.write(message + "\n")
	if live {
		r.drawLive()
	}
	return nil
}

func (r *Reporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Reporter) write(message string) {
	n, err := io.WriteString(r.writer, message)
	if err == nil && n != len(message) {
		err = io.ErrShortWrite
	}
	r.err = err
}

// Close stops the live line. A step still active was interrupted, so it is
// marked as stopped rather than finished. Close is idempotent.
func (r *Reporter) Close() error {
	r.mu.Lock()
	stop, done := r.stopTick, r.tickDone
	r.stopTick = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.stopStep()
		if r.open && r.err == nil {
			r.write("\n")
			r.open = false
		}
	}
	r.closed = true
	return r.err
}

// Summary reports counts for a published transcript, never its speech.
func (r *Reporter) Summary(turns, speakers, unknown int, elapsed time.Duration) error {
	if turns < 0 || speakers < 0 || unknown < 0 || unknown > turns || elapsed < 0 {
		return ErrEvent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return io.ErrClosedPipe
	}
	r.finishStep()
	if r.err != nil || r.opts.Quiet {
		return nil
	}
	message := fmt.Sprintf("Done in %s: %s, %s", took(elapsed), count(turns, "turn"), count(speakers, "speaker"))
	if unknown > 0 {
		message += fmt.Sprintf(", %s with an unknown speaker", count(unknown, "turn"))
	}
	if r.color {
		message = "\x1b[32m" + message + "\x1b[0m"
	}
	return r.line(message + ".")
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
