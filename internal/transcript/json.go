package transcript

import (
	"encoding/json"
	"fmt"

	"github.com/teapot2/turnbeam/internal/jsonstrict"
)

// Encode validates and serializes a sidecar without altering its data or order.
func Encode(t *Transcript) ([]byte, error) {
	if err := Validate(t); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode transcript: %w", err)
	}
	return data, nil
}

// Decode accepts exactly one schema-v1 or schema-v2 object. Unknown fields, duplicate keys,
// missing required fields and invalid data are rejected, never silently discarded.
func Decode(data []byte) (*Transcript, error) {
	if err := jsonstrict.Validate(data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var header struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("%w: decode version: %w", ErrInvalid, err)
	}
	if header.SchemaVersion == nil {
		return nil, invalid("schemaVersion", "is required")
	}
	if *header.SchemaVersion != 1 && *header.SchemaVersion != SchemaVersion {
		return nil, versionError(*header.SchemaVersion)
	}
	var t Transcript
	if err := jsonstrict.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := Validate(&t); err != nil {
		return nil, err
	}
	if err := omittedWhenEmpty(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// omittedWhenEmpty rejects the engine-specific keys written with empty values.
// Encode omits them, so accepting them would silently drop them on rename or
// export instead of round-tripping the sidecar unchanged.
func omittedWhenEmpty(data []byte, t *Transcript) error {
	var raw struct {
		Pipeline map[string]json.RawMessage   `json:"pipeline"`
		Turns    []map[string]json.RawMessage `json:"turns"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	for _, key := range []string{"engine", "metaModel", "metaMode", "asrModel", "asrRevision"} {
		var value string
		if field, ok := raw.Pipeline[key]; ok && json.Unmarshal(field, &value) == nil && value == "" {
			return invalid("pipeline."+key, "must be omitted rather than empty")
		}
	}
	for i, turn := range raw.Turns {
		if _, ok := turn["source"]; ok && t.Turns[i].Source == (Source{}) {
			return invalid(fmt.Sprintf("turns[%d].source", i), "must be omitted rather than empty")
		}
	}
	return nil
}
