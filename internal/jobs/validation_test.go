package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/teapot2/turnbeam/internal/chunking"
	"github.com/teapot2/turnbeam/internal/meta"
)

func TestResumeValidatesManifestSemantics(t *testing.T) {
	cases := map[string]func(*manifest){
		"version":     func(m *manifest) { m.SchemaVersion++ },
		"missing map": func(m *manifest) { m.Completed = nil },
		"missing media": func(m *manifest) {
			m.Media = nil
		},
		"invalid clock":    func(m *manifest) { m.Media.Samples = 0 },
		"missing rttm":     func(m *manifest) { m.RTTM = nil },
		"missing plan":     func(m *manifest) { m.Plan = nil },
		"plan hash":        func(m *manifest) { m.PlanSHA256 = digest([]byte("wrong")) },
		"plan gap":         func(m *manifest) { (*m.Plan)[1].StartSample++ },
		"unexpected work":  func(m *manifest) { m.Completed["unknown"] = m.Completed["chunk_0001"] },
		"false completion": func(m *manifest) { m.Complete = true },
		"audio checksum": func(m *manifest) {
			v := m.Completed["chunk_0001"]
			v.AudioSHA256 = ""
			m.Completed["chunk_0001"] = v
		},
		"attempt count": func(m *manifest) {
			v := m.Completed["chunk_0001"]
			v.Attempts = 0
			m.Completed["chunk_0001"] = v
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			j := f.open(t, false)
			f.seed(t, j)
			if err := j.SaveResult(context.Background(), response(f.plan[0])); err != nil {
				t.Fatal(err)
			}
			damage(&j.m)
			// Recompute the envelope to exercise semantic validation independently.
			payload, err := json.Marshal(j.m)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(envelope{j.m, digest(payload)})
			if err != nil {
				t.Fatal(err)
			}
			if err := j.root.WriteFile("manifest.json", data, 0600); err != nil {
				t.Fatal(err)
			}
			j.Close()
			if restored, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir, Resume: true}); restored != nil || !errors.Is(err, ErrCorrupt) {
				t.Fatalf("invalid manifest reused: %v", err)
			}
		})
	}
}

func TestRTTMBoundaryAndInvalidArtifacts(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	ctx := context.Background()
	if err := j.CheckMedia(ctx, f.recording); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"INCOMPLETE\n", "SPEAKER recording 1 2.900 0.111 <NA> <NA> native_a <NA> <NA>\n"} {
		if err := os.WriteFile(f.rttm.RTTMPath, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := j.SaveDiarization(ctx, f.rttm); err == nil || j.m.RTTM != nil {
			t.Fatal("invalid RTTM committed")
		}
	}
	raw := []byte("SPEAKER recording 1 2.900 0.110 <NA> <NA> native_a <NA> <NA>\n")
	if err := os.WriteFile(f.rttm.RTTMPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := j.SaveDiarization(ctx, f.rttm); err != nil {
		t.Fatal(err)
	}
	stored, err := j.readArtifact(ctx, "rttm", *j.m.RTTM, maxRTTM)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatal("raw RTTM was modified")
	}
	timeline, ok, err := j.Diarization(ctx)
	if err != nil || !ok || len(timeline.Adjustments) != 1 {
		t.Fatalf("clipping diagnostic lost: %v", err)
	}
	j.Close()
	j = f.open(t, true)
	restored, _, err := j.Diarization(ctx)
	if err != nil || !reflect.DeepEqual(restored, timeline) {
		t.Fatal("resume changed timing/diagnostics")
	}
}

func TestStateGuardsAndIdempotence(t *testing.T) {
	f := newFixture(t)
	j := f.open(t, false)
	ctx := context.Background()
	if err := j.SavePlan(ctx, f.plan); !errors.Is(err, ErrState) {
		t.Fatal("plan saved before valid global analysis")
	}
	if err := j.Finish(ctx); !errors.Is(err, ErrState) {
		t.Fatal("empty generation looked complete")
	}
	f.seed(t, j)
	f.seed(t, j)
	plan := append([]chunking.Chunk(nil), f.plan...)
	plan[0].ID = "changed"
	if err := j.SavePlan(ctx, plan); !errors.Is(err, ErrMismatch) {
		t.Fatal("frozen plan changed")
	}
	r := response(f.plan[0])
	for range 2 {
		if err := j.SaveResult(ctx, r); err != nil {
			t.Fatal("identical repeated completion failed", err)
		}
	}
	r.Attempts++
	if err := j.SaveResult(ctx, r); !errors.Is(err, ErrMismatch) {
		t.Fatal("different completion replaced committed response")
	}
	j.Close()
	if err := j.Close(); err != nil {
		t.Fatal("close was not idempotent")
	}
	if _, err := j.Transcribe(ctx, meta.Pool{}, nil, ""); !errors.Is(err, ErrState) {
		t.Fatal("closed job reused")
	}
}

func TestRecoveryRejectsUnsafeDirectoriesAndChangedOpenInput(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(f.dir, 0755); err != nil {
		t.Fatal(err)
	}
	if j, err := Open(context.Background(), f.input, f.settings, Options{Dir: f.dir}); j != nil || !errors.Is(err, ErrCorrupt) {
		t.Fatal("public cache directory accepted")
	}
	if err := os.Chmod(f.dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cache-link")
	if err := os.Symlink(f.dir, link); err != nil {
		t.Fatal(err)
	}
	if j, err := Open(context.Background(), f.input, f.settings, Options{Dir: link}); j != nil || !errors.Is(err, ErrCorrupt) {
		t.Fatal("symlink cache root accepted")
	}
	j := f.open(t, false)
	if err := os.WriteFile(f.input, []byte("changed after opening"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := j.CheckMedia(context.Background(), f.recording); !errors.Is(err, ErrMismatch) {
		t.Fatal("modified original accepted before inference")
	}
}
