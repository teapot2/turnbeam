package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestOfflineCommandsNeverDispatchInference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meeting.transcript.json")
	doc := &transcript.Transcript{SchemaVersion: 1,
		Recording:    transcript.Recording{Name: "missing.wav", DurationMS: 1000, SHA256: strings.Repeat("a", 64)},
		Pipeline:     transcript.Pipeline{MetaModel: "test", MetaMode: "DIARIZATION", DiarizationModel: "test", DiarizationRevision: "test", RuntimeVersion: "test"},
		Speakers:     []transcript.Speaker{{ID: "opaque-second", Index: 1}, {ID: "opaque-first", Index: 2}},
		Turns:        []transcript.Turn{{ID: "turn", StartMS: 100, EndMS: 500, SpeakerID: new("opaque-second"), Text: "Hello.", Attribution: transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}, Source: transcript.Source{ChunkID: "chunk"}}},
		SourceChunks: []transcript.SourceChunk{{ID: "chunk", EndMS: 1000, MetaSessionID: "session", RawTurns: []transcript.RawTurn{{StartMS: 100, EndMS: 500, Transcript: "Hello."}}}},
	}
	before, err := transcript.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, string, error, int) {
		var stdout, stderr bytes.Buffer
		code := 2
		root := newRootWithTranscriber("test", &code, &stdout, func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error {
			t.Fatal("offline command dispatched inference")
			return nil
		})
		root.SetOut(&stderr)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		return stdout.String(), stderr.String(), err, code
	}
	stdout, stderr, err, _ := invoke("speakers", "rename", path, "--speaker", "2=Bob, Jr.", "--speaker", "1=Alice=A")
	if err != nil || stdout != "" || stderr != "Updated 2 speaker names in the JSON sidecar. Run `turnbeam export` to write a transcript with them.\n" {
		t.Fatal("rename failed or emitted transcript data", err, stdout, stderr)
	}
	data, _ := os.ReadFile(path)
	got, err := transcript.Decode(data)
	doc.Speakers[0].Name, doc.Speakers[1].Name = new("Alice=A"), new("Bob, Jr.")
	if err != nil || !reflect.DeepEqual(got, doc) {
		t.Fatal("mapping did not preserve global IDs and timing", err)
	}
	stdout, stderr, err, _ = invoke("export", path, "--format", "text", "--timestamps", "range", "--quiet", "--verbose")
	if err != nil || stderr != "" || stdout != "[00:00:00.100 - 00:00:00.500] Alice=A: Hello.\n" {
		t.Fatal("offline export mixed output", err, stdout, stderr)
	}
	if again, _, err, _ := invoke("export", path, "-o", "-", "--format", "text", "--timestamps", "range"); err != nil || again != stdout {
		t.Fatal("-o - did not export to stdout", err, again)
	}
	output := filepath.Join(dir, "meeting.md")
	stdout, stderr, err, _ = invoke("export", path, "--output", output, "--timestamps", "none")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatal("file export failed", err)
	}
	if data, _ := os.ReadFile(output); string(data) != "**Alice=A:** Hello.\n" {
		t.Fatal("wrong human export")
	}
	for _, args := range [][]string{
		{"export", path, "--output", output},
		{"speakers", "rename", path, "--speaker", "1=Alice", "--speaker", "3=Unknown"},
	} {
		stdout, _, err, code := invoke(args...)
		if err == nil || code != 1 || stdout != "" {
			t.Fatal("conflict or unknown-index failure was hidden", err)
		}
		current, _ := os.ReadFile(path)
		if !bytes.Equal(current, data) {
			t.Fatal("failed command modified the source")
		}
	}
}
