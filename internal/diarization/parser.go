package diarization

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// EndTolerance is one native diarization frame; larger overruns are invalid.
const EndTolerance = 10 * time.Millisecond

// ParserVersion identifies timing, label mapping and boundary-adjustment rules.
const ParserVersion = "rttm-v1"

const maxRTTMBytes = 64 << 20

// ErrRTTM identifies malformed or out-of-bounds native output.
var ErrRTTM = errors.New("invalid RTTM")

type rawSegment struct {
	start, end time.Duration
	speaker    string
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ParseRTTM reads the pinned runtime's ten-column, single-recording, mono SPEAKER
// records. It retains exact fixed-point times, allows real overlaps and empty
// silence output, and never echoes native records in errors. First-appearance
// ties use lexical native-label order, independent of input/map iteration order.
func ParseRTTM(ctx context.Context, input io.Reader, recordingID string, duration time.Duration) (Timeline, error) {
	if err := ctx.Err(); err != nil {
		return Timeline{}, err
	}
	if duration <= 0 || !token(recordingID) {
		return Timeline{}, fmt.Errorf("%w: positive duration and recording ID required", ErrRTTM)
	}
	timeline := Timeline{Duration: duration, Speakers: []Speaker{}, Segments: []Segment{}, Adjustments: []EndAdjustment{}, Warnings: []string{}}
	limited := &io.LimitedReader{R: contextReader{ctx, input}, N: maxRTTMBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var records []rawSegment
	first := make(map[string]time.Duration)
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return Timeline{}, err
		}
		row := strings.TrimSpace(scanner.Text())
		if row == "" || strings.HasPrefix(row, "#") {
			continue
		}
		fail := func(reason string) (Timeline, error) {
			return Timeline{}, fmt.Errorf("%w: line %d: %s", ErrRTTM, line, reason)
		}
		fields := strings.Fields(row)
		if !utf8.ValidString(row) || len(fields) != 10 || fields[0] != "SPEAKER" || fields[1] != recordingID || fields[2] != "1" {
			return fail("expected one matching mono SPEAKER record")
		}
		start, err := seconds(fields[3])
		if err != nil {
			return fail("invalid start time")
		}
		length, err := seconds(fields[4])
		if err != nil || length <= 0 || length > time.Duration(math.MaxInt64)-start {
			return fail("invalid duration or timestamp overflow")
		}
		if !token(fields[7]) || fields[7] == "<NA>" {
			return fail("native speaker label is required")
		}
		end := start + length
		if start >= duration {
			return fail("segment starts outside the recording")
		}
		if end > duration {
			if end-duration > EndTolerance {
				return fail("segment exceeds the recording by more than one native frame")
			}
			timeline.Adjustments = append(timeline.Adjustments, EndAdjustment{Line: line, RawEnd: end, End: duration})
			end = duration
		}
		records = append(records, rawSegment{start: start, end: end, speaker: fields[7]})
		if len(records) > 1_000_000 {
			return fail("too many intervals")
		}
		if previous, exists := first[fields[7]]; !exists || start < previous {
			first[fields[7]] = start
		}
	}
	if err := scanner.Err(); err != nil {
		return Timeline{}, fmt.Errorf("%w: read records: %w", ErrRTTM, err)
	}
	if limited.N == 0 {
		return Timeline{}, fmt.Errorf("%w: output exceeds 64 MiB", ErrRTTM)
	}
	if err := ctx.Err(); err != nil {
		return Timeline{}, err
	}
	labels := make([]string, 0, len(first))
	for label := range first {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		if first[labels[i]] != first[labels[j]] {
			return first[labels[i]] < first[labels[j]]
		}
		return labels[i] < labels[j]
	})
	ids := make(map[string]string, len(labels))
	for i, label := range labels {
		id := fmt.Sprintf("speaker_%04d", i+1)
		ids[label] = id
		timeline.Speakers = append(timeline.Speakers, Speaker{ID: id, Index: i + 1, RawID: label})
	}
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.end != b.end {
			return a.end < b.end
		}
		return a.speaker < b.speaker
	})
	for _, record := range records {
		timeline.Segments = append(timeline.Segments, Segment{Start: record.start, End: record.end, SpeakerID: ids[record.speaker]})
	}
	if len(labels) >= MaxSpeakers {
		timeline.Warnings = append(timeline.Warnings, SpeakerLimitReached)
	}
	if err := ctx.Err(); err != nil {
		return Timeline{}, err
	}
	return timeline, nil
}

func token(value string) bool {
	fields := strings.Fields(value)
	if value == "" || !utf8.ValidString(value) || len(fields) != 1 || fields[0] != value {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == 127 {
			return false
		}
	}
	return true
}

// Parse decimal seconds as integers, avoiding float addition and millisecond drift.
func seconds(value string) (time.Duration, error) {
	whole, fraction, _ := strings.Cut(value, ".")
	if whole == "" || len(value) > 32 || len(fraction) > 9 || strings.HasSuffix(value, ".") {
		return 0, ErrRTTM
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return 0, ErrRTTM
		}
	}
	seconds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || seconds > math.MaxInt64/int64(time.Second) {
		return 0, ErrRTTM
	}
	nanos, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	base := seconds * int64(time.Second)
	if err != nil || nanos > math.MaxInt64-base {
		return 0, ErrRTTM
	}
	return time.Duration(base + nanos), nil
}
