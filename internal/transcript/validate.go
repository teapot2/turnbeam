package transcript

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

var (
	// ErrInvalid identifies malformed or inconsistent canonical data.
	ErrInvalid            = errors.New("invalid transcript")
	ErrUnsupportedVersion = errors.New("unsupported transcript schema version")
)

func invalid(field, problem string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, problem)
}

func versionError(version int) error {
	return fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
}

// Validate checks the completed contract without repairing, sorting or renaming it.
// Error messages identify schema fields, never echo recording text or names.
func Validate(t *Transcript) error {
	if t == nil {
		return invalid("transcript", "must not be null")
	}
	if t.SchemaVersion != 1 && t.SchemaVersion != SchemaVersion {
		return versionError(t.SchemaVersion)
	}
	r := t.Recording
	if !text(r.Name) || r.Name == "." || r.Name == ".." || strings.ContainsAny(r.Name, `/\`) {
		return invalid("recording.name", "must be a filename, not a path")
	}
	if r.DurationMS <= 0 {
		return invalid("recording.durationMs", "must be positive")
	}
	if digest, err := hex.DecodeString(r.SHA256); err != nil || len(digest) != 32 || hex.EncodeToString(digest) != r.SHA256 {
		return invalid("recording.sha256", "must be a lowercase SHA-256 hex digest")
	}
	local, err := validatePipeline(t.SchemaVersion, t.Pipeline)
	if err != nil {
		return err
	}
	if t.Speakers == nil || t.Turns == nil || t.SourceChunks == nil {
		return invalid("transcript", "speakers, turns and sourceChunks must be arrays, not null")
	}
	if local && len(t.SourceChunks) != 0 {
		return invalid("sourceChunks", "must be empty for the local engine")
	}
	method := TimelineOverlap
	if local {
		method = WordTimelineOverlap
	}
	speakers := make(map[string]bool, len(t.Speakers))
	for i, s := range t.Speakers {
		field := fmt.Sprintf("speakers[%d]", i)
		if !text(s.ID) || speakers[s.ID] || s.Index != i+1 {
			return invalid(field, "IDs must be unique and indices contiguous in display order")
		}
		if s.Name != nil && !text(*s.Name) {
			return invalid(field+".name", "must be nonempty text or null")
		}
		speakers[s.ID] = true
	}

	chunks := make(map[string]SourceChunk, len(t.SourceChunks))
	rawTurns := make(map[string]map[int64]RawTurn, len(t.SourceChunks))
	for i, c := range t.SourceChunks {
		field := fmt.Sprintf("sourceChunks[%d]", i)
		if _, exists := chunks[c.ID]; !text(c.ID) || exists || !text(c.MetaSessionID) {
			return invalid(field, "a unique ID and Meta session ID are required")
		}
		if c.StartMS == c.EndMS || !interval(c.StartMS, c.EndMS, r.DurationMS) {
			return invalid(field, "timestamps must describe a positive interval within the recording")
		}
		if i > 0 && c.StartMS < t.SourceChunks[i-1].StartMS {
			return invalid(field, "chunks must be in start-time order")
		}
		if c.RawTurns == nil {
			return invalid(field+".rawTurns", "must be an array, not null")
		}
		byID := make(map[int64]RawTurn, len(c.RawTurns))
		for j, raw := range c.RawTurns {
			path := fmt.Sprintf("%s.rawTurns[%d]", field, j)
			if _, exists := byID[raw.TurnID]; raw.TurnID < 0 || exists {
				return invalid(path+".turnId", "must be nonnegative and unique within its chunk")
			}
			if !interval(raw.StartMS, raw.EndMS, c.EndMS-c.StartMS) || !utf8.ValidString(raw.Transcript) {
				return invalid(path, "valid UTF-8 text and ordered chunk-local timestamps are required")
			}
			if raw.Speaker != nil && !utf8.ValidString(*raw.Speaker) {
				return invalid(path+".speaker", "must be valid UTF-8 or unavailable")
			}
			byID[raw.TurnID] = raw
		}
		chunks[c.ID], rawTurns[c.ID] = c, byID
	}

	turns := make(map[string]bool, len(t.Turns))
	sources := make(map[Source]bool, len(t.Turns))
	for i, turn := range t.Turns {
		field := fmt.Sprintf("turns[%d]", i)
		if !text(turn.ID) || turns[turn.ID] {
			return invalid(field+".id", "must be nonempty and unique")
		}
		turns[turn.ID] = true
		if !interval(turn.StartMS, turn.EndMS, r.DurationMS) || !text(turn.Text) {
			return invalid(field, "nonempty text and ordered recording-local timestamps are required")
		}
		if i > 0 && turn.StartMS < t.Turns[i-1].StartMS {
			return invalid(field, "turns must be in start-time order")
		}
		if err := validateAttribution(field, turn, speakers, method); err != nil {
			return err
		}
		if local {
			if turn.Source != (Source{}) {
				return invalid(field+".source", "local-engine turns have no Meta source")
			}
			continue
		}
		c, exists := chunks[turn.Source.ChunkID]
		raw, found := rawTurns[turn.Source.ChunkID][turn.Source.MetaTurnID]
		if !exists || !found {
			return invalid(field+".source", "must reference an existing chunk and Meta turn")
		}
		if sources[turn.Source] {
			return invalid(field+".source", "each Meta turn may back only one canonical turn")
		}
		sources[turn.Source] = true
		if !text(raw.Transcript) {
			return invalid(field+".source", "empty source text belongs only in rawTurns")
		}
		// Compare by subtraction so even near-MaxInt64 offsets cannot overflow.
		if turn.StartMS < c.StartMS || turn.StartMS-c.StartMS != raw.StartMS || turn.EndMS-c.StartMS != raw.EndMS {
			return invalid(field+".source", "canonical timestamps must equal raw timestamps plus the chunk offset")
		}
	}
	return nil
}

// validatePipeline checks provenance and reports whether the transcript comes
// from the local engine. Version 1 has no engine field and is always Meta.
func validatePipeline(version int, p Pipeline) (local bool, err error) {
	if !text(p.DiarizationModel) || !text(p.DiarizationRevision) || !text(p.RuntimeVersion) {
		return false, invalid("pipeline", "model, revision and runtime provenance are required")
	}
	switch {
	case version == 1 && p.Engine == "", version != 1 && p.Engine == EngineMeta:
		if !text(p.MetaModel) {
			return false, invalid("pipeline", "model, revision and runtime provenance are required")
		}
		if p.MetaMode != "DIARIZATION" && p.MetaMode != "ENDPOINTING" {
			return false, invalid("pipeline.metaMode", "must be DIARIZATION or ENDPOINTING")
		}
		if p.ASRModel != "" || p.ASRRevision != "" {
			return false, invalid("pipeline", "local model provenance belongs only to the local engine")
		}
		return false, nil
	case version != 1 && p.Engine == EngineLocal:
		if !text(p.ASRModel) || !text(p.ASRRevision) {
			return false, invalid("pipeline", "local model and revision provenance are required")
		}
		if p.MetaModel != "" || p.MetaMode != "" {
			return false, invalid("pipeline", "Meta provenance belongs only to the Meta engine")
		}
		return true, nil
	case version == 1:
		return false, invalid("pipeline.engine", "requires schema version 2")
	}
	return false, invalid("pipeline.engine", "must be meta or local")
}

func validateAttribution(field string, turn Turn, speakers map[string]bool, method AttributionMethod) error {
	a := turn.Attribution
	if turn.StartMS == turn.EndMS && a.Status != Unknown {
		return invalid(field+".attribution", "zero-duration text must remain unknown")
	}
	switch a.Status {
	case Assigned:
		if turn.SpeakerID == nil || !speakers[*turn.SpeakerID] || a.Method != method || a.Reason != "" || len(a.CandidateSpeakerIDs) != 0 {
			return invalid(field+".attribution", "assigned turns require a global speaker and the engine's overlap method, without unknown metadata")
		}
	case Unknown:
		if turn.SpeakerID != nil || !slices.Contains(reasons, a.Reason) || a.Method != "" {
			return invalid(field+".attribution", "unknown turns require a null speakerId and a known reason, without an assignment method")
		}
	default:
		return invalid(field+".attribution.status", "must be assigned or unknown")
	}
	seen := make(map[string]bool, len(a.CandidateSpeakerIDs))
	for _, id := range a.CandidateSpeakerIDs {
		if !speakers[id] || seen[id] {
			return invalid(field+".attribution.candidateSpeakerIds", "must reference distinct global speakers")
		}
		seen[id] = true
	}
	return nil
}

func interval(start, end, duration int64) bool {
	return 0 <= start && start <= end && end <= duration
}

func text(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != ""
}
