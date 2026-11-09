package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teapot2/turnbeam/internal/output"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/progress"
	"github.com/teapot2/turnbeam/internal/transcript"
)

const testTimeout = 10 * time.Second

// client drives a Server over pipes the way a host process would.
type client struct {
	t        *testing.T
	stdin    *io.PipeWriter
	cancel   context.CancelFunc
	served   chan error
	readDone chan struct{}
	notes    chan message
	closeErr error
	once     sync.Once

	mu      sync.Mutex
	pending map[string]chan message
	next    int
}

func serve(t *testing.T, s *Server) *client {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, stdin: stdinW, cancel: cancel, served: make(chan error, 1), readDone: make(chan struct{}), notes: make(chan message, 256), pending: map[string]chan message{}}
	go func() {
		c.served <- s.Serve(ctx, stdinR, stdoutW)
		stdoutW.Close()
	}()
	go c.read(stdoutR)
	t.Cleanup(func() { c.close() })
	return c
}

// close ends stdin and waits for the server to stop, as a host does.
func (c *client) close() error {
	c.once.Do(func() {
		c.stdin.Close()
		select {
		case c.closeErr = <-c.served:
		case <-time.After(testTimeout):
			c.cancel()
			c.closeErr = errors.New("server did not stop after stdin closed")
		}
		<-c.readDone
	})
	return c.closeErr
}

func (c *client) read(r io.Reader) {
	defer close(c.readDone)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
	for scanner.Scan() {
		var msg message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			c.t.Errorf("server wrote invalid JSON: %q", scanner.Bytes())
			continue
		}
		if msg.ID == nil {
			c.notes <- msg
			continue
		}
		c.mu.Lock()
		ch := c.pending[requestKey(msg.ID)]
		c.mu.Unlock()
		if ch == nil {
			c.t.Errorf("response for an unknown request id %s", msg.ID)
			continue
		}
		ch <- msg
	}
}

func (c *client) expect(id string) chan message {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan message, 1)
	c.pending[id] = ch
	return ch
}

func (c *client) raw(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.stdin, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) send(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.raw(string(data))
}

// start sends a request and returns the channel its response will arrive on.
func (c *client) start(method string, params any) (string, chan message) {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	key := requestKey(json.RawMessage(strings.TrimSpace(string(mustJSON(id)))))
	ch := c.expect(key)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.send(req)
	return key, ch
}

func (c *client) request(method string, params any) message {
	c.t.Helper()
	_, ch := c.start(method, params)
	return c.await(ch, method)
}

func (c *client) await(ch chan message, what string) message {
	c.t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(testTimeout):
		c.t.Fatalf("no response to %s", what)
		return message{}
	}
}

func (c *client) notify(method string, params any) {
	c.t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call invokes a tool and returns its result; a JSON-RPC error fails the test.
func (c *client) call(name string, args any, progressToken any) toolResult {
	c.t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	if progressToken != nil {
		params["_meta"] = map[string]any{"progressToken": progressToken}
	}
	msg := c.request("tools/call", params)
	if msg.Error != nil {
		c.t.Fatalf("%s: %v", name, msg.Error)
	}
	var res toolResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		c.t.Fatal(err)
	}
	return res
}

// failure invokes a tool that must fail and returns the message.
func (c *client) failure(name string, args any) string {
	c.t.Helper()
	res := c.call(name, args, nil)
	if !res.IsError || len(res.Content) != 1 {
		c.t.Fatalf("%s did not fail: %+v", name, res)
	}
	return res.Content[0].Text
}

// drain returns the notifications received so far.
func (c *client) drain() []message {
	var notes []message
	for {
		select {
		case n := <-c.notes:
			notes = append(notes, n)
		default:
			return notes
		}
	}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// decode converts structuredContent into a typed view.
func decode(t *testing.T, v any, into any) {
	t.Helper()
	if err := json.Unmarshal(mustJSON(v), into); err != nil {
		t.Fatal(err)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func sampleTranscript() *transcript.Transcript {
	assigned := transcript.Attribution{Status: transcript.Assigned, Method: transcript.TimelineOverlap}
	return &transcript.Transcript{SchemaVersion: 1,
		Recording: transcript.Recording{Name: "meeting.wav", DurationMS: 10000, SHA256: strings.Repeat("a", 64)},
		Pipeline:  transcript.Pipeline{MetaModel: "test", MetaMode: "DIARIZATION", DiarizationModel: "test", DiarizationRevision: "test", RuntimeVersion: "test"},
		Speakers:  []transcript.Speaker{{ID: "s1", Index: 1}, {ID: "s2", Index: 2}},
		Turns: []transcript.Turn{
			{ID: "t1", StartMS: 100, EndMS: 2000, SpeakerID: new("s1"), Text: "Hello.", Attribution: assigned, Source: transcript.Source{ChunkID: "c", MetaTurnID: 0}},
			{ID: "t2", StartMS: 2500, EndMS: 5000, SpeakerID: new("s2"), Text: "Hi there.", Attribution: assigned, Source: transcript.Source{ChunkID: "c", MetaTurnID: 1}},
			{ID: "t3", StartMS: 6000, EndMS: 9000, Text: "Who said that?", Attribution: transcript.Attribution{Status: transcript.Unknown, Reason: transcript.AmbiguousOverlapOrBoundary}, Source: transcript.Source{ChunkID: "c", MetaTurnID: 2}},
		},
		SourceChunks: []transcript.SourceChunk{{ID: "c", EndMS: 10000, MetaSessionID: "session", RawTurns: []transcript.RawTurn{
			{TurnID: 0, StartMS: 100, EndMS: 2000, Transcript: "Hello."}, {TurnID: 1, StartMS: 2500, EndMS: 5000, Transcript: "Hi there."}, {TurnID: 2, StartMS: 6000, EndMS: 9000, Transcript: "Who said that?"},
		}}},
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeSidecar(t *testing.T, dir, stem string) string {
	t.Helper()
	data, err := transcript.Encode(sampleTranscript())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, stem+".transcript.json")
	writeFile(t, path, data)
	return path
}

// publishing is a pipeline fake that reports progress and writes the sample
// transcript through the real output package.
func publishing(onCall func(pipeline.Options)) func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error {
	return func(ctx context.Context, input string, opts pipeline.Options, stdout, stderr io.Writer) error {
		if onCall != nil {
			onCall(opts)
		}
		if opts.Progress.Observer != nil {
			opts.Progress.Observer(progress.Event{Stage: progress.Normalize})
			opts.Progress.Observer(progress.Event{Stage: progress.Transcribe, Done: 0, Total: 3})
			opts.Progress.Observer(progress.Event{Stage: progress.Transcribe, Done: 3, Total: 3})
		}
		d, err := output.Open(ctx, opts.Output, opts.Render, stdout)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.Write(ctx, sampleTranscript()); err != nil {
			return err
		}
		if opts.Saved != nil && d.Path() != "" {
			opts.Saved(d.Path(), d.Sidecar())
		}
		return nil
	}
}

// gated is a pipeline fake that waits for release, or for cancellation.
func gated(release <-chan struct{}, started chan<- string) func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error {
	return func(ctx context.Context, input string, opts pipeline.Options, stdout, stderr io.Writer) error {
		if started != nil {
			started <- input
		}
		select {
		case <-release:
			return publishing(nil)(ctx, input, opts, stdout, stderr)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func newServer(dir string, transcribe func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error) *Server {
	return &Server{Version: "test", Roots: []string{dir}, DefaultModel: "parakeet", Transcribe: transcribe,
		Probe: func(context.Context, string) (time.Duration, error) { return 0, errors.New("no probe in this test") }}
}
