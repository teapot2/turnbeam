package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/teapot2/turnbeam/internal/apple"
	"github.com/teapot2/turnbeam/internal/pipeline"
	"github.com/teapot2/turnbeam/internal/setup"
	"github.com/teapot2/turnbeam/internal/transcript"
)

// tool is one MCP tool: its published description and its handler.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	handler     func(context.Context, *Server, *call, json.RawMessage) (*toolResult, error)
}

type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// result pairs a JSON-serializable value with its text form, so clients that
// read only content and clients that read structuredContent both get it.
func result(v any) (*toolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &toolResult{Content: []content{{Type: "text", Text: string(data)}}, StructuredContent: v}, nil
}

func textResult(text string, v any) *toolResult {
	return &toolResult{Content: []content{{Type: "text", Text: text}}, StructuredContent: v}
}

func toolList() []tool {
	tools := make([]tool, len(catalog))
	copy(tools, catalog)
	return tools
}

func findTool(name string) *tool {
	for i := range catalog {
		if catalog[i].Name == name {
			return &catalog[i]
		}
	}
	return nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage, calls *inflight, key string) (any, *rpcError) {
	var request struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	}
	t := findTool(request.Name)
	if t == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool"}
	}
	c := &call{progressToken: request.Meta.ProgressToken}
	calls.mu.Lock()
	if current, ok := calls.calls[key]; ok {
		current.progressToken = request.Meta.ProgressToken
		c = current
	}
	calls.mu.Unlock()
	if len(bytes.TrimSpace(request.Arguments)) == 0 {
		request.Arguments = json.RawMessage("{}")
	}
	res, err := t.handler(ctx, s, c, request.Arguments)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrArgument) {
			return nil, &rpcError{Code: codeInternal, Message: "request canceled"}
		}
		if s.Verbose {
			s.log.Printf("%s failed: %s", request.Name, s.describe(err))
		}
		return &toolResult{Content: []content{{Type: "text", Text: s.describe(err)}}, IsError: true}, nil
	}
	return res, nil
}

// decodeArgs rejects unknown fields, so a misspelled argument fails instead
// of being silently ignored.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return argumentError("%v", err)
	}
	if dec.More() {
		return argumentError("arguments must be a single object")
	}
	return nil
}

// progress sends a notifications/progress message when the client asked for
// them. Progress must increase, so callers pass a running count.
func (s *Server) progress(c *call, count int, message string) {
	if c == nil || c.progressToken == nil {
		return
	}
	s.writer.notify("notifications/progress", map[string]any{
		"progressToken": c.progressToken, "progress": count, "message": message,
	})
}

// modelNames lists every model: Meta first, then the local models.
func modelNames() []string {
	names := []string{pipeline.MetaModel}
	for _, model := range setup.LocalModels() {
		names = append(names, model.Name)
	}
	return append(names, apple.Model)
}

func joinChoices[T ~string](values []T) string {
	names := make([]string, len(values))
	for i, value := range values {
		names[i] = string(value)
	}
	return strings.Join(names, ", ")
}

func enumSchema[T ~string](description string, values []T) map[string]any {
	names := make([]any, len(values))
	for i, value := range values {
		names[i] = string(value)
	}
	return map[string]any{"type": "string", "description": description, "enum": names}
}

// renderOptions validates presentation arguments; empty values take the
// defaults, and an output extension picks the format when none is given.
func renderOptions(format, timestamps, output string) (transcript.RenderOptions, error) {
	if format == "" && output != "" {
		if f, ok := transcript.FormatForExtension(output); ok {
			format = string(f)
		}
	}
	opts := transcript.RenderOptions{Format: transcript.Format(format), Timestamps: transcript.Timestamps(timestamps)}
	if opts.Format == "" {
		opts.Format = transcript.FormatMarkdown
	}
	if opts.Timestamps == "" {
		opts.Timestamps = transcript.TimestampsStart
	}
	if err := opts.Validate(); err != nil {
		return opts, argumentError("format must be one of %s and timestamps one of %s", joinChoices(transcript.Formats), joinChoices(transcript.TimestampStyles))
	}
	return opts, nil
}

var catalog = []tool{
	{
		Name: "list_recordings",
		Description: "Find meeting recordings (audio or video files) on this Mac, newest first, with their modification time, duration and whether a transcript already exists. " +
			"Without a directory, the usual folders (Downloads, Desktop, Documents, Movies) are searched.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"directory": map[string]any{"type": "string", "description": "Absolute directory (or ~/...) to search."},
				"since":     map[string]any{"type": "string", "description": "Only recordings modified on or after this local date (YYYY-MM-DD) or RFC 3339 time."},
				"until":     map[string]any{"type": "string", "description": "Only recordings modified on or before this local date (YYYY-MM-DD) or RFC 3339 time."},
				"recursive": map[string]any{"type": "boolean", "description": "Search subfolders too (default true, a few levels deep; hidden folders are skipped)."},
				"limit":     map[string]any{"type": "integer", "description": "Maximum results (default 20, at most 50).", "minimum": 1, "maximum": maxRecordings},
			},
		},
		Annotations: map[string]any{"readOnlyHint": true},
		handler:     listRecordings,
	},
	{
		Name: "transcribe",
		Description: "Start transcribing a recording into a speaker-labeled transcript. Returns a job: when its state is not yet done, call job_status with the job_id. " +
			"Writes the transcript next to the recording (or at output) plus a .transcript.json sidecar. Existing files are never overwritten: when the name is taken, the run writes a numbered one such as meeting-2.md, and the job's output and transcript fields give the actual paths. " +
			"Local models (parakeet, nemotron-3.5, nemotron-en, parakeet-ctc, and apple, Apple's own transcriber with language) run on this Mac; meta sends the audio to Meta's API and needs a key.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"path"},
			"properties": map[string]any{
				"path":         map[string]any{"type": "string", "description": "Absolute path (or ~/...) of the recording."},
				"model":        enumSchema("Transcription model; the default is the server's configured default.", modelNames()),
				"output":       map[string]any{"type": "string", "description": "Transcript file to write (default: next to the recording, named after it with the format's extension: .md, .txt, .srt, .vtt, or .transcript.json for json). Its extension picks the format."},
				"format":       enumSchema("Transcript format (default: from the output extension, else markdown).", transcript.Formats),
				"timestamps":   enumSchema("Timestamp style for markdown and text (default start).", transcript.TimestampStyles),
				"resume":       map[string]any{"type": "boolean", "description": "Meta only: continue an interrupted run from its checkpoints instead of paying again."},
				"language":     map[string]any{"type": "string", "description": "apple only: the recording's language as a tag such as en-US or es-MX (default: the Mac's language)."},
				"wait_seconds": map[string]any{"type": "integer", "description": "How long to wait for the job before returning (default 20, at most 300).", "minimum": 0, "maximum": maxWaitSeconds},
			},
		},
		Annotations: map[string]any{"destructiveHint": false, "openWorldHint": true},
		handler:     transcribe,
	},
	{
		Name:        "job_status",
		Description: "Report a transcription job's progress, waiting up to wait_seconds for it to finish. When done, the result names the transcript files and summarizes speakers and turns.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"job_id"},
			"properties": map[string]any{
				"job_id":       map[string]any{"type": "string"},
				"wait_seconds": map[string]any{"type": "integer", "description": "How long to wait before returning (default 20, at most 300).", "minimum": 0, "maximum": maxWaitSeconds},
			},
		},
		Annotations: map[string]any{"readOnlyHint": true},
		handler:     jobStatus,
	},
	{
		Name:        "cancel_job",
		Description: "Stop a running or queued transcription job. Meta runs keep their checkpoints for a later resume.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"job_id"},
			"properties": map[string]any{"job_id": map[string]any{"type": "string"}},
		},
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		handler:     cancelJob,
	},
	{
		Name: "read_transcript",
		Description: "Return an existing transcript's text (markdown by default) from its .transcript.json sidecar, given the sidecar or the recording it belongs to. " +
			"start_seconds and end_seconds select part of a long meeting. Speaker names, duration and turn counts come back as structured data.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"path"},
			"properties": map[string]any{
				"path":          map[string]any{"type": "string", "description": "Absolute path (or ~/...) of the recording or its .transcript.json."},
				"format":        enumSchema("Text format (default markdown).", transcript.Formats),
				"timestamps":    enumSchema("Timestamp style for markdown and text (default start).", transcript.TimestampStyles),
				"start_seconds": map[string]any{"type": "number", "description": "Only turns that end after this recording time.", "minimum": 0},
				"end_seconds":   map[string]any{"type": "number", "description": "Only turns that start before this recording time.", "minimum": 0},
			},
		},
		Annotations: map[string]any{"readOnlyHint": true},
		handler:     readTranscript,
	},
	{
		Name:        "rename_speakers",
		Description: "Give speakers display names in a transcript's .transcript.json sidecar, by their index (Speaker 1 is index 1). Existing exported files keep the old names; read_transcript and export use the new ones.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"path", "names"},
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "Absolute path (or ~/...) of the recording or its .transcript.json."},
				"names": map[string]any{"type": "object", "description": "Speaker index to name, for example {\"1\": \"Alice\", \"2\": \"Bob\"}.", "additionalProperties": map[string]any{"type": "string"}},
			},
		},
		Annotations: map[string]any{"destructiveHint": false, "idempotentHint": true},
		handler:     renameSpeakers,
	},
	{
		Name:        "export",
		Description: "Write a transcript file from an existing .transcript.json sidecar, in markdown, text, JSON or SRT/WebVTT subtitles. The output's extension picks the format; existing files are never overwritten.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"path", "output"},
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "Absolute path (or ~/...) of the recording or its .transcript.json."},
				"output":     map[string]any{"type": "string", "description": "Absolute path (or ~/...) of the file to write."},
				"format":     enumSchema("Format (default: from the output extension, else markdown).", transcript.Formats),
				"timestamps": enumSchema("Timestamp style for markdown and text (default start).", transcript.TimestampStyles),
			},
		},
		Annotations: map[string]any{"destructiveHint": false},
		handler:     export,
	},
	{
		Name:        "status",
		Description: "Check whether Turnbeam is ready: FFmpeg, the speaker model, installed transcription models, whether a Meta API key is configured, the accessible directories and current jobs. Offline.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		Annotations: map[string]any{"readOnlyHint": true},
		handler:     status,
	},
}
