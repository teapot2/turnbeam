// Package mcp serves Turnbeam's tools to AI assistants over the Model Context
// Protocol: JSON-RPC 2.0 messages, one per line, on stdin and stdout. The
// assistant finds recordings, starts transcriptions, and reads, renames and
// exports transcripts; summarizing them is the assistant's job.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// message is any incoming JSON-RPC message: a request (ID and method), a
// notification (method only) or a response to a request this server sent
// (result or error; it sends none, so those are ignored).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// response answers one request. Result is never nil on success, so a success
// always serializes a result member.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

var nullID = json.RawMessage("null")

// writer sends one JSON document per line. Handlers run concurrently and
// share it; encoding/json escapes newlines, so a document never spans lines.
type writer struct {
	mu  sync.Mutex
	out io.Writer
	err error
}

func (w *writer) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	_, w.err = w.out.Write(append(data, '\n'))
	return w.err
}

func (w *writer) respond(id json.RawMessage, result any) error {
	if id == nil {
		id = nullID
	}
	return w.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (w *writer) fail(id json.RawMessage, code int, message string) error {
	if id == nil {
		id = nullID
	}
	return w.send(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (w *writer) notify(method string, params any) error {
	return w.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// requestKey identifies a request by its ID regardless of the client's
// whitespace, so a cancellation notification matches the request it names.
func requestKey(id json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, id); err != nil {
		return string(id)
	}
	return compact.String()
}
