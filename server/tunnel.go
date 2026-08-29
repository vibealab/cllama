package server

// Parent-side reverse tunnel.
//
// A child cllama server connects to this (parent) server with
//     GET /admin/parent/stream?model=<parent model>
// and keeps the SSE connection open. The parent pushes request events to the
// child over that stream; the child executes the request against its own
// backends and POSTs the result back to
//     POST /admin/parent/response/<id>            (single-shot)
//     POST /admin/parent/response/<id>?stream=true (NDJSON lines)
//
// Wire protocol:
//
//	parent -> child, SSE event:
//	  data: {"id":"...","kind":"chat"|"embed","stream":bool,"body":{...}}
//	  ("body" is an OpenAI-format request whose model has already been
//	  rewritten to the child's model name; keepalive lines start with ":")
//
//	child -> parent, single-shot body (one JSON value):
//	  {"error": "..."}          -> request failed
//	  <OpenAI-format response>   -> success
//
//	child -> parent, stream NDJSON lines:
//	  {"error": "..."}          -> stream failed (possibly mid-stream)
//	  <StreamingChatResponse>  -> one SSE chunk, relayed as `data: <line>`
//	  [DONE]                   -> end of stream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"cllama/client"
)

// tunnelEvent is one request pushed from parent to child over the SSE stream.
type tunnelEvent struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"` // "chat" | "embed"
	Stream bool            `json:"stream,omitempty"`
	Body   json.RawMessage `json:"body"`
}

// tunnelCall tracks one in-flight tunneled request.
type tunnelCall struct {
	id     string
	owner  *tunnelClient
	events chan string // NDJSON lines from the child (buffered)
	done   chan struct{}
	once   sync.Once

	mu    sync.Mutex
	failed string
}

func (c *tunnelCall) finishCall() { c.once.Do(func() { close(c.done) }) }

func (c *tunnelCall) setFail(msg string) {
	c.mu.Lock()
	c.failed = msg
	c.mu.Unlock()
}

func (c *tunnelCall) failReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

// callRegistry maps request ids to in-flight calls (parent side).
type callRegistry struct {
	mu    sync.Mutex
	calls map[string]*tunnelCall
}

func newCallRegistry() *callRegistry {
	return &callRegistry{calls: make(map[string]*tunnelCall)}
}

func (cr *callRegistry) register(call *tunnelCall) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.calls[call.id] = call
}

func (cr *callRegistry) get(id string) *tunnelCall {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.calls[id]
}

func (cr *callRegistry) remove(id string) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	delete(cr.calls, id)
}

// failOwner finishes all pending calls belonging to a disconnected tunnel.
func (cr *callRegistry) failOwner(owner *tunnelClient, reason string) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	for id, c := range cr.calls {
		if c.owner == owner {
			c.setFail(reason)
			c.finishCall()
			delete(cr.calls, id)
		}
	}
}

// feed delivers one NDJSON line to a waiting call, waiting up to waitFeed for
// the consumer to catch up. It reports whether the line was delivered.
func (cr *callRegistry) feed(call *tunnelCall, line string) bool {
	select {
	case call.events <- line:
		return true
	case <-call.done:
		return false
	case <-time.After(60 * time.Second):
		return false
	}
}

// envelopeError returns the error message if line is a tunnel error
// envelope ({"error": "..."}), otherwise "".
func envelopeError(line string) string {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(line), &env) == nil {
		return env.Error
	}
	return ""
}

// tunnelClient implements client.Client on top of a child cllama server that
// connected through /admin/parent/stream. Requests are pushed to the child
// via out; results arrive through the call registry.
type tunnelClient struct {
	reg *callRegistry

	mu   sync.Mutex
	dead string // non-empty once the child disconnected
	out  chan string
}

var _ client.Client = (*tunnelClient)(nil)

func newTunnelClient(reg *callRegistry) *tunnelClient {
	return &tunnelClient{
		reg: reg,
		out: make(chan string, 32),
	}
}

// kill marks the tunnel dead and fails all pending calls.
func (t *tunnelClient) kill(reason string) {
	t.mu.Lock()
	if t.dead == "" {
		t.dead = reason
	}
	t.mu.Unlock()
	t.reg.failOwner(t, reason)
}

// send registers a call and pushes the request event to the child.
func (t *tunnelClient) send(kind string, stream bool, body any) (*tunnelCall, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	id := generateID()
	payload, err := json.Marshal(tunnelEvent{ID: id, Kind: kind, Stream: stream, Body: raw})
	if err != nil {
		return nil, err
	}

	call := &tunnelCall{
		id:     id,
		owner:  t,
		events: make(chan string, 64),
		done:   make(chan struct{}),
	}

	t.mu.Lock()
	if t.dead != "" {
		reason := t.dead
		t.mu.Unlock()
		return nil, errors.New(reason)
	}
	t.reg.register(call)
	select {
	case t.out <- string(payload):
		t.mu.Unlock()
		return call, nil
	default:
		t.reg.remove(id)
		t.mu.Unlock()
		return nil, errors.New("tunnel child is disconnected or overloaded")
	}
}

// wait blocks for the first response line of a single-shot call.
func (t *tunnelClient) wait(ctx context.Context, call *tunnelCall) (string, error) {
	select {
	case line := <-call.events:
		return line, nil
	case <-call.done:
		if msg := call.failReason(); msg != "" {
			return "", errors.New(msg)
		}
		return "", errors.New("tunnel closed before response")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Chat sends a non-streaming chat request through the tunnel.
func (t *tunnelClient) Chat(ctx context.Context, req *client.ChatRequest) (*client.ChatResponse, error) {
	call, err := t.send("chat", false, req)
	if err != nil {
		return nil, err
	}
	defer t.reg.remove(call.id)

	line, err := t.wait(ctx, call)
	if err != nil {
		return nil, err
	}
	if msg := envelopeError(line); msg != "" {
		return nil, errors.New("child cllama: " + msg)
	}

	var resp client.ChatResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("tunnel chat decode: %w", err)
	}
	return &resp, nil
}

// Embeddings sends an embedding request through the tunnel. The child always
// replies in OpenAI format; this endpoint's handler re-formats as needed.
func (t *tunnelClient) Embeddings(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	call, err := t.send("embed", false, map[string]interface{}{
		"model": model,
		"input": input,
	})
	if err != nil {
		return nil, err
	}
	defer t.reg.remove(call.id)

	line, err := t.wait(ctx, call)
	if err != nil {
		return nil, err
	}
	if msg := envelopeError(line); msg != "" {
		return nil, errors.New("child cllama: " + msg)
	}

	var resp client.EmbeddingResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("tunnel embed decode: %w", err)
	}
	embeddings := make([][]float64, len(resp.Data))
	for _, d := range resp.Data {
		if d.Index >= 0 && d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}
	return embeddings, nil
}

// ChatStream sends a streaming chat request through the tunnel and returns a
// reader over the child's NDJSON response lines.
func (t *tunnelClient) ChatStream(ctx context.Context, req *client.ChatRequest) (io.ReadCloser, error) {
	call, err := t.send("chat", true, req)
	if err != nil {
		return nil, err
	}
	return &tunnelReader{ctx: ctx, t: t, call: call}, nil
}

// StreamToSSE converts the child's NDJSON lines into OpenAI SSE events.
func (t *tunnelClient) StreamToSSE(ctx context.Context, body io.Reader, model string, out chan<- string) error {
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "[DONE]" {
			select {
			case out <- "data: [DONE]\n\n":
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		}
		if msg := envelopeError(line); msg != "" {
			return errors.New("child cllama: " + msg) // stream failed; end without [DONE]
		}
		select {
		case out <- "data: " + line + "\n\n":
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("tunnel stream read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("tunnel child closed before [DONE]")
}

// tunnelReader turns a call's NDJSON lines into an io.ReadCloser.
type tunnelReader struct {
	ctx  context.Context
	t    *tunnelClient
	call *tunnelCall

	buf []byte
	eof bool
}

func (r *tunnelReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		select {
		case line := <-r.call.events:
			r.buf = append([]byte(line), '\n')
			if line == "[DONE]" {
				r.eof = true
			}
		case <-r.call.done:
			r.eof = true
		case <-r.ctx.Done():
			r.eof = true
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *tunnelReader) Close() error {
	r.t.reg.remove(r.call.id)
	return nil
}
