// Package alignment assembles original-time turns without treating Meta labels as identities.
package alignment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/transcript"
)

var ErrAssembly = errors.New("invalid transcript assembly input")

// Assemble combines a complete plan and unordered results on the recording clock.
// Raw turns are preserved; canonical turns remain unattributed until alignment.
// The result is detached from input. Errors return no partial transcript.
func Assemble(ctx context.Context, recording transcript.Recording, pipeline transcript.Pipeline, plan []chunking.Chunk, results []meta.Result) (*transcript.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(plan) == 0 || len(results) != len(plan) {
		return nil, ErrAssembly
	}
	if err := chunking.Validate(ctx, plan, plan[len(plan)-1].EndSample); err != nil {
		return nil, errors.Join(ErrAssembly, err)
	}
	if plan[len(plan)-1].EndMS() != recording.DurationMS {
		return nil, ErrAssembly
	}
	byID := make(map[string]int, len(plan))
	for i, c := range plan {
		byID[c.ID] = i
	}
	t := &transcript.Transcript{SchemaVersion: transcript.SchemaVersion, Recording: recording, Pipeline: pipeline,
		Speakers: []transcript.Speaker{}, Turns: []transcript.Turn{}, SourceChunks: make([]transcript.SourceChunk, len(plan))}
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, r := result.Chunk, result.Response
		i, exists := byID[c.ID]
		if !exists || seen[c.ID] || c != plan[i] || r == nil || r.AudioDurationMS < 0 || !utf8.ValidString(r.Transcript) || len(r.Turns) == 0 && strings.TrimSpace(r.Transcript) != "" {
			return nil, ErrAssembly
		}
		seen[c.ID] = true
		if r.Turns == nil {
			return nil, ErrAssembly
		}
		raw, err := copyRawTurns(ctx, r.Turns)
		if err != nil {
			return nil, err
		}
		t.SourceChunks[i] = transcript.SourceChunk{ID: c.ID, StartMS: c.StartMS(), EndMS: c.EndMS(), MetaSessionID: r.SessionID, RawTurns: raw}
	}
	// Validate source clocks before adding offsets, preventing overflow or repairs.
	if err := transcript.Validate(t); err != nil {
		return nil, errors.Join(ErrAssembly, err)
	}
	byChunk := make([][]transcript.Turn, len(plan))
	for i, c := range t.SourceChunks {
		for _, raw := range c.RawTurns {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if strings.TrimSpace(raw.Transcript) == "" {
				continue
			}
			reason := transcript.NotAligned
			if raw.StartMS == raw.EndMS {
				reason = transcript.InsufficientTiming
			}
			byChunk[i] = append(byChunk[i], transcript.Turn{StartMS: c.StartMS + raw.StartMS, EndMS: c.StartMS + raw.EndMS, Text: raw.Transcript,
				Attribution: transcript.Attribution{Status: transcript.Unknown, Reason: reason}, Source: transcript.Source{ChunkID: c.ID, MetaTurnID: raw.TurnID}})
		}
	}
	removed, err := duplicates(ctx, plan, byChunk)
	if err != nil {
		return nil, err
	}
	for _, turns := range byChunk {
		for _, turn := range turns {
			if !removed[turn.Source] {
				t.Turns = append(t.Turns, turn)
			}
		}
	}
	sort.Slice(t.Turns, func(i, j int) bool {
		a, b := t.Turns[i], t.Turns[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.EndMS != b.EndMS {
			return a.EndMS < b.EndMS
		}
		if a.Source.ChunkID != b.Source.ChunkID {
			return byID[a.Source.ChunkID] < byID[b.Source.ChunkID]
		}
		return a.Source.MetaTurnID < b.Source.MetaTurnID
	})
	for i := range t.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t.Turns[i].ID = fmt.Sprintf("turn_%06d", i+1)
	}
	if err := transcript.Validate(t); err != nil {
		return nil, errors.Join(ErrAssembly, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return t, nil
}
