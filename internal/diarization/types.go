// Package diarization produces one speaker timeline for a complete recording.
package diarization

import "time"

// MaxSpeakers is the pinned Nemotron model's speaker limit, not an accuracy claim.
const MaxSpeakers = 8

// SpeakerLimitReached warns that additional participants may have been merged.
const SpeakerLimitReached = "speaker_limit_reached"

// Speaker maps a recording-local native label to a deterministic canonical ID.
// It is a model hypothesis, never an automatically established real-world identity.
type Speaker struct {
	ID    string
	Index int
	RawID string
}

// Segment preserves original-recording time, including pauses and overlaps.
// Durations retain sub-millisecond precision until canonical transcript assembly.
type Segment struct {
	Start, End time.Duration
	SpeakerID  string
}

// EndAdjustment records a bounded native-frame overrun clipped for analysis only.
// Line refers to the unchanged raw RTTM, including any blank/comment lines.
type EndAdjustment struct {
	Line        int
	RawEnd, End time.Duration
}

// Timeline is sorted chronologically; the underlying raw RTTM order is not changed.
type Timeline struct {
	Duration    time.Duration
	Speakers    []Speaker
	Segments    []Segment
	Adjustments []EndAdjustment
	Warnings    []string
}
