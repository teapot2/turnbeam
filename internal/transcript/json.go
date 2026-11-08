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

// Decode accepts exactly one schema-v1 object. Unknown fields, duplicate keys,
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
	if *header.SchemaVersion != SchemaVersion {
		return nil, versionError(*header.SchemaVersion)
	}
	var t Transcript
	if err := jsonstrict.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := Validate(&t); err != nil {
		return nil, err
	}
	return &t, nil
}
