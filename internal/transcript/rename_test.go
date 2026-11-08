package transcript

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRenameChangesOnlyDisplayNames(t *testing.T) {
	input := example()
	before, _ := Encode(input)
	names, err := ParseSpeakerNames([]string{"2=Bob, Jr.=B", "1=Jos\u00e9\n**A**"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Rename(context.Background(), input, names)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := Encode(input)
	if !bytes.Equal(before, after) {
		t.Fatal("rename mutated its input")
	}
	input.Speakers[0].Name, input.Speakers[1].Name = ptr(names[1]), ptr(names[2])
	if !reflect.DeepEqual(got, input) {
		t.Fatal("rename changed IDs, ordering, attribution, timing or raw provenance")
	}
	// Duplicate display names do not merge global identities.
	got, err = Rename(context.Background(), got, map[int]string{1: "Same", 2: "Same"})
	if err != nil || got.Speakers[0].ID == got.Speakers[1].ID {
		t.Fatal("display names affected identity", err)
	}
}

func TestInvalidSpeakerMappings(t *testing.T) {
	for _, values := range [][]string{
		nil, {"Alice"}, {"0=Alice"}, {"-1=Alice"}, {"x=Alice"}, {"1="}, {"1= \t"},
		{"1=Alice", "01=Bob"}, {"999999999999999999999999999=Alice"}, {"1=" + string([]byte{0xff})},
	} {
		if got, err := ParseSpeakerNames(values); got != nil || !errors.Is(err, ErrSpeakerMapping) || strings.Contains(err.Error(), "Alice") {
			t.Fatal("bad mapping accepted or name leaked", err)
		}
	}
	input := example()
	before, _ := Encode(input)
	for _, names := range []map[int]string{nil, {0: "Alice"}, {1: "Alice", 3: "Bob"}, {1: " "}, {1: string([]byte{0xff})}} {
		if got, err := Rename(context.Background(), input, names); got != nil || !errors.Is(err, ErrSpeakerMapping) {
			t.Fatal("invalid rename returned a partial result", err)
		}
		after, _ := Encode(input)
		if !bytes.Equal(before, after) {
			t.Fatal("failed rename changed canonical data")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := Rename(ctx, input, map[int]string{1: "Alice"}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled rename returned data", err)
	}
	if _, err := Rename(context.Background(), nil, map[int]string{1: "Alice"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid transcript accepted", err)
	}
}
