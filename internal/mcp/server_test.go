package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestHandshakeAndProtocolErrors(t *testing.T) {
	dir := t.TempDir()
	c := serve(t, newServer(dir, nil))

	init := c.request("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	var hello struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo   struct{ Name, Version string } `json:"serverInfo"`
		Instructions string                         `json:"instructions"`
	}
	if init.Error != nil || json.Unmarshal(init.Result, &hello) != nil || hello.ProtocolVersion != "2025-03-26" || hello.Capabilities.Tools == nil || hello.ServerInfo.Name != "turnbeam" || hello.ServerInfo.Version != "test" || hello.Instructions == "" {
		t.Fatalf("initialize: %s %v", init.Result, init.Error)
	}
	c.notify("notifications/initialized", nil)
	if msg := c.request("initialize", map[string]any{"protocolVersion": "1999-01-01"}); msg.Error != nil || json.Unmarshal(msg.Result, &hello) != nil || hello.ProtocolVersion != latestProtocol {
		t.Fatalf("unknown protocol version must get the latest: %s", msg.Result)
	}
	if msg := c.request("ping", nil); msg.Error != nil || string(msg.Result) != "{}" {
		t.Fatalf("ping: %s %v", msg.Result, msg.Error)
	}

	list := c.request("tools/list", nil)
	var tools struct {
		Tools []struct {
			Name        string
			Description string
			InputSchema map[string]any `json:"inputSchema"`
		}
	}
	if list.Error != nil || json.Unmarshal(list.Result, &tools) != nil {
		t.Fatal("tools/list", list.Error)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" || tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("tool %s lacks a description or strict object schema", tool.Name)
		}
	}
	want := []string{"list_recordings", "transcribe", "job_status", "cancel_job", "read_transcript", "rename_speakers", "export", "status"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	if msg := c.request("resources/list", nil); msg.Error == nil || msg.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method: %+v", msg.Error)
	}
	if msg := c.request("tools/call", map[string]any{"name": "nope"}); msg.Error == nil || msg.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool: %+v", msg.Error)
	}
	if msg := c.request("tools/call", "not an object"); msg.Error == nil || msg.Error.Code != codeInvalidParams {
		t.Fatalf("invalid params: %+v", msg.Error)
	}

	nullCh := c.expect("null")
	c.raw("this is not json")
	if msg := c.await(nullCh, "parse error"); msg.Error == nil || msg.Error.Code != codeParse {
		t.Fatalf("parse error: %+v", msg.Error)
	}
	nullCh = c.expect("null")
	c.raw(`[{"jsonrpc":"2.0","id":9,"method":"ping"}]`)
	if msg := c.await(nullCh, "batch"); msg.Error == nil || msg.Error.Code != codeInvalidRequest {
		t.Fatalf("batch: %+v", msg.Error)
	}
	c.notify("notifications/whatever", map[string]any{"x": 1})
	c.raw(`{"jsonrpc":"2.0","id":77,"result":{}}`)
	if msg := c.request("ping", nil); msg.Error != nil {
		t.Fatal("server stopped answering after ignorable messages", msg.Error)
	}
	if notes := c.drain(); len(notes) != 0 {
		t.Fatalf("unexpected notifications: %+v", notes)
	}
}

func TestCancelledRequestGetsNoResponse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "meeting.wav"), []byte("riff"))
	release := make(chan struct{})
	c := serve(t, newServer(dir, gated(release, nil)))

	var view jobView
	decode(t, c.call("transcribe", map[string]any{"path": filepath.Join(dir, "meeting.wav"), "wait_seconds": 0}, nil).StructuredContent, &view)
	if terminal(view.State) {
		t.Fatalf("job finished without release: %+v", view)
	}
	key, ch := c.start("tools/call", map[string]any{"name": "job_status", "arguments": map[string]any{"job_id": view.JobID, "wait_seconds": 300}})
	time.Sleep(50 * time.Millisecond)
	c.notify("notifications/cancelled", map[string]any{"requestId": json.RawMessage(key), "reason": "user"})
	if msg := c.request("ping", nil); msg.Error != nil {
		t.Fatal("ping failed while a request was being cancelled", msg.Error)
	}
	select {
	case msg := <-ch:
		t.Fatalf("cancelled request was answered: %+v", msg)
	case <-time.After(200 * time.Millisecond):
	}
	decode(t, c.call("cancel_job", map[string]any{"job_id": view.JobID}, nil).StructuredContent, &view)
	if view.State != stateCanceled || view.Hint == "" {
		t.Fatalf("cancel_job: %+v", view)
	}
}

func TestShutdownCancelsRunningJobs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "meeting.wav"), []byte("riff"))
	started := make(chan string, 1)
	c := serve(t, newServer(dir, gated(make(chan struct{}), started)))
	c.call("transcribe", map[string]any{"path": filepath.Join(dir, "meeting.wav"), "wait_seconds": 0}, nil)
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("job never started")
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeRejectsBadConfiguration(t *testing.T) {
	ctx := context.Background()
	if err := (&Server{Roots: []string{filepath.Join(t.TempDir(), "missing")}}).Serve(ctx, nil, nil); err == nil {
		t.Fatal("missing root accepted")
	}
	if err := (&Server{Roots: []string{t.TempDir()}, DefaultModel: "whisper"}).Serve(ctx, nil, nil); err == nil {
		t.Fatal("unknown default model accepted")
	}
	if err := CheckRoots([]string{"relative"}); err == nil {
		t.Fatal("relative root accepted")
	}
}
