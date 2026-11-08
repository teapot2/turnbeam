package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/diarization"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/meta"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestDefaultDirPreservesRecoveryLocation(t *testing.T) {
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefaultDir()
	if err != nil || got != filepath.Join(base, "turnbeam", "jobs") {
		t.Fatalf("default recovery location changed: %q, %v", got, err)
	}
}

type fixture struct {
	input, dir string
	recording  *media.Prepared
	settings   Settings
	plan       []chunking.Chunk
	rttm       *diarization.Result
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	input := filepath.Join(t.TempDir(), "original.wav")
	normal := filepath.Join(t.TempDir(), "normalized.wav")
	raw := filepath.Join(t.TempDir(), "timeline.rttm")
	for path, data := range map[string]string{input: "original public test bytes", normal: "normalized public test bytes", raw: "SPEAKER recording 1 0.100 0.800 <NA> <NA> native_a <NA> <NA>\nSPEAKER recording 1 2.500 0.500 <NA> <NA> native_a <NA> <NA>\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := DefaultSettings(meta.Diarization)
	plan := []chunking.Chunk{}
	for i := 0; i < 3; i++ {
		boundary := chunking.Pause
		if i == 2 {
			boundary = chunking.End
		}
		plan = append(plan, chunking.Chunk{ID: fmt.Sprintf("chunk_%04d", i+1), StartSample: int64(i) * media.SampleRate, EndSample: int64(i+1) * media.SampleRate, Boundary: boundary})
	}
	return fixture{input: input, dir: filepath.Join(t.TempDir(), "jobs"), settings: s, recording: &media.Prepared{Path: normal, Samples: 3 * media.SampleRate, Duration: 3 * time.Second}, plan: plan, rttm: &diarization.Result{RTTMPath: raw, RuntimeVersion: s.RuntimeVersion, ModelRevision: s.ModelRevision}}
}

func (f fixture) open(t *testing.T, resume bool) *Job {
	t.Helper()
	j, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}
func (f fixture) seed(t *testing.T, j *Job) {
	t.Helper()
	ctx := context.Background()
	if err := j.CheckMedia(ctx, f.recording); err != nil {
		t.Fatal(err)
	}
	if err := j.SaveDiarization(ctx, f.rttm); err != nil {
		t.Fatal(err)
	}
	if err := j.SavePlan(ctx, f.plan); err != nil {
		t.Fatal(err)
	}
}
func response(c chunking.Chunk) meta.Result {
	return meta.Result{Chunk: c, Attempts: 1, AudioSHA256: digest([]byte(c.ID)), Response: &meta.Response{SessionID: "session-" + c.ID, Transcript: "Hello.", AudioDurationMS: 1040, Turns: []transcript.RawTurn{{TurnID: 0, StartMS: 0, EndMS: 1000, Transcript: "Hello.", Speaker: nil}}}}
}

func TestCheckpointLifecycleAndCachedRead(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	for _, c := range f.plan {
		if err := j.SaveResult(context.Background(), response(c)); err != nil {
			t.Fatal(err)
		}
	}
	if rows, complete, err := j.Results(context.Background()); rows != nil || complete || err != nil {
		t.Fatal("unfinished job looked complete")
	}
	if err := j.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldTimeline, ok, err := j.Diarization(context.Background())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = f.open(t, true)
	timeline, ok, err := j.Diarization(context.Background())
	if err != nil || !ok || !reflect.DeepEqual(timeline, oldTimeline) {
		t.Fatal("global diarization state changed")
	}
	rows, err := j.Transcribe(context.Background(), meta.Pool{}, nil, "")
	if err != nil || len(rows) != 3 {
		t.Fatalf("fully cached job required audio/credentials: %v", err)
	}
	for i, r := range rows {
		if r.Chunk != f.plan[i] || r.Response.AudioDurationMS != 1040 || r.Response.SessionID != response(f.plan[i]).Response.SessionID {
			t.Fatal("cached order/metadata changed")
		}
	}
	plan := j.Plan()
	plan[0].ID = "modified"
	if j.Plan()[0].ID != f.plan[0].ID {
		t.Fatal("caller mutated frozen plan")
	}
	if err := filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0700 {
				t.Fatal("cache directory not private")
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal("cache file not private")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), f.input) || strings.Contains(string(data), f.recording.Path) || strings.HasSuffix(path, ".wav") {
			t.Fatal("private path or audio persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRejectsChangedInputSettingsAndMedia(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	j.Close()
	changed := f.settings
	changed.MetaMode = meta.Endpointing
	if j, err := Open(context.Background(), f.input, changed, Options{Dir: f.dir, Resume: true}); j != nil || !errors.Is(err, ErrMismatch) {
		t.Fatal("changed mode reused cache")
	}
	changed = f.settings
	changed.ModelRevision = "different-revision"
	if j, err := Open(context.Background(), f.input, changed, Options{Dir: f.dir, Resume: true}); j != nil || !errors.Is(err, ErrMismatch) {
		t.Fatal("changed model reused cache")
	}
	if err := os.WriteFile(f.input, []byte("different original contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if j, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Resume: true}); j != nil || !errors.Is(err, ErrMismatch) {
		t.Fatal("changed input reused cache")
	}
	if err := os.WriteFile(f.input, []byte("original public test bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	j = f.open(t, true)
	if err := os.WriteFile(f.recording.Path, []byte("different normalization"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := j.CheckMedia(context.Background(), f.recording); !errors.Is(err, ErrMismatch) {
		t.Fatal("different normalized bytes reused cache")
	}
}

func TestNewFreshAndExclusiveSlot(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	old := j.root.Name()
	for _, opts := range []Options{{Dir: f.dir, Resume: true}, {Dir: f.dir, Fresh: true}} {
		if other, err := Open(context.Background(), f.input, f.settings, opts); other != nil || !errors.Is(err, ErrBusy) {
			t.Fatal("active generation bypassed slot lock")
		}
	}
	j.Close()
	for _, fresh := range []bool{false, true} {
		j, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Fresh: fresh})
		if err != nil {
			t.Fatal(err)
		}
		if j.root.Name() == old || j.Plan() != nil {
			t.Fatal("new run implicitly reused results")
		}
		if _, err := os.Stat(filepath.Join(old, "manifest.json")); err != nil {
			t.Fatal("prior generation was deleted")
		}
		j.Close()
	}
	if j, err := Open(context.Background(), "missing", f.settings, Options{Resume: true, Fresh: true}); j != nil || !errors.Is(err, ErrOptions) {
		t.Fatal("conflicting flags reached file IO")
	}
	freshDir := filepath.Join(t.TempDir(), "jobs")
	if j, err := Open(context.Background(), f.input, f.settings, Options{Dir: freshDir, Resume: true}); j != nil || !errors.Is(err, ErrMissing) {
		t.Fatal("missing checkpoint silently started new work")
	}
}

func TestInterruptedManifestCommitPreservesOnlyCommittedWork(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	f.seed(t, j)
	if err := j.SaveResult(context.Background(), response(f.plan[0])); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(j.root.Name(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("simulated interruption before rename")
	j.write = func(_ context.Context, root *os.Root, _ string, _ []byte) error {
		root.WriteFile(".tmp-interrupted", []byte("incomplete"), 0600)
		return want
	}
	if err := j.SaveResult(context.Background(), response(f.plan[1])); !errors.Is(err, want) {
		t.Fatal("failed manifest commit reported success")
	}
	after, err := os.ReadFile(filepath.Join(j.root.Name(), "manifest.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("prior manifest was damaged")
	}
	if err := j.Finish(context.Background()); !errors.Is(err, ErrState) {
		t.Fatal("uncertain session continued")
	}
	j.Close()
	j = f.open(t, true)
	if len(j.m.Completed) != 1 || j.m.Complete {
		t.Fatal("orphan response/temp file looked committed")
	}
}

func TestResumeRejectsCommittedCorruption(t *testing.T) {
	for _, mode := range []string{"response", "rttm", "manifest", "permission", "symlink", "pointer"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			j := f.open(t, false)
			f.seed(t, j)
			if err := j.SaveResult(context.Background(), response(f.plan[0])); err != nil {
				t.Fatal(err)
			}
			gen, slot := j.root.Name(), j.slot.Name()
			ref := j.m.Completed[f.plan[0].ID].Response
			rawRef := *j.m.RTTM
			j.Close()
			switch mode {
			case "response":
				os.WriteFile(filepath.Join(gen, artifactName("response", ref)), []byte("damaged"), 0600)
			case "rttm":
				os.Remove(filepath.Join(gen, artifactName("rttm", rawRef)))
			case "manifest":
				data, _ := os.ReadFile(filepath.Join(gen, "manifest.json"))
				data = []byte(strings.Replace(string(data), `"attempts":1`, `"attempts":2`, 1))
				os.WriteFile(filepath.Join(gen, "manifest.json"), data, 0600)
			case "permission":
				os.Chmod(filepath.Join(gen, "manifest.json"), 0644)
			case "symlink":
				name := filepath.Join(gen, artifactName("response", ref))
				os.Remove(name)
				os.Symlink(f.input, name)
			case "pointer":
				data, _ := json.Marshal(pointer{schemaVersion, "../outside"})
				os.WriteFile(filepath.Join(slot, "current.json"), data, 0600)
			}
			if restored, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Resume: true}); restored != nil || !errors.Is(err, ErrCorrupt) {
				t.Fatalf("damaged committed state reused: %v", err)
			}
			fresh, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Fresh: true})
			if err != nil {
				t.Fatal(err)
			}
			fresh.Close()
		})
	}
}

func TestAtomicWriteCancellationAndReplacement(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("value", []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := atomicWrite(ctx, root, "value", []byte("new")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled commit succeeded")
	}
	if data, _ := root.ReadFile("value"); string(data) != "old" {
		t.Fatal("canceled write damaged prior data")
	}
	if err := atomicWrite(context.Background(), root, "value", []byte("new")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary files remain")
	}
}

func TestNewRunKeepsResumePointerUntilFirstResult(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.open(t, false)
	f.seed(t, first)
	if err := first.SaveResult(ctx, response(f.plan[0])); err != nil {
		t.Fatal(err)
	}
	paid := first.root.Name()
	first.Close()

	// A later normal run that fails before transcribing anything must not hide paid work.
	abandoned := f.open(t, false)
	f.seed(t, abandoned)
	abandoned.Close()
	resumed := f.open(t, true)
	if resumed.root.Name() != paid || len(resumed.m.Completed) != 1 {
		t.Fatal("abandoned run replaced the resumable generation")
	}
	resumed.Close()

	next := f.open(t, false)
	f.seed(t, next)
	if err := next.SaveResult(ctx, response(f.plan[1])); err != nil {
		t.Fatal(err)
	}
	newer := next.root.Name()
	next.Close()
	if resumed := f.open(t, true); resumed.root.Name() != newer {
		t.Fatal("first saved result did not make the new generation resumable")
	}
}
