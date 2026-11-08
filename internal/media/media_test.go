package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/testutil"
)

const inputJSON = `{"streams":[{"index":0,"codec_type":"video"},{"index":1,"codec_type":"audio","codec_name":"aac","sample_rate":"48000","channels":2,"start_time":"5.250000","duration":"1.500000"},{"index":2,"codec_type":"audio","codec_name":"aac","sample_rate":"48000","channels":2,"start_time":"5.250000","duration":"1.500000","disposition":{"default":1}}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","start_time":"5.000000","duration":"2.000000"}}`
const outputJSON = `{"streams":[{"index":0,"codec_type":"audio","codec_name":"pcm_s16le","sample_rate":"24000","channels":1,"bits_per_sample":16,"sample_fmt":"s16","time_base":"1/24000","duration_ts":48000,"start_time":"0.000000","duration":"2.000000"}],"format":{"format_name":"wav","duration":"2.000000"}}`

func inputFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recording with spaces.mp4")
	if err := os.WriteFile(path, []byte("fake media"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeProcessor(t *testing.T, mode, ready string) Processor {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Processor{TempDir: t.TempDir(), command: func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestMediaProcess$", "--", tool}, args...)...)
		cmd.Env = append(os.Environ(), "TURNBEAM_MEDIA_HELPER=1", "TURNBEAM_MEDIA_MODE="+mode, "TURNBEAM_MEDIA_READY="+ready, "GORACE=atexit_sleep_ms=0")
		return cmd
	}}
}

func TestMediaProcess(t *testing.T) {
	if os.Getenv("TURNBEAM_MEDIA_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	tool, args := args[1], args[2:]
	mode := os.Getenv("TURNBEAM_MEDIA_MODE")
	if tool == "ffmpeg" && strings.HasPrefix(mode, "silence") {
		switch mode {
		case "silence-hang":
			os.WriteFile(os.Getenv("TURNBEAM_MEDIA_READY"), []byte("ready"), 0600)
			time.Sleep(time.Hour)
		case "silence-failure":
			fmt.Fprint(os.Stderr, "private native metadata")
			os.Exit(7)
		case "silence-invalid":
			fmt.Print("lavfi.silence_end=1\n")
		case "silence-many":
			// More metadata than the 1 MiB buffer used for probe output.
			out := bufio.NewWriter(os.Stdout)
			for i := 0; i < 12000; i++ {
				start := float64(i)
				fmt.Fprintf(out, "frame:%d pts:%d pts_time:%.6f\nlavfi.silence_start=%.6f\nframe:%d pts:%d pts_time:%.6f\nlavfi.silence_end=%.6f\nlavfi.silence_duration=0.500000\n",
					2*i, i*24000, start, start, 2*i+1, i*24000+12000, start+0.5, start+0.5)
			}
			out.Flush()
		default:
			fmt.Print("frame:0 pts:0 pts_time:0\nlavfi.silence_start=0.25\nlavfi.silence_end=1.25\nlavfi.silence_duration=1\n")
		}
		os.Exit(0)
	}
	if mode == "hang-probe" || mode == "hang-normalize" && tool == "ffmpeg" {
		if tool == "ffmpeg" {
			os.WriteFile(args[len(args)-1], []byte("partial WAV"), 0600)
		}
		os.WriteFile(os.Getenv("TURNBEAM_MEDIA_READY"), []byte("ready"), 0600)
		time.Sleep(time.Hour)
	}
	if tool == "ffprobe" {
		data := inputJSON
		if filepath.Base(args[len(args)-1]) == "chunk.wav" {
			info, err := os.Stat(args[len(args)-1])
			if err != nil {
				os.Exit(2)
			}
			count := (info.Size() - 44) / 2
			if mode == "extract-budget" {
				count = 48000
			}
			if mode == "extract-count" {
				count--
			}
			var metadata map[string]any
			json.Unmarshal([]byte(outputJSON), &metadata)
			stream := metadata["streams"].([]any)[0].(map[string]any)
			stream["duration_ts"], stream["duration"] = count, fmt.Sprintf("%.9f", float64(count)/SampleRate)
			metadata["format"].(map[string]any)["duration"] = stream["duration"]
			if mode == "extract-format" {
				stream["channels"] = 2
			}
			if mode == "extract-offset" {
				stream["start_time"] = "1.000000"
				metadata["format"].(map[string]any)["start_time"] = "1.000000"
			}
			json.NewEncoder(os.Stdout).Encode(metadata)
			os.Exit(0)
		}
		if filepath.Base(args[len(args)-1]) == "normalized.wav" {
			data = outputJSON
			if mode == "wrong-format" {
				data = strings.Replace(data, `"channels":1`, `"channels":2`, 1)
			}
			if mode == "wrong-duration" {
				data = strings.Replace(data, `"duration_ts":48000`, `"duration_ts":47000`, 1)
			}
		}
		switch mode {
		case "malformed":
			data = "not JSON"
		case "no-audio":
			data = `{"streams":[{"codec_type":"video"}]}`
		case "oversized":
			data = strings.Repeat("x", 1<<20+1)
		}
		fmt.Print(data)
	} else {
		if mode == "extract-missing" || mode == "extract-symlink" {
			output := args[len(args)-1]
			os.Remove(output)
			if mode == "extract-symlink" {
				for i := range args {
					if args[i] == "-i" && i+1 < len(args) {
						if err := os.Symlink(args[i+1], output); err != nil {
							os.Exit(2)
						}
					}
				}
			}
			os.Exit(0)
		}
		if mode == "decode-failure" || mode == "extract-failure" {
			os.WriteFile(args[len(args)-1], []byte("partial WAV"), 0600)
			fmt.Fprint(os.Stderr, "private native metadata")
			os.Exit(1)
		}
		f, err := os.OpenFile(args[len(args)-1], os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			os.Exit(2)
		}
		size := int64(44 + 48000*2)
		if filepath.Base(args[len(args)-1]) == "chunk.wav" {
			for i := range args {
				if args[i] == "-af" && i+1 < len(args) {
					var start, end int64
					if n, _ := fmt.Sscanf(args[i+1], "atrim=start_sample=%d:end_sample=%d", &start, &end); n != 2 {
						os.Exit(2)
					}
					size = 44 + (end-start)*2
				}
			}
			if mode == "extract-budget" {
				size = MaxRequestBytes - RequestHeadroom
			}
			if mode == "extract-truncated" {
				size = 44
			}
		}
		if mode == "truncated" {
			size = 44
		}
		if err := f.Truncate(size); err != nil {
			os.Exit(2)
		}
		f.Close()
		if mode == "extract-permission" {
			os.Chmod(args[len(args)-1], 0644)
		}
	}
	os.Exit(0)
}

func TestProbeSelectionAndTimeline(t *testing.T) {
	info, err := parseProbe([]byte(inputJSON))
	if err != nil || info.StreamIndex != 2 || info.AudioOffset != 250*time.Millisecond || info.Duration != 2*time.Second || info.SampleRate != 48000 || info.Channels != 2 {
		t.Fatalf("wrong selected stream/timeline: %+v, %v", info, err)
	}
	first := strings.Replace(inputJSON, `"default":1`, `"default":0`, 1)
	info, err = parseProbe([]byte(first))
	if err != nil || info.StreamIndex != 1 {
		t.Fatalf("first audio fallback: %+v, %v", info, err)
	}
	fallback := strings.Replace(inputJSON, `"duration":"2.000000"`, `"duration":"N/A"`, 1)
	info, err = parseProbe([]byte(fallback))
	if err != nil || info.Duration != 1750*time.Millisecond {
		t.Fatalf("stream duration fallback: %+v, %v", info, err)
	}
}

func TestProbeMalformed(t *testing.T) {
	for name, data := range map[string]string{
		"bad JSON": "{", "no streams": "{}", "video only": `{"streams":[{"codec_type":"video"}]}`,
		"missing index":    strings.ReplaceAll(inputJSON, `"index":2,`, ""),
		"unknown codec":    strings.ReplaceAll(inputJSON, `"aac"`, `"unknown"`),
		"no channels":      strings.ReplaceAll(inputJSON, `"channels":2`, `"channels":0`),
		"invalid rate":     strings.ReplaceAll(inputJSON, `"48000"`, `"invalid"`),
		"zero duration":    strings.ReplaceAll(inputJSON, `"duration":"2.000000"`, `"duration":"0"`),
		"negative length":  strings.ReplaceAll(inputJSON, `"duration":"2.000000"`, `"duration":"-1"`),
		"NaN duration":     strings.ReplaceAll(inputJSON, `"duration":"2.000000"`, `"duration":"NaN"`),
		"infinite length":  strings.ReplaceAll(inputJSON, `"duration":"2.000000"`, `"duration":"Inf"`),
		"duration units":   strings.ReplaceAll(inputJSON, `"duration":"2.000000"`, `"duration":"1m"`),
		"huge timestamp":   strings.ReplaceAll(inputJSON, `"5.000000"`, `"999999999999999"`),
		"bad audio start":  strings.ReplaceAll(inputJSON, `"5.250000"`, `"broken"`),
		"offset underflow": strings.ReplaceAll(strings.ReplaceAll(inputJSON, `"5.000000"`, `"9223372036.854775807"`), `"5.250000"`, `"-9223372036.854775808"`),
		"offset overflow":  strings.ReplaceAll(strings.ReplaceAll(inputJSON, `"5.000000"`, `"-9223372036.854775808"`), `"5.250000"`, `"9223372036.854775807"`),
		"end overflow":     strings.ReplaceAll(strings.ReplaceAll(inputJSON, `"5.000000"`, `"0"`), `"5.250000"`, `"9223372036.854775807"`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseProbe([]byte(data))
			if !errors.Is(err, ErrInvalidMedia) && !errors.Is(err, ErrNoAudio) {
				t.Fatalf("malformed media accepted: %v", err)
			}
		})
	}
}

func TestPrepareAndOwnership(t *testing.T) {
	p := fakeProcessor(t, "", "")
	var commands [][]string
	command := p.command
	p.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		commands = append(commands, append([]string{tool}, args...))
		return command(ctx, tool, args...)
	}
	prepared, err := p.Prepare(context.Background(), inputFile(t))
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Samples != 48000 || prepared.Duration != 2*time.Second || prepared.Source.StreamIndex != 2 {
		t.Fatalf("wrong prepared metadata: %+v", prepared)
	}
	for path, mode := range map[string]os.FileMode{prepared.Path: 0600, prepared.dir: 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("insecure temporary permissions: %s, %v", path, err)
		}
	}
	args := strings.Join(commands[1], " ")
	for _, flag := range []string{"-map 0:2", "-copyts", "asetpts=PTS-(5.000000000)/TB", "first_pts=0", "min_hard_comp=0", "apad=whole_len=48000", "-ar 24000 -ac 1 -c:a pcm_s16le", "-map_metadata -1", "-protocol_whitelist file"} {
		if !strings.Contains(args, flag) {
			t.Errorf("missing normalization contract %q", flag)
		}
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary audio remains: %v", err)
	}
}

func TestPreparationFailuresCleanUp(t *testing.T) {
	for _, mode := range []string{"malformed", "no-audio", "oversized", "decode-failure", "wrong-format", "wrong-duration", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			p := fakeProcessor(t, mode, "")
			prepared, err := p.Prepare(context.Background(), inputFile(t))
			if err == nil || prepared != nil || strings.Contains(err.Error(), "private native metadata") {
				t.Fatalf("partial preparation was returned: %+v, %v", prepared, err)
			}
			if mode == "decode-failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("subprocess error was lost: %v", err)
				}
			}
			assertEmpty(t, p.TempDir)
		})
	}
}

func TestCancellationReapsProcessAndCleansUp(t *testing.T) {
	for _, mode := range []string{"hang-probe", "hang-normalize"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			p := fakeProcessor(t, mode, ready)
			path := inputFile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := p.Prepare(ctx, path); done <- err }()
			testutil.WaitReady(t, ready, done, cancel)
			cancel()
			err := testutil.WaitError(t, done)
			var exit *exec.ExitError
			if !errors.Is(err, context.Canceled) || !errors.As(err, &exit) {
				t.Fatalf("cancellation or waited child status lost: %v", err)
			}
			assertEmpty(t, p.TempDir)
		})
	}
}

func TestLocalInputAndDependencies(t *testing.T) {
	for _, path := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "missing"), "https://example.invalid/audio.wav"} {
		if _, err := (Processor{}).Probe(context.Background(), path); err == nil {
			t.Fatalf("invalid local input accepted: %q", path)
		}
	}
	empty := filepath.Join(t.TempDir(), "empty.wav")
	os.WriteFile(empty, nil, 0600)
	if _, err := (Processor{}).Probe(context.Background(), empty); !errors.Is(err, ErrInvalidMedia) {
		t.Fatal(err)
	}
	if _, err := (Processor{FFprobe: filepath.Join(t.TempDir(), "missing-tool")}).Probe(context.Background(), inputFile(t)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependency error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Processor{}).Prepare(ctx, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancellation lost: %v", err)
	}
}

func TestPreparationDependencyAndStorageFailures(t *testing.T) {
	path := inputFile(t)
	p := fakeProcessor(t, "", "")
	p.TempDir = filepath.Join(t.TempDir(), "missing-directory")
	if prepared, err := p.Prepare(context.Background(), path); prepared != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("storage error lost: %+v, %v", prepared, err)
	}
	p = fakeProcessor(t, "", "")
	command := p.command
	p.command = func(ctx context.Context, tool string, args ...string) *exec.Cmd {
		if tool == "ffmpeg" {
			return exec.CommandContext(ctx, filepath.Join(p.TempDir, "missing-ffmpeg"), args...)
		}
		return command(ctx, tool, args...)
	}
	if prepared, err := p.Prepare(context.Background(), path); prepared != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing FFmpeg was not reported: %+v, %v", prepared, err)
	}
	assertEmpty(t, p.TempDir)
}

func TestSampleBoundaries(t *testing.T) {
	for _, samples := range []int64{1, 23999, 24000, 24001, 24000 * 5400} {
		d := sampleDuration(samples)
		if got := samplesFor(d); got != samples {
			t.Fatalf("sample boundary changed: %d -> %v -> %d", samples, d, got)
		}
	}
	for _, samples := range []int64{1, 24000, maxSamples} {
		if !(&Prepared{Samples: samples, Duration: sampleDuration(samples)}).ValidTimeline() {
			t.Fatal("valid sample timeline rejected")
		}
	}
	for _, p := range []*Prepared{nil, {}, {Samples: -1}, {Samples: maxSamples + 1}, {Samples: 24000, Duration: time.Millisecond}} {
		if p.ValidTimeline() {
			t.Fatal("invalid sample timeline accepted")
		}
	}
}

func TestTemporaryCleanupFailureIsExplicit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "owned")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	err := (&Prepared{dir: dir}).Close()
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, os.ErrPermission) {
		t.Fatal("cleanup failure was hidden", err)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial media remains: %v, %v", entries, err)
	}
}
