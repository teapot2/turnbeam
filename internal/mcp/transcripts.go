package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/transcript"
)

// maxTranscriptBytes bounds a sidecar read; larger files fail rather than truncate.
const maxTranscriptBytes = 256 << 20

type speakerInfo struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

type timeRange struct {
	StartSeconds *float64 `json:"start_seconds,omitempty"`
	EndSeconds   *float64 `json:"end_seconds,omitempty"`
}

// transcriptInfo describes a transcript without repeating its text.
type transcriptInfo struct {
	Transcript      string        `json:"transcript"`
	Recording       string        `json:"recording"`
	DurationSeconds float64       `json:"duration_seconds"`
	Engine          string        `json:"engine"`
	Model           string        `json:"model,omitempty"`
	Speakers        []speakerInfo `json:"speakers"`
	Turns           int           `json:"turns"`
	UnknownTurns    int           `json:"unknown_turns"`
	Format          string        `json:"format,omitempty"`
	Range           *timeRange    `json:"range,omitempty"`
}

// sidecarPath finds the canonical JSON for a recording or sidecar path.
func (s *Server) sidecarPath(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(expanded)
	candidate := clean
	if !strings.HasSuffix(strings.ToLower(clean), ".json") {
		candidate = sidecarFor(clean)
	}
	real, err := s.roots.resolve(candidate, true)
	switch {
	case errors.Is(err, os.ErrNotExist) && candidate != clean:
		return "", argumentError("no transcript for %s yet (expected %s); call transcribe first", path, candidate)
	case errors.Is(err, ErrOutsideRoots):
		return "", fmt.Errorf("%w: %w: %s", ErrArgument, ErrOutsideRoots, path)
	}
	return real, err
}

func loadTranscript(path string) (*transcript.Transcript, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTranscriptBytes {
		return nil, fmt.Errorf("%w: expected a regular file under %d bytes", output.ErrSource, maxTranscriptBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return transcript.Decode(data)
}

func describeTranscript(path string, whole, shown *transcript.Transcript, format transcript.Format, window *timeRange) transcriptInfo {
	info := transcriptInfo{Transcript: path, Recording: whole.Recording.Name, DurationSeconds: float64(whole.Recording.DurationMS) / 1000,
		Engine: whole.Pipeline.Engine, Model: whole.Pipeline.MetaModel, Speakers: []speakerInfo{}, Turns: len(shown.Turns), Format: string(format), Range: window}
	if info.Engine == "" {
		info.Engine = transcript.EngineMeta
	}
	if whole.Pipeline.Engine == transcript.EngineLocal {
		info.Model = whole.Pipeline.ASRModel
	}
	for _, speaker := range whole.Speakers {
		name := fmt.Sprintf("Speaker %d", speaker.Index)
		if speaker.Name != nil {
			name = *speaker.Name
		}
		info.Speakers = append(info.Speakers, speakerInfo{Index: speaker.Index, Name: name})
	}
	for _, turn := range shown.Turns {
		if turn.SpeakerID == nil {
			info.UnknownTurns++
		}
	}
	return info
}

type readArgs struct {
	Path         string   `json:"path"`
	Format       string   `json:"format"`
	Timestamps   string   `json:"timestamps"`
	StartSeconds *float64 `json:"start_seconds"`
	EndSeconds   *float64 `json:"end_seconds"`
}

func readTranscript(ctx context.Context, s *Server, _ *call, raw json.RawMessage) (*toolResult, error) {
	var args readArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	opts, err := renderOptions(args.Format, args.Timestamps, "")
	if err != nil {
		return nil, err
	}
	sidecar, err := s.sidecarPath(args.Path)
	if err != nil {
		return nil, err
	}
	whole, err := loadTranscript(sidecar)
	if err != nil {
		return nil, err
	}
	shown, window, err := window(whole, args.StartSeconds, args.EndSeconds)
	if err != nil {
		return nil, err
	}
	data, err := transcript.Render(ctx, shown, opts)
	if err != nil {
		return nil, err
	}
	return textResult(string(data), describeTranscript(sidecar, whole, shown, opts.Format, window)), nil
}

// window keeps the turns that overlap [start, end). Everything else is shared
// with the input, so the subset still validates and renders.
func window(t *transcript.Transcript, start, end *float64) (*transcript.Transcript, *timeRange, error) {
	if start == nil && end == nil {
		return t, nil, nil
	}
	if start != nil && (*start < 0 || *start != *start) || end != nil && (*end < 0 || *end != *end) {
		return nil, nil, argumentError("start_seconds and end_seconds must be nonnegative numbers")
	}
	if start != nil && end != nil && *end <= *start {
		return nil, nil, argumentError("end_seconds must be greater than start_seconds")
	}
	subset := *t
	subset.Turns = []transcript.Turn{}
	for _, turn := range t.Turns {
		if start != nil && float64(turn.EndMS) <= *start*1000 || end != nil && float64(turn.StartMS) >= *end*1000 {
			continue
		}
		subset.Turns = append(subset.Turns, turn)
	}
	return &subset, &timeRange{StartSeconds: start, EndSeconds: end}, nil
}

type renameArgs struct {
	Path  string            `json:"path"`
	Names map[string]string `json:"names"`
}

func renameSpeakers(ctx context.Context, s *Server, _ *call, raw json.RawMessage) (*toolResult, error) {
	var args renameArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Names) == 0 {
		return nil, argumentError("names must map at least one speaker index to a name")
	}
	names := make(map[int]string, len(args.Names))
	for key, name := range args.Names {
		index, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil || index < 1 || strings.TrimSpace(name) == "" {
			return nil, argumentError("names keys must be positive speaker indices and values nonempty names")
		}
		names[index] = name
	}
	sidecar, err := s.sidecarPath(args.Path)
	if err != nil {
		return nil, err
	}
	if err := output.Rename(ctx, sidecar, names); err != nil {
		return nil, err
	}
	whole, err := loadTranscript(sidecar)
	if err != nil {
		return nil, err
	}
	info := describeTranscript(sidecar, whole, whole, "", nil)
	return result(map[string]any{"transcript": sidecar, "renamed": len(names), "speakers": info.Speakers,
		"hint": "Existing exported files keep the old names; call export or read_transcript for the new ones."})
}

type exportArgs struct {
	Path       string `json:"path"`
	Output     string `json:"output"`
	Format     string `json:"format"`
	Timestamps string `json:"timestamps"`
}

func export(ctx context.Context, s *Server, _ *call, raw json.RawMessage) (*toolResult, error) {
	var args exportArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Output) == "" || strings.HasSuffix(args.Output, string(filepath.Separator)) {
		return nil, argumentError("output must name a file")
	}
	sidecar, err := s.sidecarPath(args.Path)
	if err != nil {
		return nil, err
	}
	out, err := s.roots.resolve(args.Output, false)
	if err != nil {
		return nil, err
	}
	opts, err := renderOptions(args.Format, args.Timestamps, out)
	if err != nil {
		return nil, err
	}
	if err := output.Export(ctx, sidecar, out, opts, nil); err != nil {
		return nil, err
	}
	return result(map[string]any{"transcript": sidecar, "output": out, "format": string(opts.Format)})
}
