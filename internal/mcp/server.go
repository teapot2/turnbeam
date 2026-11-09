package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/credentials"
	"github.com/teapot2/turnbeam/internal/media"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
)

// Protocol revisions this server speaks. Its tools behave the same under each,
// so a client's known revision is echoed; an unknown one gets the latest.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const (
	latestProtocol = "2025-06-18"
	// maxMessageBytes bounds one incoming line; tool arguments are small.
	maxMessageBytes = 16 << 20
	// maxInFlight bounds concurrent requests; reading pauses beyond it.
	maxInFlight = 8
)

// Installation answers offline questions about the managed runtime and models.
type Installation interface {
	Doctor(context.Context) ([]setup.Check, error)
	LocalStatus(context.Context) ([]setup.ModelStatus, error)
}

// Server exposes Turnbeam as MCP tools. Zero-value dependencies select the
// product implementations; tests inject fakes.
type Server struct {
	Version string
	// Roots are the directories tools may read and write. Paths outside them
	// are refused. Empty means the home directory.
	Roots []string
	// InstallDir locates the managed runtime and models; empty uses the default.
	InstallDir string
	// DefaultModel transcribes when a call names none, unless Default is set.
	DefaultModel string
	// Default, when set, picks the model for a call that names none, each time,
	// so a default saved while the server runs applies to the next job.
	Default func(context.Context) (string, error)
	// Transcribe runs the pipeline; it defaults to pipeline.Runner.
	Transcribe func(context.Context, string, pipeline.Options, io.Writer, io.Writer) error
	// Installation defaults to setup.Manager for InstallDir.
	Installation Installation
	// Apple reports Apple's transcriber; it defaults to apple.Helper for InstallDir.
	Apple AppleStatus
	// KeyStatus reports whether a Meta API key is available, never the key.
	KeyStatus func(context.Context) (credentials.Source, bool, error)
	// Probe returns a recording's duration; it defaults to FFprobe.
	Probe func(context.Context, string) (time.Duration, error)
	// Describe turns a failure into text safe to return to the assistant.
	Describe func(error) string
	// Stderr receives diagnostics; the protocol owns stdout.
	Stderr  io.Writer
	Verbose bool

	roots  roots
	writer *writer
	jobs   *jobTable
	log    *log.Logger
}

// call is one in-flight request.
type call struct {
	cancel        context.CancelFunc
	progressToken json.RawMessage
	canceled      bool
}

type inflight struct {
	mu    sync.Mutex
	calls map[string]*call
}

func (f *inflight) add(key string, c *call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[key] = c
}

func (f *inflight) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.calls, key)
}

// cancel stops the request and marks it so no response is sent for it.
func (f *inflight) cancel(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.calls[key]; ok {
		c.canceled = true
		c.cancel()
	}
}

func (f *inflight) wasCanceled(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.calls[key]
	return ok && c.canceled
}

// AppleStatus reports whether Apple's transcriber is set up, and for which language.
type AppleStatus interface {
	Built() bool
	Status(context.Context, string) (apple.State, error)
}

// defaultModel is the model for a call that names none. The server runs only
// catalog models, so a default that is a model file must be named instead.
func (s *Server) defaultModel(ctx context.Context) (string, error) {
	if s.Default == nil {
		return s.DefaultModel, nil
	}
	name, err := s.Default(ctx)
	if err != nil {
		return "", argumentError("cannot determine the default model (%s); pass model", s.describe(err))
	}
	if !slices.Contains(modelNames(), name) {
		return "", argumentError("the default model %s cannot be used here; pass model, one of: %s", name, strings.Join(modelNames(), ", "))
	}
	return name, nil
}

func (s *Server) init() error {
	roots, err := newRoots(s.Roots)
	if err != nil {
		return err
	}
	s.roots = roots
	if s.DefaultModel == "" {
		s.DefaultModel = pipeline.MetaModel
	}
	if s.Default == nil && !slices.Contains(modelNames(), s.DefaultModel) {
		return fmt.Errorf("default model must be one of: %v", modelNames())
	}
	if s.Transcribe == nil {
		s.Transcribe = pipeline.Runner{}.Run
	}
	if s.Installation == nil {
		s.Installation = setup.Manager{Dir: s.InstallDir}
	}
	if s.Apple == nil {
		s.Apple = apple.Helper{Dir: s.InstallDir}
	}
	if s.KeyStatus == nil {
		s.KeyStatus = credentials.Provider{}.Status
	}
	if s.Probe == nil {
		s.Probe = func(ctx context.Context, path string) (time.Duration, error) {
			info, err := media.Processor{}.Probe(ctx, path)
			return info.Duration, err
		}
	}
	if s.Describe == nil {
		s.Describe = func(err error) string { return err.Error() }
	}
	if s.Stderr == nil {
		s.Stderr = io.Discard
	}
	s.log = log.New(s.Stderr, "turnbeam mcp: ", 0)
	return nil
}

// Serve handles messages until stdin ends or ctx is canceled. Transcriptions
// started by tools run in the background for the server's lifetime and are
// canceled when it stops; Meta runs keep their checkpoints.
func (s *Server) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	if err := s.init(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.writer = &writer{out: stdout}
	s.jobs = newJobTable(ctx, s.runJob)
	calls := &inflight{calls: map[string]*call{}}
	s.log.Printf("serving over stdio; accessible directories: %v", s.roots)

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			select {
			case lines <- bytes.Clone(line):
			case <-ctx.Done():
				return
			}
		}
		readErr <- scanner.Err()
	}()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxInFlight)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			s.dispatch(ctx, line, calls, &wg, sem)
		}
	}
	cancel()
	wg.Wait()
	s.jobs.wait()
	select {
	case err := <-readErr:
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
	default:
	}
	return nil
}

// dispatch answers malformed input at once and runs each request in its own
// goroutine, so a long tool call never blocks ping, tools/list or cancellation.
func (s *Server) dispatch(ctx context.Context, line []byte, calls *inflight, wg *sync.WaitGroup, sem chan struct{}) {
	if line[0] == '[' {
		s.writer.fail(nil, codeInvalidRequest, "batch requests are not supported")
		return
	}
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		s.writer.fail(nil, codeParse, "parse error")
		return
	}
	if msg.Method == "" {
		return // A response to a request this server never sent.
	}
	if msg.ID == nil {
		s.handleNotification(msg, calls)
		return
	}
	key := requestKey(msg.ID)
	rctx, rcancel := context.WithCancel(ctx)
	calls.add(key, &call{cancel: rcancel})
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		rcancel()
		calls.remove(key)
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { <-sem }()
		defer calls.remove(key)
		defer rcancel()
		result, rpcErr := s.handle(rctx, msg, calls, key)
		if calls.wasCanceled(key) {
			return // The client withdrew the request and expects no response.
		}
		if rpcErr != nil {
			s.writer.fail(msg.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		s.writer.respond(msg.ID, result)
	}()
}

func (s *Server) handleNotification(msg message, calls *inflight) {
	switch msg.Method {
	case "notifications/initialized", "notifications/roots/list_changed":
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &params) == nil && params.RequestID != nil {
			calls.cancel(requestKey(params.RequestID))
		}
	default:
		if s.Verbose {
			s.log.Printf("ignoring notification %q", msg.Method)
		}
	}
}

func (s *Server) handle(ctx context.Context, msg message, calls *inflight, key string) (any, *rpcError) {
	if s.Verbose {
		s.log.Printf("request %s", msg.Method)
	}
	switch msg.Method {
	case "initialize":
		return s.initialize(msg.Params), nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		return s.callTool(ctx, msg.Params, calls, key)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found"}
}

func (s *Server) initialize(params json.RawMessage) any {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(params, &request)
	version := latestProtocol
	if slices.Contains(protocolVersions, request.ProtocolVersion) {
		version = request.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "turnbeam", "version": s.Version},
		"instructions":    instructions,
	}
}

const instructions = "Turnbeam transcribes recorded meetings on this Mac and labels who spoke when. " +
	"Use list_recordings to find a recording, transcribe to start a run (it returns a job; " +
	"call job_status until its state is done), then read_transcript to get the text. " +
	"Speakers are labeled Speaker 1, Speaker 2, … until rename_speakers gives them names; " +
	"turns the speaker model could not attribute are labeled Unknown speaker and must stay that way. " +
	"Transcripts are verbatim: summarize them yourself."

// describe writes a failure for the assistant. Argument errors are written
// for it already; everything else is classified without echoing paths,
// responses or keys.
func (s *Server) describe(err error) string {
	if errors.Is(err, ErrArgument) {
		return err.Error()
	}
	return s.Describe(err)
}
