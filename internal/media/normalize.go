package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SampleRate is the normalized mono PCM16 sample rate.
const SampleRate = 24000

// Version identifies normalization and extracted PCM behavior in recovery records.
const Version = "pcm24-v1"

// ErrCleanup reports temporary audio that could not be removed.
var ErrCleanup = errors.New("temporary audio cleanup failed; inspect the temporary directory")

const maxSamples = math.MaxInt64/int64(time.Second)*SampleRate + math.MaxInt64%int64(time.Second)*SampleRate/int64(time.Second)

// Prepared owns a verified temporary WAV. Call Close after all consumers finish,
// including when the enclosing operation is canceled. No audio is held in Go RAM.
type Prepared struct {
	Path     string
	Source   Info
	Samples  int64
	Duration time.Duration
	dir      string
}

// ValidTimeline checks positive, representable sample/duration metadata, not files.
func (p *Prepared) ValidTimeline() bool {
	return p != nil && p.Samples > 0 && p.Samples <= maxSamples && p.Duration == SampleDuration(p.Samples)
}

// Close removes only this preparation's private temporary directory; it is idempotent.
func (p *Prepared) Close() error {
	if err := os.RemoveAll(p.dir); err != nil {
		return errors.Join(ErrCleanup, err)
	}
	return nil
}

// Prepare probes and decodes one local recording into a private 24-kHz mono PCM16
// WAV. Failure/cancellation removes partial audio; success transfers ownership.
func (p Processor) Prepare(ctx context.Context, path string) (_ *Prepared, err error) {
	source, err := p.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve recording: %w", err)
	}
	dir, err := os.MkdirTemp(p.TempDir, "turnbeam-media-")
	if err != nil {
		return nil, fmt.Errorf("create private media directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			if cleanup := os.RemoveAll(dir); cleanup != nil {
				err = errors.Join(err, ErrCleanup, cleanup)
			}
		}
	}()
	output := filepath.Join(dir, "normalized.wav")
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create temporary WAV: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close temporary WAV: %w", err)
	}
	expected := SamplesCeil(source.Duration)
	if expected >= maxSamples {
		return nil, fmt.Errorf("%w: recording timeline is too large", ErrInvalidMedia)
	}
	// Explicitly subtract the container origin. -start_at_zero can instead rebase
	// to the selected audio stream when video is excluded, losing its initial delay.
	origin := strconv.FormatFloat(source.start.Seconds(), 'f', 9, 64)
	filter := fmt.Sprintf("asetpts=PTS-(%s)/TB,aresample=%d:async=1:first_pts=0:min_hard_comp=0,apad=whole_len=%d", origin, SampleRate, expected)
	_, err = p.run(ctx, p.FFmpeg, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-xerror", "-y",
		"-copyts", "-protocol_whitelist", "file", "-i", abs,
		"-map", "0:"+strconv.Itoa(source.StreamIndex), "-vn", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-filter_threads", "1", "-af", filter, "-ar", "24000", "-ac", "1", "-c:a", "pcm_s16le", "-threads", "1",
		"-rf64", "auto", "-f", "wav", output)
	if err != nil {
		return nil, fmt.Errorf("normalize recording: %w", err)
	}
	wav, err := p.Probe(ctx, output)
	if err != nil {
		return nil, fmt.Errorf("verify normalized WAV: %w", err)
	}
	if !wav.normalized() {
		return nil, fmt.Errorf("%w: normalized WAV must be 24-kHz mono signed PCM16 at time zero", ErrInvalidMedia)
	}
	if wav.samples < expected-1 || wav.samples > expected+1 {
		return nil, fmt.Errorf("%w: normalized duration differs from the recording timeline", ErrInvalidMedia)
	}
	stat, err := os.Stat(output)
	if err != nil {
		return nil, fmt.Errorf("inspect normalized WAV: %w", err)
	}
	if wav.samples <= 0 || stat.Size() < 44+wav.samples*2 {
		return nil, fmt.Errorf("%w: normalized WAV is empty or truncated", ErrInvalidMedia)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	complete = true
	return &Prepared{Path: output, Source: source, Samples: wav.samples, Duration: SampleDuration(wav.samples), dir: dir}, nil
}

func (i Info) normalized() bool {
	return i.format == "wav" && i.Codec == "pcm_s16le" && i.sampleFmt == "s16" && i.bits == 16 && i.SampleRate == SampleRate && i.Channels == 1 && i.timeBase == "1/24000" && i.AudioOffset == 0 && i.start == 0
}

// SamplesCeil converts a duration to samples at SampleRate, rounding up, without
// overflowing long recordings.
func SamplesCeil(d time.Duration) int64 {
	return int64(d/time.Second)*SampleRate + (int64(d%time.Second)*SampleRate+int64(time.Second)-1)/int64(time.Second)
}

// SampleDuration converts samples at SampleRate to a duration, rounding down,
// without overflowing long recordings.
func SampleDuration(samples int64) time.Duration {
	return time.Duration(samples/SampleRate)*time.Second + time.Duration(samples%SampleRate)*time.Second/SampleRate
}
