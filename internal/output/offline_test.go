package output

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/filelock"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func offlineSample() *transcript.Transcript {
	doc := sample()
	doc.Speakers = []transcript.Speaker{{ID: "global-b", Index: 1}, {ID: "global-a", Index: 2}}
	doc.Turns[0].SpeakerID = &doc.Speakers[0].ID
	doc.Turns[0].Attribution = transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}
	second := doc.Turns[0]
	second.ID, second.StartMS, second.EndMS, second.Text = "second", 600, 800, "Yes."
	second.Source.MetaTurnID, second.SpeakerID = 1, &doc.Speakers[1].ID
	unknown := second
	unknown.ID, unknown.StartMS, unknown.EndMS, unknown.Text = "unknown", 900, 900, "Maybe."
	unknown.Source.MetaTurnID, unknown.SpeakerID = 2, nil
	unknown.Attribution = transcript.Attribution{Status: transcript.Unknown, Reason: transcript.InsufficientTiming, CandidateSpeakerIDs: []string{"global-b", "global-a"}}
	doc.Turns = append(doc.Turns, second, unknown)
	doc.SourceChunks[0].RawTurns = append(doc.SourceChunks[0].RawTurns,
		transcript.RawTurn{TurnID: 1, StartMS: 600, EndMS: 800, Transcript: "Yes."},
		transcript.RawTurn{TurnID: 2, StartMS: 900, EndMS: 900, Transcript: "Maybe."})
	return doc
}

func sourceFile(t *testing.T) (string, []byte) {
	t.Helper()
	data, err := transcript.Encode(offlineSample())
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the exact source bytes on export, not just equivalent JSON.
	data = append([]byte(" \n"), data...)
	path := filepath.Join(t.TempDir(), "meeting.transcript.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestOfflineRenameAndExportPreserveCanonicalData(t *testing.T) {
	ctx := context.Background()
	path, _ := sourceFile(t)
	human := filepath.Join(filepath.Dir(path), "meeting.md")
	old := []byte("existing human export")
	os.WriteFile(human, old, 0600)
	names := map[int]string{2: "Bob, Jr.=B", 1: "Jos\u00e9 **A**"}
	if err := Rename(ctx, path, names); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := transcript.Decode(data)
	want, _ := transcript.Rename(ctx, offlineSample(), names)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("rename changed more than display names", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("replacement was not private", err)
	}
	if data, err := os.ReadFile(human); err != nil || !bytes.Equal(data, old) {
		t.Fatal("rename modified a human export", err)
	}
	for _, format := range transcript.Formats {
		for _, timestamps := range transcript.TimestampStyles {
			opts := transcript.RenderOptions{Format: format, Timestamps: timestamps}
			var stdout bytes.Buffer
			if err := Export(ctx, path, "", opts, &stdout); err != nil {
				t.Fatal(err)
			}
			rendered, _ := transcript.Render(ctx, want, opts)
			if !bytes.Equal(stdout.Bytes(), rendered) {
				t.Fatal("offline render changed canonical data/presentation")
			}
		}
	}
	files(t, filepath.Dir(path), 2)
}

func TestExportReusesOnlyItsExactSourceSidecar(t *testing.T) {
	ctx := context.Background()
	path, original := sourceFile(t)
	dir := filepath.Dir(path)
	before, _ := os.Stat(path)
	var stdout bytes.Buffer
	output := filepath.Join(dir, "meeting.md")
	if err := Export(ctx, path, output, transcript.RenderOptions{Timestamps: "none"}, &stdout); err != nil || stdout.Len() != 0 {
		t.Fatal("could not reuse input sidecar", err)
	}
	after, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if !unchanged(before, after) || !bytes.Equal(data, original) {
		t.Fatal("export rewrote the source")
	}
	if err := Export(ctx, path, output, transcript.RenderOptions{}, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("existing human export overwritten", err)
	}
	if err := Export(ctx, path, path, transcript.RenderOptions{Format: "json"}, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("source JSON was overwritten", err)
	}
	copyPath := filepath.Join(dir, "copy.txt")
	if err := Export(ctx, path, copyPath, transcript.RenderOptions{Format: "text"}, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "copy.transcript.json"))
	if got, err := transcript.Decode(data); err != nil || !reflect.DeepEqual(got, offlineSample()) {
		t.Fatal("new basename lacks canonical sidecar", err)
	}
	for _, kind := range []string{"same-data", "hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			other := filepath.Join(t.TempDir(), "other.transcript.json")
			switch kind {
			case "hardlink":
				if err := os.Link(path, other); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				os.Symlink(path, other)
			default:
				os.WriteFile(other, original, 0600)
			}
			if err := Export(ctx, path, strings.TrimSuffix(other, ".transcript.json")+".md", transcript.RenderOptions{}, nil); !errors.Is(err, ErrConflict) {
				t.Fatal("unrelated existing sidecar accepted", err)
			}
			files(t, filepath.Dir(other), 1)
		})
	}
	files(t, dir, 4)
}

func TestOfflineRejectsInvalidInputWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"missing", "directory", "symlink", "oversize", "malformed", "future"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.json")
			switch kind {
			case "directory":
				os.Mkdir(path, 0700)
			case "symlink":
				os.Symlink("missing", path)
			case "oversize":
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				f.Truncate(maxTranscriptBytes + 1)
				f.Close()
			case "future":
				os.WriteFile(path, []byte(`{"schemaVersion":3}`), 0600)
			case "malformed":
				os.WriteFile(path, []byte(`{"schemaVersion":1,"schemaVersion":1}`), 0600)
			}
			before, _ := os.ReadDir(filepath.Dir(path))
			var stdout bytes.Buffer
			output := filepath.Join(filepath.Dir(path), "out.md")
			for _, err := range []error{Export(ctx, path, output, transcript.RenderOptions{}, &stdout), Rename(ctx, path, map[int]string{1: "Alice"})} {
				if err == nil || stdout.Len() != 0 {
					t.Fatal("invalid input generated output")
				}
				if kind == "future" && !errors.Is(err, transcript.ErrUnsupportedVersion) {
					t.Fatal("version error lost", err)
				}
			}
			files(t, filepath.Dir(path), len(before))
		})
	}
	path, before := sourceFile(t)
	if err := Rename(ctx, path, map[int]string{1: "Alice", 3: "Bob"}); !errors.Is(err, transcript.ErrSpeakerMapping) {
		t.Fatal("unknown index accepted", err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, before) {
		t.Fatal("invalid rename partially committed")
	}
}

func TestRenameStorageFailuresAndConcurrentEdits(t *testing.T) {
	for _, kind := range []string{"pre-sync", "replace", "post-sync", "cancel", "external-edit"} {
		t.Run(kind, func(t *testing.T) {
			path, before := sourceFile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := openSource(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			want := errors.New("storage unavailable")
			if kind == "replace" {
				s.commit = func(string, string) error { return want }
			}
			calls := 0
			sync := s.sync
			s.sync = func() error {
				calls++
				if kind == "pre-sync" || kind == "post-sync" && calls == 2 {
					return want
				}
				if kind == "cancel" {
					cancel()
				}
				if kind == "external-edit" {
					os.WriteFile(path, []byte("an external edit"), 0600)
				}
				return sync()
			}
			updated, _ := transcript.Rename(ctx, s.transcript, map[int]string{1: "Alice"})
			err = s.save(ctx, updated)
			if kind == "cancel" {
				want = context.Canceled
			} else if kind == "external-edit" {
				want = ErrSourceChanged
			}
			if !errors.Is(err, want) {
				t.Fatal("storage/cancellation error lost", err)
			}
			data, _ := os.ReadFile(path)
			switch kind {
			case "post-sync":
				if !errors.Is(err, ErrCommit) {
					t.Fatal("post-commit uncertainty not explicit")
				}
				got, e := transcript.Decode(data)
				if e != nil || !reflect.DeepEqual(got, updated) {
					t.Fatal("post-commit error left partial JSON", e)
				}
			case "external-edit":
				if string(data) != "an external edit" {
					t.Fatal("external edit overwritten")
				}
			default:
				if !bytes.Equal(data, before) {
					t.Fatal("pre-commit failure changed the original")
				}
			}
			files(t, filepath.Dir(path), 1)
		})
	}
}

func TestSourceLockAndFingerprint(t *testing.T) {
	path, before := sourceFile(t)
	s, err := openSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := openSource(context.Background(), path); other != nil || !errors.Is(err, filelock.ErrBusy) || errors.Is(err, ErrSource) {
		t.Fatal("overlapping edit/export not excluded", err)
	}
	publishLockWait = 50 * time.Millisecond
	defer func() { publishLockWait = 5 * time.Second }()
	d := open(t, filepath.Join(filepath.Dir(path), "new.md"), transcript.RenderOptions{}, nil)
	if err := d.Write(context.Background(), sample()); !errors.Is(err, filelock.ErrBusy) || !errors.Is(err, ErrPublishLocked) {
		t.Fatal("publication did not coordinate with an offline edit", err)
	}
	d.Close()
	// Hash checking detects same-size in-place edits even with restored mtime.
	os.WriteFile(path, bytes.Replace(before, []byte("Hello."), []byte("Hallo."), 1), 0644)
	os.Chtimes(path, s.info.ModTime(), s.info.ModTime())
	if err := s.check(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("fingerprint did not detect content edit", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if next, err := openSource(context.Background(), path); err != nil {
		t.Fatal("directory lock leaked", err)
	} else {
		next.close()
	}
}

func TestReusedSidecarIsNeverRolledBack(t *testing.T) {
	for _, mode := range []string{"link", "sync", "cancel", "changed"} {
		t.Run(mode, func(t *testing.T) {
			path, original := sourceFile(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := openSource(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			d, err := openDestination(ctx, filepath.Join(filepath.Dir(path), "meeting.md"), transcript.RenderOptions{}, nil, s)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			link := d.link
			want := errors.New("storage failure")
			d.link = func(from, to string) error {
				if mode == "link" {
					return want
				}
				err := link(from, to)
				if mode == "cancel" {
					cancel()
				}
				if mode == "changed" {
					os.WriteFile(path, []byte("external change"), 0600)
				}
				return err
			}
			if mode == "sync" {
				d.sync = func() error { return want }
			} else if mode == "cancel" {
				want = context.Canceled
			} else if mode == "changed" {
				want = ErrSourceChanged
			}
			if err := d.Write(ctx, s.transcript); !errors.Is(err, want) {
				t.Fatal("failure not preserved", err)
			}
			d.Close()
			data, _ := os.ReadFile(path)
			if mode == "changed" {
				original = []byte("external change")
			}
			if !bytes.Equal(data, original) {
				t.Fatal("rollback touched reused sidecar")
			}
			files(t, filepath.Dir(path), 1)
		})
	}
}

func TestRenameInterruptionKeepsCompleteJSON(t *testing.T) {
	if path := os.Getenv("TURNBEAM_RENAME_CHILD"); path != "" {
		s, err := openSource(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		commit := s.commit
		s.commit = func(from, to string) error {
			if os.Getenv("TURNBEAM_RENAME_COMMIT") == "after" {
				if err := commit(from, to); err != nil {
					return err
				}
			}
			os.Exit(7)
			return nil
		}
		updated, _ := transcript.Rename(context.Background(), s.transcript, map[int]string{1: "Alice"})
		s.save(context.Background(), updated)
		os.Exit(8)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			path, before := sourceFile(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestRenameInterruptionKeepsCompleteJSON$")
			cmd.Env = append(os.Environ(), "TURNBEAM_RENAME_CHILD="+path, "TURNBEAM_RENAME_COMMIT="+phase)
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
				t.Fatal("child did not interrupt at commit", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := transcript.Decode(data)
			if err != nil || phase == "before" && !bytes.Equal(before, data) || phase == "after" && (got.Speakers[0].Name == nil || *got.Speakers[0].Name != "Alice") {
				t.Fatal("interruption left partial or unexpected canonical JSON", err)
			}
			if err := Rename(ctx, path, map[int]string{1: "Next"}); err != nil {
				t.Fatal("interrupted process leaked its lock", err)
			}
		})
	}
}

func TestPublicationWaitsForBriefOfflineEdit(t *testing.T) {
	path, _ := sourceFile(t)
	s, err := openSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		released <- s.close()
	}()
	d := open(t, filepath.Join(filepath.Dir(path), "after-edit.md"), transcript.RenderOptions{}, nil)
	if err := d.Write(context.Background(), sample()); err != nil {
		t.Fatal("publication failed while an offline edit briefly held the lock", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}
