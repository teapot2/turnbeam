package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

var (
	// ErrInvalidMedia identifies unreadable or invalid media/timeline metadata.
	ErrInvalidMedia = errors.New("invalid media")
	ErrNoAudio      = errors.New("recording has no audio stream")
)

// Info describes the selected audio and the recording-relative timeline.
type Info struct {
	StreamIndex int
	Codec       string
	SampleRate  int
	Channels    int
	Duration    time.Duration
	AudioOffset time.Duration
	start       time.Duration
	format      string
	bits        int
	sampleFmt   string
	timeBase    string
	samples     int64
}

type probeStream struct {
	Index         *int   `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	SampleRate    string `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	SampleFormat  string `json:"sample_fmt"`
	Start         string `json:"start_time"`
	Duration      string `json:"duration"`
	DurationTS    int64  `json:"duration_ts"`
	TimeBase      string `json:"time_base"`
	Disposition   struct {
		Default int `json:"default"`
	} `json:"disposition"`
}

// Probe validates a readable, nonempty local file and selects the default audio
// stream, falling back to the first audio stream. It never mixes separate tracks.
func (p Processor) Probe(ctx context.Context, path string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	path, err := localInput(path)
	if err != nil {
		return Info{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := p.run(ctx, p.FFprobe, "ffprobe", "-v", "error", "-protocol_whitelist", "file",
		"-show_entries", "format=format_name,start_time,duration:stream=index,codec_type,codec_name,sample_rate,channels,bits_per_sample,sample_fmt,start_time,duration,duration_ts,time_base:stream_disposition=default",
		"-of", "json", "-i", path)
	if err != nil {
		return Info{}, fmt.Errorf("probe recording: %w", err)
	}
	return parseProbe(data)
}

func localInput(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: input path is empty", ErrInvalidMedia)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve recording: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect recording: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("%w: input must be a nonempty regular file", ErrInvalidMedia)
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("open recording: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close recording: %w", err)
	}
	return abs, nil
}

func parseProbe(data []byte) (Info, error) {
	var result struct {
		Streams []probeStream `json:"streams"`
		Format  struct {
			Name     string `json:"format_name"`
			Start    string `json:"start_time"`
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Info{}, fmt.Errorf("%w: decode FFprobe metadata: %w", ErrInvalidMedia, err)
	}
	var selected *probeStream
	for i := range result.Streams {
		s := &result.Streams[i]
		if s.CodecType == "audio" && (selected == nil || selected.Disposition.Default == 0 && s.Disposition.Default != 0) {
			selected = s
		}
	}
	if selected == nil {
		return Info{}, ErrNoAudio
	}
	s := selected
	rate, err := strconv.Atoi(s.SampleRate)
	if s.Index == nil || *s.Index < 0 || err != nil || rate <= 0 || s.Channels <= 0 || s.CodecName == "" || s.CodecName == "unknown" {
		return Info{}, fmt.Errorf("%w: selected audio has invalid index, codec, rate or channels", ErrInvalidMedia)
	}
	start, err := seconds(result.Format.Start, true)
	if err != nil {
		return Info{}, err
	}
	audioStart := start
	if s.Start != "" && s.Start != "N/A" {
		audioStart, err = seconds(s.Start, false)
		if err != nil {
			return Info{}, err
		}
	}
	// Guard subtraction before deriving an offset from signed container timestamps.
	if start > 0 && audioStart < time.Duration(math.MinInt64)+start || start < 0 && audioStart > time.Duration(math.MaxInt64)+start {
		return Info{}, fmt.Errorf("%w: audio offset overflows", ErrInvalidMedia)
	}
	offset := audioStart - start
	duration, err := seconds(result.Format.Duration, true)
	if err != nil || duration < 0 {
		return Info{}, fmt.Errorf("%w: invalid recording duration", ErrInvalidMedia)
	}
	if result.Format.Duration != "" && result.Format.Duration != "N/A" && duration == 0 {
		return Info{}, fmt.Errorf("%w: recording duration is zero", ErrInvalidMedia)
	}
	if s.Duration != "" && s.Duration != "N/A" {
		audioDuration, err := seconds(s.Duration, false)
		if err != nil || audioDuration <= 0 || offset > time.Duration(math.MaxInt64)-audioDuration {
			return Info{}, fmt.Errorf("%w: invalid audio duration", ErrInvalidMedia)
		}
		if end := offset + audioDuration; end > duration {
			duration = end
		}
	}
	if duration <= 0 || offset >= duration {
		return Info{}, fmt.Errorf("%w: positive recording duration and audio within its timeline are required", ErrInvalidMedia)
	}
	return Info{
		StreamIndex: *s.Index, Codec: s.CodecName, SampleRate: rate, Channels: s.Channels,
		Duration: duration, AudioOffset: offset, start: start, format: result.Format.Name, bits: s.BitsPerSample,
		sampleFmt: s.SampleFormat, timeBase: s.TimeBase, samples: s.DurationTS,
	}, nil
}

func seconds(value string, optional bool) (time.Duration, error) {
	if optional && (value == "" || value == "N/A") {
		return 0, nil
	}
	if _, err := strconv.ParseFloat(value, 64); err != nil {
		return 0, fmt.Errorf("%w: invalid FFprobe timestamp", ErrInvalidMedia)
	}
	d, err := time.ParseDuration(value + "s")
	if err != nil {
		return 0, fmt.Errorf("%w: invalid FFprobe timestamp", ErrInvalidMedia)
	}
	return d, nil
}
