package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teapot2/turnbeam/internal/pipeline"
)

func TestMCPCommandServesStdio(t *testing.T) {
	dir := t.TempDir()
	code := 2
	var stdout, stderr bytes.Buffer
	root := newRootWithTranscriber("test", &code, &stdout, func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error {
		t.Fatal("starting the server dispatched inference")
		return nil
	})
	root.SetIn(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":"two","method":"ping"}` + "\n"))
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs([]string{"mcp", "--root", dir})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout must carry exactly the two responses: %q", stdout.String())
	}
	// Requests are answered concurrently, so match responses by id.
	byID := map[string]map[string]any{}
	for _, line := range lines {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Result map[string]any  `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Result == nil {
			t.Fatalf("line %q (%v)", line, err)
		}
		byID[string(msg.ID)] = msg.Result
	}
	info, _ := byID["1"]["serverInfo"].(map[string]any)
	if info["name"] != "turnbeam" || info["version"] != "test" || byID[`"two"`] == nil {
		t.Fatalf("responses: %v", byID)
	}
	if !strings.Contains(stderr.String(), "serving over stdio") || strings.Contains(stderr.String(), "{") {
		t.Fatalf("stderr: %q", stderr.String())
	}
}

func TestMCPCommandRejectsBadFlags(t *testing.T) {
	dir := t.TempDir()
	if code, stderr := invoke(context.Background(), "mcp", "--root", filepath.Join(dir, "missing")); code != exitUsage || !strings.Contains(stderr, "--root must be an existing directory") {
		t.Fatalf("missing root: code=%d, stderr=%q", code, stderr)
	}
	if code, stderr := invoke(context.Background(), "mcp", "--install-dir", ""); code != exitUsage || !strings.Contains(stderr, "--install-dir") {
		t.Fatalf("empty install dir: code=%d, stderr=%q", code, stderr)
	}
	t.Setenv(modelEnv, "whisper")
	if code, stderr := invoke(context.Background(), "mcp", "--root", dir); code != exitUsage || !strings.Contains(stderr, modelEnv+" must be one of") {
		t.Fatalf("bad default model: code=%d, stderr=%q", code, stderr)
	}
}
