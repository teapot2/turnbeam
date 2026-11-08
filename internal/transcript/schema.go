// Package transcript defines the canonical, versioned meeting transcript.
package transcript

// SchemaVersion is the only canonical schema supported by this implementation.
const SchemaVersion = 1

// AttributionStatus says whether a turn has a recording-wide speaker.
type AttributionStatus string

const (
	Assigned AttributionStatus = "assigned"
	Unknown  AttributionStatus = "unknown"
)

// AttributionMethod is the evidence behind an Assigned turn.
type AttributionMethod string

// TimelineOverlap uses the global diarization timeline.
const TimelineOverlap AttributionMethod = "timeline_overlap"

// AttributionReason explains why a turn is Unknown.
type AttributionReason string

const (
	AmbiguousOverlapOrBoundary AttributionReason = "ambiguous_overlap_or_boundary"
	// InsufficientTiming marks text whose source has no positive time interval.
	InsufficientTiming AttributionReason = "insufficient_timing"
	// NotAligned marks assembled text awaiting recording-wide speaker analysis.
	NotAligned AttributionReason = "not_aligned"
)

var reasons = []AttributionReason{AmbiguousOverlapOrBoundary, InsufficientTiming, NotAligned}

// Transcript is a completed sidecar, not an in-progress checkpoint.
type Transcript struct {
	SchemaVersion int           `json:"schemaVersion"`
	Recording     Recording     `json:"recording"`
	Pipeline      Pipeline      `json:"pipeline"`
	Speakers      []Speaker     `json:"speakers"`
	Turns         []Turn        `json:"turns"`
	SourceChunks  []SourceChunk `json:"sourceChunks"`
}

// Recording identifies the original input without storing its filesystem path.
type Recording struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"durationMs"`
	SHA256     string `json:"sha256"`
}

// Pipeline records inference provenance, independently of display settings.
type Pipeline struct {
	MetaModel           string `json:"metaModel"`
	MetaMode            string `json:"metaMode"`
	DiarizationModel    string `json:"diarizationModel"`
	DiarizationRevision string `json:"diarizationRevision"`
	RuntimeVersion      string `json:"runtimeVersion"`
}

// Speaker has a stable recording-wide ID and a separately editable display name.
type Speaker struct {
	ID    string  `json:"id"`
	Index int     `json:"index"`
	Name  *string `json:"name"`
}

// Turn uses original-recording milliseconds, never display-formatted timestamps.
// A zero-duration text turn retains its source timing and must remain Unknown.
type Turn struct {
	ID          string      `json:"id"`
	StartMS     int64       `json:"startMs"`
	EndMS       int64       `json:"endMs"`
	Text        string      `json:"text"`
	SpeakerID   *string     `json:"speakerId"`
	Attribution Attribution `json:"attribution"`
	Source      Source      `json:"source"`
}

// Attribution separates global identity evidence from request-local Meta labels.
type Attribution struct {
	Status              AttributionStatus `json:"status"`
	Method              AttributionMethod `json:"method,omitempty"`
	Reason              AttributionReason `json:"reason,omitempty"`
	CandidateSpeakerIDs []string          `json:"candidateSpeakerIds,omitempty"`
}

// Source identifies an original Meta turn within one request, including turn 0.
type Source struct {
	ChunkID    string `json:"chunkId"`
	MetaTurnID int64  `json:"metaTurnId"`
}

// SourceChunk preserves request-local provenance and its original time offset.
// RawTurns retain API order even when canonical Turns are chronologically sorted.
type SourceChunk struct {
	ID            string    `json:"id"`
	StartMS       int64     `json:"startMs"`
	EndMS         int64     `json:"endMs"`
	MetaSessionID string    `json:"metaSessionId"`
	RawTurns      []RawTurn `json:"rawTurns"`
}

// RawTurn is an allowlist of Meta's known turn fields, in chunk-local milliseconds.
// Missing and null speaker labels both mean unavailable; neither is a global ID.
// Empty transcripts and zero-duration intervals are retained as observed from Meta.
// There is deliberately no arbitrary metadata, headers, credentials or audio field.
type RawTurn struct {
	TurnID     int64   `json:"turnId"`
	StartMS    int64   `json:"startMs"`
	EndMS      int64   `json:"endMs"`
	Transcript string  `json:"transcript"`
	Speaker    *string `json:"speaker,omitempty"`
}
