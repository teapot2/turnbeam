package output

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/transcript"
)

func sample() *transcript.Transcript {
	return &transcript.Transcript{SchemaVersion: 1, Recording: transcript.Recording{Name: "public.wav", DurationMS: 1000, SHA256: strings.Repeat("a", 64)}, Pipeline: transcript.Pipeline{MetaModel: "test", MetaMode: "DIARIZATION", DiarizationModel: "test", DiarizationRevision: "test", RuntimeVersion: "test"}, Speakers: []transcript.Speaker{}, Turns: []transcript.Turn{{ID: "turn", StartMS: 100, EndMS: 500, Text: "Hello.", Attribution: transcript.Attribution{Status: transcript.Unknown, Reason: transcript.AmbiguousOverlapOrBoundary}, Source: transcript.Source{ChunkID: "chunk", MetaTurnID: 0}}}, SourceChunks: []transcript.SourceChunk{{ID: "chunk", EndMS: 1000, MetaSessionID: "session", RawTurns: []transcript.RawTurn{{TurnID: 0, StartMS: 100, EndMS: 500, Transcript: "Hello."}}}}}
}
func open(t *testing.T, path string, opts transcript.RenderOptions, out io.Writer) *Destination {
	t.Helper()
	d, err := Open(context.Background(), path, opts, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func files(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != want {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
}

func TestStdoutAndPairedFiles(t *testing.T) {
	ctx := context.Background()
	for _, format := range transcript.Formats {
		t.Run(string(format), func(t *testing.T) {
			opts := transcript.RenderOptions{Format: format, Timestamps: "none"}
			want, _ := transcript.Render(ctx, sample(), opts)
			var stdout bytes.Buffer
			d := open(t, "", opts, &stdout)
			if err := d.Write(ctx, sample()); err != nil || !bytes.Equal(stdout.Bytes(), want) {
				t.Fatal("stdout was not exactly requested output", err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "meeting.md")
			d = open(t, path, opts, &stdout)
			st, err := d.root.Stat(d.stage)
			if err != nil || st.Mode().Perm() != 0700 {
				t.Fatal("staging is not private")
			}
			stdout.Reset()
			if err := d.Write(ctx, sample()); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Fatal("file output leaked into stdout")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, want) {
				t.Fatal("rendered file changed")
			}
			canonical, err := os.ReadFile(filepath.Join(dir, "meeting.transcript.json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := transcript.Decode(canonical)
			if err != nil || !reflect.DeepEqual(got, sample()) {
				t.Fatal("sidecar lost canonical timing/provenance", err)
			}
			for _, name := range []string{path, filepath.Join(dir, "meeting.transcript.json")} {
				st, err := os.Stat(name)
				if err != nil || st.Mode().Perm() != 0600 {
					t.Fatal("output is not private")
				}
			}
			files(t, dir, 2)
		})
	}
}

func TestNamesNumberingAndSingleCanonicalJSON(t *testing.T) {
	for name, numbered := range map[string]string{"meeting": "meeting-2", "meeting.md": "meeting-2.md", "meeting.txt": "meeting-2.txt", "meeting.json": "meeting-2.json",
		".meeting": ".meeting-2", "meeting.v1.md": "meeting.v1-2.md", "meeting.transcript.json": "meeting-2.transcript.json", "Meeting.Transcript.JSON": "Meeting-2.Transcript.JSON"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			d := open(t, path, transcript.RenderOptions{Format: "json"}, nil)
			if err := d.Write(context.Background(), sample()); err != nil {
				t.Fatal(err)
			}
			d.Close()
			count := 2
			if strings.HasSuffix(strings.ToLower(name), ".transcript.json") {
				count = 1
			}
			files(t, dir, count)
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d = open(t, path, transcript.RenderOptions{Format: "json"}, nil)
			if d.Path() != filepath.Join(dir, numbered) || d.Sidecar() != filepath.Join(dir, sidecarName(numbered)) || d.Conflict() != path {
				t.Fatal("taken name not numbered", d.Path(), d.Sidecar())
			}
			if err := d.Write(context.Background(), sample()); err != nil {
				t.Fatal(err)
			}
			d.Close()
			files(t, dir, 2*count)
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, first) {
				t.Fatal("existing output changed", err)
			}
		})
	}
	for _, kind := range []string{"main", "sidecar", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := "meeting.transcript.json"
			if kind == "main" {
				name = "meeting.md"
			}
			path := filepath.Join(dir, name)
			switch kind {
			case "directory":
				os.Mkdir(path, 0700)
			case "symlink":
				os.Symlink("missing-target", path)
			default:
				os.WriteFile(path, []byte("keep"), 0600)
			}
			d := open(t, filepath.Join(dir, "meeting.md"), transcript.RenderOptions{}, nil)
			if d.Path() != filepath.Join(dir, "meeting-2.md") || d.Conflict() != path {
				t.Fatal("conflict not moved to a numbered name", d.Path(), d.Conflict())
			}
			if err := d.Write(context.Background(), sample()); err != nil {
				t.Fatal(err)
			}
			d.Close()
			files(t, dir, 3)
			if kind == "main" || kind == "sidecar" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
					t.Fatal("conflicting file changed", err)
				}
			}
		})
	}
	for _, path := range []string{" ", filepath.Join(t.TempDir(), "missing", "out.md"), filepath.Join(t.TempDir(), "out.transcript.json")} {
		if d, err := Open(context.Background(), path, transcript.RenderOptions{}, nil); err == nil {
			d.Close()
			t.Fatal("invalid path accepted")
		}
	}
}

func TestPublicationOrderRollbackAndCancellation(t *testing.T) {
	for _, mode := range []string{"stage-main", "stage-sidecar", "link-sidecar", "link-main", "sync-sidecar", "sync-main", "cancel-after-sidecar"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			d := open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("simulated storage failure")
			write, link, sync := d.stageFile, d.link, d.sync
			d.stageFile = func(ctx context.Context, name string, data []byte) (os.FileInfo, error) {
				if mode == "stage-"+name {
					return nil, want
				}
				return write(ctx, name, data)
			}
			var order []string
			d.link = func(from, to string) error {
				order = append(order, to)
				if to == d.main {
					if _, err := os.Stat(filepath.Join(dir, d.sidecar)); err != nil {
						t.Error("output appeared before complete sidecar")
					}
				}
				if mode == "link-sidecar" && to == d.sidecar || mode == "link-main" && to == d.main {
					return want
				}
				err := link(from, to)
				if mode == "cancel-after-sidecar" && to == d.sidecar {
					cancel()
				}
				return err
			}
			calls := 0
			d.sync = func() error {
				calls++
				if mode == "sync-sidecar" && calls == 1 || mode == "sync-main" && calls == 2 {
					return want
				}
				return sync()
			}
			err := d.Write(ctx, sample())
			if mode == "cancel-after-sidecar" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("write error lost", err)
			}
			if len(order) > 1 && order[0] != d.sidecar {
				t.Fatal("wrong commit order")
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			files(t, dir, 0)
		})
	}
}

func TestRacingDestinationAndRollbackFailurePreserveOthers(t *testing.T) {
	dir := t.TempDir()
	d := open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
	link := d.link
	raced := false
	d.link = func(from, to string) error {
		if to == d.main && !raced {
			raced = true
			if err := os.WriteFile(filepath.Join(dir, to), []byte("other writer"), 0600); err != nil {
				return err
			}
		}
		return link(from, to)
	}
	if err := d.Write(context.Background(), sample()); err != nil {
		t.Fatal("racing name was not moved", err)
	}
	if d.Path() != filepath.Join(dir, "out-2.md") || d.Sidecar() != filepath.Join(dir, "out-2.transcript.json") {
		t.Fatal("published under the wrong name", d.Path())
	}
	d.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "out.md"))
	if string(data) != "other writer" {
		t.Fatal("racing writer data lost")
	}
	if _, err := os.Stat(filepath.Join(dir, "out.transcript.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sidecar of the abandoned name was left behind", err)
	}
	files(t, dir, 3)
	otherDir := t.TempDir()
	d = open(t, filepath.Join(otherDir, "out.md"), transcript.RenderOptions{}, nil)
	want := errors.New("storage failure")
	link = d.link
	d.link = func(from, to string) error {
		if to == d.main {
			return want
		}
		return link(from, to)
	}
	d.remove = func(string) error { return os.ErrPermission }
	if err := d.Write(context.Background(), sample()); !errors.Is(err, ErrCleanup) || !errors.Is(err, want) || !errors.Is(err, os.ErrPermission) {
		t.Fatal("incomplete rollback was not explicit", err)
	}
	d.Close()
	if _, err := os.Stat(filepath.Join(otherDir, "out.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete pair looked committed")
	}
	if data, err := os.ReadFile(filepath.Join(otherDir, "out.transcript.json")); err != nil {
		t.Fatal(err)
	} else if _, err := transcript.Decode(data); err != nil {
		t.Fatal("orphan sidecar is incomplete")
	}
}

func TestConcurrentWritersGetDistinctNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.md")
	a, b := open(t, path, transcript.RenderOptions{}, nil), open(t, path, transcript.RenderOptions{}, nil)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, d := range []*Destination{a, b} {
		wg.Go(func() { errs <- d.Write(context.Background(), sample()); d.Close() })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := []string{a.Path(), b.Path()}
	slices.Sort(got)
	if !slices.Equal(got, []string{filepath.Join(dir, "out-2.md"), path}) {
		t.Fatal("writers did not get distinct names", got)
	}
	files(t, dir, 4)
}

func TestNumberedNamesAreBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.md")
	os.WriteFile(path, nil, 0600)
	os.WriteFile(filepath.Join(dir, "out-2.transcript.json"), nil, 0600)
	if got, err := Available(path); err != nil || got != filepath.Join(dir, "out-3.md") {
		t.Fatal("available name skipped a taken sidecar", got, err)
	}
	if got, err := Available(filepath.Join(dir, "free.md")); err != nil || got != filepath.Join(dir, "free.md") {
		t.Fatal("free name was numbered", got, err)
	}
	for n := 3; n <= maxNumbered; n++ {
		os.WriteFile(filepath.Join(dir, numberedName("out.md", n)), nil, 0600)
	}
	if _, err := Available(path); !errors.Is(err, ErrNoFreeName) {
		t.Fatal("exhausted names accepted", err)
	}
	if d, err := Open(context.Background(), path, transcript.RenderOptions{}, nil); d != nil || !errors.Is(err, ErrNoFreeName) {
		t.Fatal("exhausted names opened", err)
	}
	files(t, dir, maxNumbered)
}

func TestRollbackNeverRemovesReplacedSidecar(t *testing.T) {
	dir := t.TempDir()
	d := open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
	link := d.link
	want := errors.New("failed main publication")
	d.link = func(from, to string) error {
		if to != d.main {
			return link(from, to)
		}
		name := filepath.Join(dir, d.sidecar)
		if err := os.Remove(name); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte("other writer's replacement"), 0600); err != nil {
			return err
		}
		return want
	}
	if err := d.Write(context.Background(), sample()); !errors.Is(err, want) || !errors.Is(err, ErrCleanup) {
		t.Fatal("replacement conflict was hidden", err)
	}
	d.Close()
	data, err := os.ReadFile(filepath.Join(dir, "out.transcript.json"))
	if err != nil || string(data) != "other writer's replacement" {
		t.Fatal("rollback deleted another writer's file")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }
func TestBadInputStdoutFailureAndClosedState(t *testing.T) {
	d := open(t, "", transcript.RenderOptions{}, shortWriter{})
	if err := d.Write(context.Background(), sample()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short stdout write ignored", err)
	}
	dir := t.TempDir()
	d = open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
	bad := sample()
	bad.Turns[0].Source.MetaTurnID = 99
	if err := d.Write(context.Background(), bad); !errors.Is(err, transcript.ErrInvalid) {
		t.Fatal("bad transcript written")
	}
	d.Close()
	files(t, dir, 0)
	if err := d.Write(context.Background(), sample()); !errors.Is(err, ErrState) {
		t.Fatal("closed output accepted work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d, err := Open(ctx, "", transcript.RenderOptions{}, io.Discard); d != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled output opened")
	}
}

func TestProcessInterruptionLeavesNoPrimaryOutput(t *testing.T) {
	if dir := os.Getenv("TURNBEAM_OUTPUT_CHILD"); dir != "" {
		d := open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
		link := d.link
		d.link = func(from, to string) error {
			if to == d.main {
				os.Exit(7)
			}
			return link(from, to)
		}
		d.Write(context.Background(), sample())
		os.Exit(8)
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestProcessInterruptionLeavesNoPrimaryOutput$")
	cmd.Env = append(os.Environ(), "TURNBEAM_OUTPUT_CHILD="+dir)
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatal("child did not interrupt at the commit boundary", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted output looked complete")
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Decode(data); err != nil {
		t.Fatal("orphan was a partial JSON file")
	}
	d := open(t, filepath.Join(dir, "out.md"), transcript.RenderOptions{}, nil)
	if d.Path() != filepath.Join(dir, "out-2.md") {
		t.Fatal("orphan silently replaced", d.Path())
	}
}
