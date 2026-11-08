package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	MaxChunkSamples = 9 * 60 * SampleRate
	MaxRequestBytes = 32_000_000
	// Extraction leaves this allowance for multipart framing and request JSON.
	// The HTTP client must check the actual complete body against MaxRequestBytes.
	RequestHeadroom = 1_000_000
)

var ErrChunkSize = errors.New("audio chunk exceeds upload budget")

// Clip owns one API-ready WAV and its exact half-open range on the original
// recording's sample clock. Close removes only the clip, never its source.
type Clip struct {
	*Prepared
	StartSample, EndSample int64
	Bytes                  int64
}

// Extract reads a complete normalized recording without modifying it. There is
// no timestamp seek, resampling, padding or rounding tolerance at chunk cuts.
func (p Processor) Extract(ctx context.Context, recording *Prepared, start, end int64) (_ *Clip, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !recording.ValidTimeline() || start < 0 || start >= end || end > recording.Samples {
		return nil, fmt.Errorf("%w: invalid normalized recording or sample range", ErrInvalidMedia)
	}
	count := end - start
	if count > MaxChunkSamples {
		return nil, ErrChunkSize
	}
	input, err := localInput(recording.Path)
	if err != nil {
		return nil, err
	}
	wav, err := p.Probe(ctx, input)
	if err != nil {
		return nil, err
	}
	if !wav.normalized() || wav.samples != recording.Samples {
		return nil, fmt.Errorf("%w: source is not the complete normalized WAV", ErrInvalidMedia)
	}
	sourceStat, err := os.Stat(input)
	if err != nil {
		return nil, fmt.Errorf("inspect normalized source: %w", err)
	}
	if sourceStat.Size() < 44+recording.Samples*2 {
		return nil, fmt.Errorf("%w: normalized source is truncated", ErrInvalidMedia)
	}
	dir, err := os.MkdirTemp(p.TempDir, "turnbeam-chunk-")
	if err != nil {
		return nil, fmt.Errorf("create private chunk directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			if cleanup := os.RemoveAll(dir); cleanup != nil {
				err = errors.Join(err, ErrCleanup, cleanup)
			}
		}
	}()
	output := filepath.Join(dir, "chunk.wav")
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create private chunk WAV: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close private chunk WAV: %w", err)
	}
	filter := fmt.Sprintf("atrim=start_sample=%d:end_sample=%d,asetpts=PTS-STARTPTS", start, end)
	_, err = p.run(ctx, p.FFmpeg, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-xerror", "-y",
		"-protocol_whitelist", "file", "-i", input, "-map", "0:a:0", "-vn", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-filter_threads", "1", "-af", filter, "-c:a", "pcm_s16le", "-threads", "1", "-fflags", "+bitexact",
		"-rf64", "never", "-fs", fmt.Sprint(MaxRequestBytes-RequestHeadroom-1), "-f", "wav", output)
	if err != nil {
		return nil, fmt.Errorf("extract audio chunk: %w", err)
	}
	stat, err := os.Lstat(output)
	if err != nil {
		return nil, fmt.Errorf("inspect chunk WAV: %w", err)
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() < 44+count*2 {
		return nil, fmt.Errorf("%w: chunk WAV is nonregular, insecure or truncated", ErrInvalidMedia)
	}
	if stat.Size() >= MaxRequestBytes-RequestHeadroom {
		return nil, ErrChunkSize
	}
	wav, err = p.Probe(ctx, output)
	if err != nil {
		return nil, fmt.Errorf("verify chunk WAV: %w", err)
	}
	if !wav.normalized() || wav.samples != count {
		return nil, fmt.Errorf("%w: chunk format or exact sample count differs from plan", ErrInvalidMedia)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	complete = true
	return &Clip{Prepared: &Prepared{Path: output, Source: recording.Source, Samples: count, Duration: SampleDuration(count), dir: dir},
		StartSample: start, EndSample: end, Bytes: stat.Size()}, nil
}
