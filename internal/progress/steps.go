package progress

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teapot2/turnbeam/internal/meta"
)

// Stage is a fixed pipeline activity; arbitrary messages and paths are excluded.
type Stage uint8

const (
	Probe Stage = iota
	Normalize
	Diarize
	Chunk
	Transcribe
	Assemble
	Align
	Write
)

var stages = [...]string{
	Probe:      "Checking the recording",
	Normalize:  "Preparing audio",
	Diarize:    "Finding speakers (on device)",
	Chunk:      "Splitting at pauses",
	Transcribe: "Transcribing with Meta Muse",
	Assemble:   "Assembling transcript",
	Align:      "Matching turns to speakers",
	Write:      "Writing transcript",
}

const (
	labelWidth  = 30
	chunkBar    = 16
	tickPeriod  = 100 * time.Millisecond
	spinnerRune = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
)

// Stage finishes the current step and starts the next one.
func (r *Reporter) Stage(ctx context.Context, stage Stage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if int(stage) >= len(stages) {
		return ErrEvent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return io.ErrClosedPipe
	}
	r.finishStep()
	r.step, r.stepStart, r.result, r.activity = int(stage), r.clock(), "", ""
	if r.err != nil || r.opts.Quiet {
		return nil
	}
	if !r.tty {
		return r.line(r.heading(stages[stage]))
	}
	r.drawLive()
	r.startTicker()
	return nil
}

// AudioPrepared records the length of the prepared recording.
func (r *Reporter) AudioPrepared(length time.Duration) {
	r.setResult(Normalize, length >= 0, span(length)+" of audio")
}

// SpeakersFound records how many speakers the local model distinguished.
func (r *Reporter) SpeakersFound(speakers int) {
	text := count(speakers, "speaker")
	if speakers == 0 {
		text = "no speech found"
	}
	r.setResult(Diarize, speakers >= 0, text)
}

// ChunksPlanned records how many chunks the recording was split into.
func (r *Reporter) ChunksPlanned(chunks int) {
	r.setResult(Chunk, chunks >= 0, count(chunks, "chunk"))
}

// ReusingResults notes that every chunk was already transcribed.
func (r *Reporter) ReusingResults() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.step >= 0 {
		r.result = "already transcribed; reusing saved results"
	}
}

// TranscribeStarting sets the overall chunk count, including chunks a resumed
// run already transcribed.
func (r *Reporter) TranscribeStarting(chunks, done int) error {
	if chunks < 0 || done < 0 || done > chunks {
		return ErrEvent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks, r.cached, r.done, r.active = chunks, done, done, 0
	r.activity = "preparing audio"
	if done > 0 && !r.opts.Quiet {
		return r.line(fmt.Sprintf("%sresuming: %d of %s already transcribed", indent, done, count(chunks, "chunk")))
	}
	return nil
}

func (r *Reporter) setResult(stage Stage, valid bool, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if valid && r.step == int(stage) {
		r.result = text
	}
}

// Transcription plugs directly into Pool.OnProgress. All fields are validated;
// no arbitrary error string, API response, file path, key or chunk ID is accepted.
func (r *Reporter) Transcription(ctx context.Context, event meta.Progress) error {
	if event.Kind > meta.ProgressSending || event.Total < 0 || event.Completed < 0 || event.Completed > event.Total || event.Active < 0 || event.Active > event.Total-event.Completed || event.Chunk < 0 || event.Chunk > event.Total || event.Attempt < 0 || event.Attempt > meta.MaxAttempts || event.HTTPStatus != 0 && (event.HTTPStatus < 100 || event.HTTPStatus > 599) || event.RetryDelay < 0 {
		return ErrEvent
	}
	final := event.Kind == meta.ProgressFinished || event.Kind == meta.ProgressStopped
	if !final {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if final && event.Active != 0 || event.Kind == meta.ProgressFinished && event.Completed != event.Total {
		return ErrEvent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return io.ErrClosedPipe
	}
	// Chunk numbers are positions in this run's queue; offset them by the
	// chunks a resumed run already had, so they match the whole recording.
	chunk := r.cached + event.Chunk
	r.done, r.active = r.cached+event.Completed, event.Active
	r.chunks = r.cached + event.Total
	switch event.Kind {
	case meta.ProgressSending:
		r.activity = fmt.Sprintf("chunk %d sent, waiting for Meta", chunk)
		if event.Attempt > 1 {
			r.activity += fmt.Sprintf(" (attempt %d of %d)", event.Attempt, meta.MaxAttempts)
		}
	case meta.ProgressRetry:
		cause := "connection failed"
		if event.HTTPStatus != 0 {
			cause = fmt.Sprintf("HTTP %d", event.HTTPStatus)
		}
		r.activity = fmt.Sprintf("chunk %d retrying in %s", chunk, took(event.RetryDelay))
		if !r.opts.Quiet {
			return r.line(fmt.Sprintf("%schunk %d: %s, retrying in %s (attempt %d of %d)", indent, chunk, cause, took(event.RetryDelay), event.Attempt, meta.MaxAttempts))
		}
	case meta.ProgressCompleted:
		r.activity = fmt.Sprintf("chunk %d done", chunk)
		if !r.tty && !r.opts.Quiet {
			return r.line(fmt.Sprintf("%schunk %d done (%d/%d)", indent, chunk, r.done, r.chunks))
		}
	}
	if !r.opts.Quiet {
		r.drawLive()
	}
	return nil
}

// finishStep replaces the live line with a finished log entry.
func (r *Reporter) finishStep() {
	if r.step < 0 {
		return
	}
	if r.step == int(Transcribe) && r.result == "" && r.chunks > 0 {
		r.result = count(r.chunks, "chunk")
		if r.cached > 0 {
			r.result += fmt.Sprintf(", %d reused", r.cached)
		}
	}
	r.endStep(r.heading(r.label()), r.result)
}

// stopStep marks an interrupted step.
func (r *Reporter) stopStep() {
	if r.step < 0 {
		return
	}
	mark := "  ✗ "
	if r.color {
		mark = "  \x1b[31m✗\x1b[0m "
	}
	if !r.tty {
		mark = "==> "
	}
	r.endStep(mark+r.label(), "stopped")
}

func (r *Reporter) endStep(title, detail string) {
	elapsed := r.clock().Sub(r.stepStart)
	r.step = -1
	if r.err != nil || r.opts.Quiet {
		return
	}
	if elapsed >= time.Second {
		detail = strings.TrimPrefix(detail+" · "+took(elapsed), " · ")
	}
	if !r.tty {
		if detail != "" {
			r.line(indent + detail)
		}
		return
	}
	if detail != "" {
		title = r.pad(title) + r.dim(detail)
	}
	r.write("\r\x1b[2K" + title + "\n")
	r.open = false
}

// drawLive redraws the current step's line: spinner, label, activity, clock.
func (r *Reporter) drawLive() {
	if !r.tty || r.step < 0 || r.err != nil || r.opts.Quiet {
		return
	}
	elapsed := r.clock().Sub(r.stepStart)
	frames := []rune(spinnerRune)
	frame := string(frames[int(elapsed/tickPeriod)%len(frames)])
	if r.color {
		frame = "\x1b[36m" + frame + "\x1b[0m"
	}
	detail := ""
	if r.step == int(Transcribe) && r.chunks > 0 {
		detail = fmt.Sprintf("%s %d/%d chunks", r.chunkBar(), r.done, r.chunks)
		if r.activity != "" {
			detail += " · " + r.activity
		}
		if r.active > 1 {
			detail += fmt.Sprintf(" · %d in progress", r.active)
		}
		detail += " · "
	}
	r.write("\r\x1b[2K" + r.pad("  "+frame+" "+r.label()) + detail + stopwatch(elapsed))
	r.open = true
}

func (r *Reporter) startTicker() {
	if !r.autoTick || r.stopTick != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.stopTick, r.tickDone = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(tickPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.open && r.step >= 0 {
					r.drawLive()
				}
				r.mu.Unlock()
			}
		}
	}()
}

func (r *Reporter) label() string { return stages[r.step] }

// pad aligns step details; ANSI codes are excluded from the width.
func (r *Reporter) pad(title string) string {
	visible := len([]rune(stripANSI(title)))
	return title + strings.Repeat(" ", max(2, labelWidth+4-visible))
}

func (r *Reporter) dim(text string) string {
	if r.color && text != "" {
		return "\x1b[2m" + text + "\x1b[0m"
	}
	return text
}

func (r *Reporter) chunkBar() string {
	filled := chunkBar
	if r.chunks > 0 {
		filled = r.done * chunkBar / r.chunks
	}
	full, empty := strings.Repeat("█", filled), strings.Repeat("░", chunkBar-filled)
	if r.color {
		return "\x1b[36m" + full + "\x1b[0m\x1b[2m" + empty + "\x1b[0m"
	}
	return full + empty
}

func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// took formats a finished duration: 45s, 4m12s, 1h05m03s.
func took(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", s/3600, s/60%60, s%60)
}

// span formats a recording length the same way as took.
func span(d time.Duration) string { return took(d) }

// stopwatch formats a running duration: 0:07, 12:34, 1:02:03.
func stopwatch(d time.Duration) string {
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
