package transcript

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrSpeakerMapping identifies invalid or unknown display-index mappings.
var ErrSpeakerMapping = errors.New("invalid speaker mapping; use distinct existing indices and nonempty INDEX=NAME values")

// ParseSpeakerNames accepts repeatable INDEX=NAME values without splitting names
// on commas or additional equals signs. Errors never echo display names.
func ParseSpeakerNames(values []string) (map[int]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: at least one --speaker INDEX=NAME is required", ErrSpeakerMapping)
	}
	names := make(map[int]string, len(values))
	for _, value := range values {
		index, name, ok := strings.Cut(value, "=")
		id, err := strconv.Atoi(index)
		if !ok || err != nil || id < 1 || !text(name) {
			return nil, fmt.Errorf("%w: --speaker must use a positive INDEX=NAME with valid text", ErrSpeakerMapping)
		}
		if _, exists := names[id]; exists {
			return nil, fmt.Errorf("%w: --speaker repeats index %d", ErrSpeakerMapping, id)
		}
		names[id] = name
	}
	return names, nil
}

// Rename changes display names only. It copies the speaker slice; all other
// canonical data is shared read-only with input, without changing IDs or timing.
func Rename(ctx context.Context, input *Transcript, names map[int]string) (*Transcript, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Validate(input); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, ErrSpeakerMapping
	}
	for index, name := range names {
		if index < 1 || index > len(input.Speakers) || !text(name) {
			return nil, fmt.Errorf("%w: --speaker index must exist and name must be nonempty text", ErrSpeakerMapping)
		}
	}
	result := *input
	result.Speakers = append([]Speaker{}, input.Speakers...)
	for i := range result.Speakers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name, ok := names[result.Speakers[i].Index]; ok {
			result.Speakers[i].Name = &name
		}
	}
	return &result, nil
}
