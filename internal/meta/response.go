package meta

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/teapot2/turnbeam/internal/transcript"
)

// Response keeps only known fields. Turns retain API order, clip-local timing and
// optional request-local labels; AudioDurationMS never replaces the WAV clock.
type Response struct {
	SessionID       string               `json:"sessionId"`
	Transcript      string               `json:"transcript"`
	AudioDurationMS int64                `json:"audioDurationMs"`
	Turns           []transcript.RawTurn `json:"turns"`
}

// DecodeResponse validates persisted or captured API data against the WAV clock.
// Unknown extensions are accepted but never included in the returned allowlist.
func DecodeResponse(data []byte, durationMS int64) (*Response, error) {
	if len(data) > maxResponseBytes || durationMS < 0 {
		return nil, ErrResponse
	}
	return decodeResponse(data, durationMS)
}

func decodeResponse(data []byte, durationMS int64) (*Response, error) {
	if !utf8.Valid(data) {
		return nil, ErrResponse
	}
	o, err := object(data)
	if err != nil {
		return nil, err
	}
	if raw, exists := o["error"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrResponse
	}
	session, err := field[string](o, "sessionId")
	if err != nil || strings.TrimSpace(session) == "" {
		return nil, ErrResponse
	}
	text, err := field[string](o, "transcript")
	if err != nil {
		return nil, err
	}
	duration, err := field[int64](o, "audioDurationMs")
	if err != nil || duration < 0 {
		return nil, ErrResponse
	}
	items, err := field[[]json.RawMessage](o, "turns")
	if err != nil || len(items) == 0 && strings.TrimSpace(text) != "" {
		return nil, ErrResponse
	}
	r := &Response{SessionID: session, Transcript: text, AudioDurationMS: duration, Turns: []transcript.RawTurn{}}
	ids := make(map[int64]bool, len(items))
	for _, item := range items {
		turn, err := object(item)
		if err != nil {
			return nil, err
		}
		id, e1 := field[int64](turn, "turnId")
		start, e2 := field[int64](turn, "startMs")
		end, e3 := field[int64](turn, "endMs")
		text, e4 := field[string](turn, "transcript")
		var speaker *string
		if raw, ok := turn["speaker"]; ok {
			if err := json.Unmarshal(raw, &speaker); err != nil {
				return nil, ErrResponse
			}
		}
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || id < 0 || ids[id] || start < 0 || end < start || end > durationMS {
			return nil, ErrResponse
		}
		ids[id] = true
		r.Turns = append(r.Turns, transcript.RawTurn{TurnID: id, StartMS: start, EndMS: end, Transcript: text, Speaker: speaker})
	}
	return r, nil
}

// Known object levels reject ambiguous duplicate keys; unknown extensions are
// accepted and discarded, not persisted or interpreted as a schema extension.
func object(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, ErrResponse
	}
	result := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, ErrResponse
		}
		if _, exists := result[name]; exists {
			return nil, ErrResponse
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, ErrResponse
		}
		result[name] = raw
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, ErrResponse
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrResponse
	}
	return result, nil
}

func field[T any](o map[string]json.RawMessage, key string) (value T, err error) {
	raw, exists := o[key]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return value, ErrResponse
	}
	return value, nil
}

func (r *Response) hasSecret(key string) bool {
	if strings.Contains(r.SessionID, key) || strings.Contains(r.Transcript, key) {
		return true
	}
	for _, t := range r.Turns {
		if strings.Contains(t.Transcript, key) || t.Speaker != nil && strings.Contains(*t.Speaker, key) {
			return true
		}
	}
	return false
}

func (e *Error) readTaxonomy(data []byte, key string) {
	o, err := object(data)
	if err != nil {
		return
	}
	safe := func(o map[string]json.RawMessage, name string) string {
		v, err := field[string](o, name)
		if err != nil || !token(v, 128) || strings.Contains(v, key) {
			return ""
		}
		for _, c := range v {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
				return ""
			}
		}
		return v
	}
	e.SessionID = safe(o, "sessionId")
	if fields, err := object(o["error"]); err == nil {
		e.Type, e.Code, e.Param = safe(fields, "type"), safe(fields, "code"), safe(fields, "param")
	}
}
