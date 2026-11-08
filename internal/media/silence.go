package media

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

// Silence is waveform evidence on the unchanged recording clock, not a claim
// that speech is absent. The chunk planner also requires a diarization gap.
type Silence struct{ Start, End time.Duration }

const MinSilence = 500 * time.Millisecond
const SilenceNoiseDB = -40

// DetectSilence analyzes, but never trims, a complete normalized recording.
// The caller owns the deadline. Metadata is parsed as FFmpeg streams it, so the
// number of pauses in a long recording is not limited by an output buffer.
func (p Processor) DetectSilence(ctx context.Context, recording *Prepared) ([]Silence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !recording.ValidTimeline() {
		return nil, fmt.Errorf("%w: silence detection requires normalized audio", ErrInvalidMedia)
	}
	path, err := localInput(recording.Path)
	if err != nil {
		return nil, err
	}
	filter := fmt.Sprintf("silencedetect=noise=%ddB:d=%.3f,ametadata=mode=print:file=-", SilenceNoiseDB, MinSilence.Seconds())
	parser := newSilenceParser(recording.Duration)
	err = p.runTo(ctx, p.FFmpeg, "ffmpeg", parser, "-hide_banner", "-loglevel", "error", "-nostdin", "-xerror",
		"-protocol_whitelist", "file", "-i", path, "-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", filter, "-f", "null", "-")
	if err != nil {
		return nil, fmt.Errorf("detect waveform silence: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return parser.finish()
}

func parseSilence(data []byte, duration time.Duration) ([]Silence, error) {
	parser := newSilenceParser(duration)
	parser.Write(data)
	return parser.finish()
}

// maxSilenceLine bounds one metadata line; real lines are under 100 bytes.
const maxSilenceLine = 4096

var errSilenceMetadata = fmt.Errorf("%w: inconsistent waveform silence metadata", ErrInvalidMedia)

// silenceParser is an io.Writer that parses ametadata lines incrementally. After
// an error it keeps draining input and reports the error from finish.
type silenceParser struct {
	duration    time.Duration
	start, last time.Duration
	result      []Silence
	partial     []byte
	err         error
}

func newSilenceParser(duration time.Duration) *silenceParser {
	return &silenceParser{duration: duration, start: -1, result: []Silence{}}
}

func (s *silenceParser) Write(p []byte) (int, error) {
	n := len(p)
	for s.err == nil && len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			s.partial = append(s.partial, p...)
			if len(s.partial) > maxSilenceLine {
				s.err = errSilenceMetadata
			}
			break
		}
		s.partial = append(s.partial, p[:end]...)
		s.err = s.line(string(s.partial))
		s.partial = s.partial[:0]
		p = p[end+1:]
	}
	return n, nil
}

func (s *silenceParser) line(line string) error {
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.HasPrefix(line, "frame:") {
		return nil
	}
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return errSilenceMetadata
	}
	timestamp, err := seconds(value, false)
	if err != nil || timestamp < 0 || timestamp > s.duration {
		return errSilenceMetadata
	}
	switch key {
	case "lavfi.silence_start":
		if s.start >= 0 || timestamp < s.last || timestamp >= s.duration {
			return errSilenceMetadata
		}
		s.start = timestamp
	case "lavfi.silence_end":
		if s.start < 0 || timestamp <= s.start {
			return errSilenceMetadata
		}
		s.result = append(s.result, Silence{s.start, timestamp})
		s.start, s.last = -1, timestamp
	case "lavfi.silence_duration":
		// FFmpeg also emits a redundant duration beside each end timestamp.
	default:
		return errSilenceMetadata
	}
	return nil
}

func (s *silenceParser) finish() ([]Silence, error) {
	if s.err == nil && len(s.partial) > 0 {
		s.err = s.line(string(s.partial))
		s.partial = s.partial[:0]
	}
	if s.err != nil {
		return nil, s.err
	}
	// ametadata has no end frame to print when silence continues through EOF.
	if s.start >= 0 {
		s.result = append(s.result, Silence{s.start, s.duration})
		s.start = -1
	}
	return s.result, nil
}
