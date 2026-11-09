package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"
)

func TestTranscribeJobLifecycle(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	input := filepath.Join(dir, "meeting.wav")
	writeFile(t, input, []byte("riff"))
	var got pipeline.Options
	c := serve(t, newServer(dir, publishing(func(o pipeline.Options) { got = o })))

	res := c.call("transcribe", map[string]any{"path": input, "model": "parakeet"}, "tok")
	var view jobView
	decode(t, res.StructuredContent, &view)
	if view.State != stateDone || view.JobID == "" || view.Model != "parakeet" || view.Recording != filepath.Join(real, "meeting.wav") ||
		view.Output != filepath.Join(real, "meeting.md") || view.Transcript != filepath.Join(real, "meeting.transcript.json") || view.Error != "" || !strings.Contains(view.Hint, "read_transcript") {
		t.Fatalf("transcribe: %+v", view)
	}
	if view.Summary == nil || *view.Summary != (jobSummary{Turns: 3, Speakers: 2, UnknownTurns: 1, DurationSeconds: 10}) {
		t.Fatalf("summary: %+v", view.Summary)
	}
	if got.Model != "parakeet" || got.Output != view.Output || got.Saved == nil || got.Render != (transcript.RenderOptions{Format: transcript.FormatMarkdown, Timestamps: transcript.TimestampsStart}) || !got.Progress.Quiet || got.Progress.Observer == nil || got.Resume {
		t.Fatalf("pipeline options: %+v", got)
	}
	for _, path := range []string{view.Output, view.Transcript} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(res.Content[0].Text, `"state": "done"`) {
		t.Fatalf("text content must mirror the result: %s", res.Content[0].Text)
	}

	var progressed int
	last := -1
	for _, note := range c.drain() {
		var params struct {
			ProgressToken string  `json:"progressToken"`
			Progress      int     `json:"progress"`
			Message       string  `json:"message"`
			Total         *string `json:"total"`
		}
		if note.Method != "notifications/progress" || json.Unmarshal(note.Params, &params) != nil || params.ProgressToken != "tok" || params.Progress <= last {
			t.Fatalf("bad progress notification: %s", note.Params)
		}
		last = params.Progress
		if strings.Contains(params.Message, "3/3 chunks") {
			progressed++
		}
	}
	if last < 0 || progressed == 0 {
		t.Fatal("progress notifications missing or without chunk counts")
	}

	decode(t, c.call("job_status", map[string]any{"job_id": view.JobID}, nil).StructuredContent, &view)
	if view.State != stateDone {
		t.Fatalf("job_status: %+v", view)
	}
	if msg := c.failure("job_status", map[string]any{"job_id": "nope"}); !strings.Contains(msg, "unknown job_id") {
		t.Fatal(msg)
	}
	// A re-run keeps the existing transcript and writes the next free name.
	decode(t, c.call("transcribe", map[string]any{"path": input}, nil).StructuredContent, &view)
	if view.State != stateDone || view.Output != filepath.Join(real, "meeting-2.md") || view.Transcript != filepath.Join(real, "meeting-2.transcript.json") || got.Output != filepath.Join(real, "meeting.md") {
		t.Fatalf("re-run: %+v (pipeline output %s)", view, got.Output)
	}
	for _, path := range []string{view.Output, view.Transcript, filepath.Join(dir, "meeting.md"), filepath.Join(dir, "meeting.transcript.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if msg := c.failure("transcribe", map[string]any{"path": input, "output": filepath.Join(dir, "other.transcript.json"), "format": "markdown"}); !strings.Contains(msg, "format json") {
		t.Fatal(msg)
	}
}

func TestTranscribeRejectsBadArgumentsBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "meeting.wav")
	writeFile(t, input, []byte("riff"))
	calls := 0
	c := serve(t, newServer(dir, publishing(func(pipeline.Options) { calls++ })))
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": "meeting.wav"}, "absolute"},
		{map[string]any{"path": filepath.Join(t.TempDir(), "x.wav")}, "outside"},
		{map[string]any{"path": filepath.Join(dir, "missing.wav")}, "does not exist"},
		{map[string]any{"path": dir}, "not a regular file"},
		{map[string]any{"path": input, "model": "whisper"}, "model must be one of"},
		{map[string]any{"path": input, "model": "parakeet", "resume": true}, "resume applies only"},
		{map[string]any{"path": input, "wait_seconds": 999}, "wait_seconds"},
		{map[string]any{"path": input, "format": "pdf"}, "format must be one of"},
		{map[string]any{"path": input, "output": filepath.Join(dir, "missing", "out.md")}, "directory does not exist"},
		{map[string]any{"path": input, "output": filepath.Join(t.TempDir(), "out.md")}, "outside"},
		{map[string]any{"path": input, "outpt": "x"}, "unknown field"},
		{map[string]any{"path": 5}, "path"},
	} {
		if msg := c.failure("transcribe", tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%v: %q does not mention %q", tc.args, msg, tc.want)
		}
	}
	if msg := c.failure("job_status", map[string]any{}); !strings.Contains(msg, "unknown job_id") {
		t.Error(msg)
	}
	if calls != 0 {
		t.Fatal("invalid arguments reached the pipeline")
	}
}

func TestTranscribeFailuresAreDescribedWithRetryHints(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.wav"), []byte("riff"))
	writeFile(t, filepath.Join(dir, "b.wav"), []byte("riff"))
	s := newServer(dir, func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error {
		return errors.New("boom: /secret/path")
	})
	s.DefaultModel = "meta"
	s.Describe = func(err error) string { return "classified failure" }
	c := serve(t, s)
	var view jobView
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "a.wav")}, nil).StructuredContent, &view)
	if view.State != stateFailed || view.Model != "meta" || view.Error != "classified failure" || !strings.Contains(view.Hint, "resume=true") || strings.Contains(view.Hint, "start over") {
		t.Fatalf("meta failure: %+v", view)
	}
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "b.wav"), "model": "nemotron-3.5"}, nil).StructuredContent, &view)
	if view.State != stateFailed || !strings.Contains(view.Hint, "start over") {
		t.Fatalf("local failure: %+v", view)
	}
}

func TestJobsRunOneAtATime(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.wav", "b.wav"} {
		writeFile(t, filepath.Join(dir, name), []byte("riff"))
	}
	release := make(chan struct{})
	started := make(chan string, 2)
	c := serve(t, newServer(dir, gated(release, started)))
	var first, second jobView
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "a.wav"), "wait_seconds": 0}, nil).StructuredContent, &first)
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("first job never started")
	}
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "b.wav"), "wait_seconds": 1}, nil).StructuredContent, &second)
	if second.State != stateQueued || len(started) != 0 {
		t.Fatalf("second job must wait for the first: %+v", second)
	}
	if msg := c.failure("transcribe", map[string]any{"path": filepath.Join(dir, "b.wav")}); !strings.Contains(msg, "already transcribing") {
		t.Fatal(msg)
	}
	close(release)
	for _, id := range []string{first.JobID, second.JobID} {
		var view jobView
		decode(t, c.call("job_status", map[string]any{"job_id": id, "wait_seconds": 10}, nil).StructuredContent, &view)
		if view.State != stateDone {
			t.Fatalf("job %s: %+v", id, view)
		}
	}
}

func TestTranscribeKeepsExistingTranscripts(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	writeFile(t, filepath.Join(dir, "meeting.wav"), []byte("riff"))
	writeFile(t, filepath.Join(dir, "meeting.md"), []byte("keep"))
	release := make(chan struct{})
	started := make(chan string, 1)
	c := serve(t, newServer(dir, gated(release, started)))
	var view jobView
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "meeting.wav"), "wait_seconds": 0}, nil).StructuredContent, &view)
	if view.Output != filepath.Join(real, "meeting-2.md") || view.Transcript != filepath.Join(real, "meeting-2.transcript.json") ||
		!strings.Contains(view.Hint, "already exists") || !strings.Contains(view.Hint, "read_transcript") {
		t.Fatalf("taken name not moved or not explained: %+v", view)
	}
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("job never started")
	}
	// Another writer takes the predicted name while the job runs.
	writeFile(t, filepath.Join(dir, "meeting-2.md"), []byte("other"))
	close(release)
	decode(t, c.call("job_status", map[string]any{"job_id": view.JobID, "wait_seconds": 10}, nil).StructuredContent, &view)
	if view.State != stateDone || view.Output != filepath.Join(real, "meeting-3.md") || view.Transcript != filepath.Join(real, "meeting-3.transcript.json") || view.Summary == nil {
		t.Fatalf("job did not report the name it published: %+v", view)
	}
	for name, want := range map[string]string{"meeting.md": "keep", "meeting-2.md": "other"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != want {
			t.Fatal("existing file changed", name, err)
		}
	}
}

func TestListRecordings(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	now := time.Now()
	files := map[string]time.Time{
		"a.mp4": now.Add(-time.Hour), "b.m4a": now.Add(-72 * time.Hour), "notes.txt": now, ".hidden.mp4": now,
		filepath.Join("sub", "c.wav"): now.Add(-2 * time.Hour), filepath.Join("sub", "d1", "d2", "d3", "deep.mp3"): now,
		filepath.Join(".git", "e.mp4"): now,
	}
	for name, mod := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, []byte("x"))
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "a.mp4"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, dir, "a")
	s := newServer(dir, nil)
	s.Probe = func(_ context.Context, path string) (time.Duration, error) {
		switch filepath.Base(path) {
		case "a.mp4":
			return 61*time.Second + 540*time.Millisecond, nil
		case "b.m4a":
			return 0, media.ErrNoAudio
		}
		return 0, errors.New("ffprobe failed")
	}
	c := serve(t, s)

	list := func(args map[string]any) listing {
		t.Helper()
		var out listing
		decode(t, c.call("list_recordings", args, nil).StructuredContent, &out)
		return out
	}
	names := func(out listing) []string {
		var names []string
		for _, r := range out.Recordings {
			names = append(names, r.Name)
		}
		return names
	}
	out := list(map[string]any{})
	if len(out.Directories) != 1 || out.Directories[0] != real || strings.Join(names(out), ",") != "a.mp4,c.wav,b.m4a" || out.Truncated || out.Partial {
		t.Fatalf("default listing: %+v", out)
	}
	a, c2, b := out.Recordings[0], out.Recordings[1], out.Recordings[2]
	if a.Path != filepath.Join(real, "a.mp4") || a.DurationSeconds != 61.5 || a.Transcript != filepath.Join(real, "a.transcript.json") || a.SizeBytes != 1 || a.Modified == "" {
		t.Fatalf("a: %+v", a)
	}
	if b.Note != "no audio stream" || b.DurationSeconds != 0 || c2.Note != "duration unavailable" || c2.Transcript != "" {
		t.Fatalf("notes: %+v %+v", b, c2)
	}
	if got := names(list(map[string]any{"since": now.Add(-48 * time.Hour).Format("2006-01-02")})); strings.Join(got, ",") != "a.mp4,c.wav" {
		t.Fatalf("since: %v", got)
	}
	if got := names(list(map[string]any{"until": now.Add(-24 * time.Hour).Format("2006-01-02")})); strings.Join(got, ",") != "b.m4a" {
		t.Fatalf("until: %v", got)
	}
	if got := names(list(map[string]any{"since": now.Add(-90 * time.Minute).Format(time.RFC3339)})); strings.Join(got, ",") != "a.mp4" {
		t.Fatalf("since RFC 3339: %v", got)
	}
	if out := list(map[string]any{"limit": 1}); strings.Join(names(out), ",") != "a.mp4" || !out.Truncated {
		t.Fatalf("limit: %+v", out)
	}
	if got := names(list(map[string]any{"recursive": false})); strings.Join(got, ",") != "a.mp4,b.m4a" {
		t.Fatalf("recursive=false: %v", got)
	}
	if got := names(list(map[string]any{"directory": filepath.Join(dir, "sub")})); strings.Join(got, ",") != "deep.mp3,c.wav" {
		t.Fatalf("directory: %v", got)
	}
	for args, want := range map[*map[string]any]string{
		{"directory": t.TempDir()}:                     "outside",
		{"directory": filepath.Join(dir, "a.mp4")}:     "not a folder",
		{"since": "yesterday"}:                         "since",
		{"since": "2030-01-01", "until": "2020-01-01"}: "until must not be before",
		{"limit": 500}:                                 "limit",
	} {
		if msg := c.failure("list_recordings", *args); !strings.Contains(msg, want) {
			t.Errorf("%v: %q", *args, msg)
		}
	}
}

func TestReadTranscript(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	sidecar := writeSidecar(t, dir, "meeting")
	c := serve(t, newServer(dir, nil))

	res := c.call("read_transcript", map[string]any{"path": filepath.Join(dir, "meeting.wav")}, nil)
	var info transcriptInfo
	decode(t, res.StructuredContent, &info)
	text := res.Content[0].Text
	if !strings.Contains(text, "[00:00:00.100] **Speaker 1:** Hello.") || !strings.Contains(text, "**Unknown speaker:** Who said that?") {
		t.Fatalf("markdown: %q", text)
	}
	if info.Transcript != filepath.Join(real, "meeting.transcript.json") || info.Recording != "meeting.wav" || info.DurationSeconds != 10 || info.Engine != "meta" || info.Model != "test" ||
		len(info.Speakers) != 2 || info.Speakers[1] != (speakerInfo{Index: 2, Name: "Speaker 2"}) || info.Turns != 3 || info.UnknownTurns != 1 || info.Format != "markdown" || info.Range != nil {
		t.Fatalf("info: %+v", info)
	}

	res = c.call("read_transcript", map[string]any{"path": sidecar, "format": "text", "timestamps": "none", "start_seconds": 2, "end_seconds": 5.5}, nil)
	decode(t, res.StructuredContent, &info)
	if res.Content[0].Text != "Speaker 2: Hi there.\n" || info.Turns != 1 || info.UnknownTurns != 0 || info.Range == nil || *info.Range.StartSeconds != 2 || *info.Range.EndSeconds != 5.5 {
		t.Fatalf("window: %q %+v", res.Content[0].Text, info)
	}
	res = c.call("read_transcript", map[string]any{"path": sidecar, "format": "json", "start_seconds": 5.5}, nil)
	if doc, err := transcript.Decode([]byte(res.Content[0].Text)); err != nil || len(doc.Turns) != 1 || doc.Turns[0].ID != "t3" {
		t.Fatalf("json window: %v %s", err, res.Content[0].Text)
	}
	for args, want := range map[*map[string]any]string{
		{"path": filepath.Join(dir, "other.wav")}:                 "no transcript for",
		{"path": sidecar, "start_seconds": 5, "end_seconds": 5}:   "end_seconds must be greater",
		{"path": sidecar, "start_seconds": -1}:                    "nonnegative",
		{"path": sidecar, "format": "docx"}:                       "format must be one of",
		{"path": filepath.Join(t.TempDir(), "x.transcript.json")}: "outside",
	} {
		if msg := c.failure("read_transcript", *args); !strings.Contains(msg, want) {
			t.Errorf("%v: %q", *args, msg)
		}
	}
	writeFile(t, filepath.Join(dir, "broken.transcript.json"), []byte(`{"schemaVersion": 7}`))
	if msg := c.failure("read_transcript", map[string]any{"path": filepath.Join(dir, "broken.transcript.json")}); !strings.Contains(msg, "unsupported transcript schema version") {
		t.Fatal(msg)
	}
}

func TestRenameAndExport(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	sidecar := writeSidecar(t, dir, "meeting")
	c := serve(t, newServer(dir, nil))

	res := c.call("rename_speakers", map[string]any{"path": filepath.Join(dir, "meeting.mp4"), "names": map[string]string{"1": "Alice", " 2": "Bob"}}, nil)
	var renamed struct {
		Transcript string
		Renamed    int
		Speakers   []speakerInfo
		Hint       string
	}
	decode(t, res.StructuredContent, &renamed)
	if renamed.Transcript != filepath.Join(real, "meeting.transcript.json") || renamed.Renamed != 2 || len(renamed.Speakers) != 2 || renamed.Speakers[0].Name != "Alice" || renamed.Speakers[1].Name != "Bob" || renamed.Hint == "" {
		t.Fatalf("rename: %+v", renamed)
	}
	if text := c.call("read_transcript", map[string]any{"path": sidecar}, nil).Content[0].Text; !strings.Contains(text, "**Alice:** Hello.") || !strings.Contains(text, "**Bob:** Hi there.") {
		t.Fatalf("names not applied: %q", text)
	}
	for args, want := range map[*map[string]any]string{
		{"path": sidecar, "names": map[string]string{}}:         "at least one",
		{"path": sidecar, "names": map[string]string{"x": "A"}}: "positive speaker indices",
		{"path": sidecar, "names": map[string]string{"1": " "}}: "nonempty names",
		{"path": sidecar, "names": map[string]string{"9": "Z"}}: "speaker mapping",
	} {
		if msg := c.failure("rename_speakers", *args); !strings.Contains(msg, want) {
			t.Errorf("%v: %q", *args, msg)
		}
	}

	out := filepath.Join(dir, "meeting.srt")
	res = c.call("export", map[string]any{"path": sidecar, "output": out}, nil)
	var exported struct{ Transcript, Output, Format string }
	decode(t, res.StructuredContent, &exported)
	data, err := os.ReadFile(out)
	if err != nil || exported.Output != filepath.Join(real, "meeting.srt") || exported.Format != "srt" || !strings.HasPrefix(string(data), "1\n00:00:00,100 --> 00:00:02,000\nAlice: Hello.") {
		t.Fatalf("export: %+v %v %q", exported, err, data)
	}
	if msg := c.failure("export", map[string]any{"path": sidecar, "output": out}); !strings.Contains(msg, "already exists") {
		t.Fatal(msg)
	}
	for args, want := range map[*map[string]any]string{
		{"path": sidecar, "output": ""}:                                                "output must name a file",
		{"path": sidecar, "output": dir + "/"}:                                         "output must name a file",
		{"path": sidecar, "output": filepath.Join(dir, "x.md"), "timestamps": "never"}: "timestamps one of",
		{"path": sidecar, "output": filepath.Join(t.TempDir(), "x.md")}:                "outside",
	} {
		if msg := c.failure("export", *args); !strings.Contains(msg, want) {
			t.Errorf("%v: %q", *args, msg)
		}
	}
}

type fakeInstallation struct {
	checks   []setup.Check
	err      error
	statuses []setup.ModelStatus
}

func (f fakeInstallation) Doctor(context.Context) ([]setup.Check, error) { return f.checks, f.err }
func (f fakeInstallation) LocalStatus(context.Context) ([]setup.ModelStatus, error) {
	return f.statuses, nil
}

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	real := realPath(t, dir)
	var statuses []setup.ModelStatus
	for _, model := range setup.LocalModels() {
		statuses = append(statuses, setup.ModelStatus{Model: model, Installed: model.Name == "parakeet"})
	}
	s := newServer(dir, nil)
	s.Installation = fakeInstallation{checks: []setup.Check{{Name: "Platform", OK: true, Message: "ok"}, {Name: "Local model", OK: true, LocalModel: "parakeet"}}, statuses: statuses}
	s.KeyStatus = func(context.Context) (credentials.Source, bool, error) { return "", false, nil }
	c := serve(t, s)
	var view statusView
	decode(t, c.call("status", map[string]any{}, nil).StructuredContent, &view)
	if !view.Ready || view.DefaultModel != "parakeet" || len(view.Roots) != 1 || view.Roots[0] != real || len(view.Jobs) != 0 || len(view.Checks) != 3 {
		t.Fatalf("status: %+v", view)
	}
	if key := view.Checks[2]; key.Name != "Meta API key" || !key.OK || !strings.Contains(key.Message, "only model meta") {
		t.Fatalf("key check: %+v", key)
	}
	if len(view.Models) != 3 || view.Models[0].Name != "meta" || view.Models[0].Ready || !strings.Contains(view.Models[0].Status, "needs a key") || view.Models[0].Default ||
		view.Models[1].Name != "parakeet" || !view.Models[1].Ready || !view.Models[1].Default || view.Models[1].SizeMB != 714 ||
		view.Models[2].Name != "nemotron-3.5" || view.Models[2].Ready || !strings.Contains(view.Models[2].Status, "not installed") {
		t.Fatalf("models: %+v", view.Models)
	}
	if msg := c.failure("status", map[string]any{"verbose": true}); !strings.Contains(msg, "unknown field") {
		t.Fatal(msg)
	}

	s = newServer(dir, nil)
	s.Installation = fakeInstallation{checks: []setup.Check{{Name: "FFmpeg", Message: "not found", Fix: "install it"}}, err: setup.ErrDoctorFailed, statuses: statuses}
	s.KeyStatus = func(context.Context) (credentials.Source, bool, error) { return credentials.Environment, true, nil }
	c = serve(t, s)
	decode(t, c.call("status", map[string]any{}, nil).StructuredContent, &view)
	if view.Ready || view.Checks[0].Fix != "install it" || !view.Models[0].Ready || view.Checks[1].Message != "from the META_API_KEY environment variable" {
		t.Fatalf("failed doctor: %+v", view)
	}

	s = newServer(dir, nil)
	s.Installation = fakeInstallation{err: errors.New("native runtime crashed")}
	c = serve(t, s)
	if msg := c.failure("status", map[string]any{}); !strings.Contains(msg, "native runtime crashed") {
		t.Fatal(msg)
	}
}
