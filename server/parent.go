package server

// Child side of the reverse tunnel.
//
// For each -parent URL (http://[token@]parent-host:port/<parent-model>), a
// ParentConn keeps a long-lived SSE connection to the parent's
// /admin/parent/stream endpoint. Request events pushed by the parent are
// executed against this server's own backends (exec* in api_llm.go) and the
// results are POSTed back to the parent's /admin/parent/response endpoint,
// either single-shot or as an NDJSON stream.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cllama/client"
)

const (
	parentEventTimeout   = 30 * time.Minute
	parentReconnectMin   = 1 * time.Second
	parentReconnectMax   = 15 * time.Second
	parentStreamMaxLine  = 64 * 1024 * 1024 // request events may carry base64 images
	parentHealthyFor     = 30 * time.Second // reconnect delay resets after this
)

// ParentConn is one outbound connection to a parent cllama server.
type ParentConn struct {
	ID    string
	Base  string // e.g. http://127.0.0.1:11435 (no credentials)
	Token string // optional bearer token presented to the parent
	Model string // model name on the parent server
	ToModel string // local model to rewrite requests to (optional)

	srv  *Server
	http *http.Client

	ctx    context.Context // per-connection context, cancelled by Remove()
	cancel context.CancelFunc

	mu        sync.Mutex
	connected bool
	connects  int
	lastErr   string
}

// ParentManager owns all parent connections.
type ParentManager struct {
	srv *Server

	mu      sync.Mutex
	conns   map[string]*ParentConn
	started bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewParentManager(srv *Server) *ParentManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ParentManager{
		srv:    srv,
		conns:  make(map[string]*ParentConn),
		ctx:    ctx,
		cancel: cancel,
	}
}

// parseParentURL parses http://[token@]host:port/model into its parts.
func parseParentURL(raw string) (base, token, model, toModel string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", fmt.Errorf("invalid parent URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", "", "", fmt.Errorf("parent URL %q must start with http:// or https://", raw)
	}
	if u.Host == "" {
		return "", "", "", "", fmt.Errorf("parent URL %q is missing host", raw)
	}
	model = strings.Trim(u.Path, "/")
	if model == "" || strings.Contains(model, "/") {
		return "", "", "", "", fmt.Errorf("parent URL %q must end with the model name, e.g. http://host:11435/mock-model1", raw)
	}
	token = u.User.Username()
	toModel = u.Query().Get("to") // optional local model alias
	u.User = nil
	base = u.Scheme + "://" + u.Host
	return base, token, model, toModel, nil
}

// Add registers a parent connection and (once started) connects immediately.
func (m *ParentManager) Add(rawURL string) (*ParentConn, error) {
	base, token, model, toModel, err := parseParentURL(rawURL)
	if err != nil {
		return nil, err
	}

	connCtx, connCancel := context.WithCancel(m.ctx)
	pc := &ParentConn{
		ID:      generateID(),
		Base:    base,
		Token:   token,
		Model:   model,
		ToModel: toModel,
		srv:     m.srv,
		http:    &http.Client{}, // no timeout: SSE streams are long-lived
		ctx:     connCtx,
		cancel:  connCancel,
		lastErr: "connecting",
	}

	m.mu.Lock()
	m.conns[pc.ID] = pc
	started := m.started
	m.mu.Unlock()

	if started {
		m.startConn(pc)
	}
	m.srv.events.emit(topicParents)
	return pc, nil
}

// Remove disconnects and forgets a parent connection.
func (m *ParentManager) Remove(id string) bool {
	m.mu.Lock()
	pc := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()
	if pc == nil {
		return false
	}
	pc.cancel()
	m.srv.events.emit(topicParents)
	return true
}

// Start launches all registered connections.
func (m *ParentManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	for _, pc := range m.conns {
		m.startConn(pc)
	}
}

func (m *ParentManager) startConn(pc *ParentConn) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		pc.run(pc.ctx)
	}()
}

// Stop disconnects all parents and waits for the loops to exit.
func (m *ParentManager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// List returns admin views of all parent connections.
func (m *ParentManager) List() []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(m.conns))
	for _, pc := range m.conns {
		pc.mu.Lock()
		out = append(out, map[string]interface{}{
			"id":         pc.ID,
			"parent":     pc.Base,
			"model":      pc.Model,
			"to_model":   pc.ToModel,
			"has_token":  pc.Token != "",
			"connected":  pc.connected,
			"connects":   pc.connects,
			"last_error": pc.lastErr,
		})
		pc.mu.Unlock()
	}
	return out
}

// Get returns one connection's admin view.
func (m *ParentManager) Get(id string) (map[string]interface{}, bool) {
	for _, view := range m.List() {
		if view["id"] == id {
			return view, true
		}
	}
	return nil, false
}

// ── Connection loop ──────────────────────────────────────────────────────────

func (pc *ParentConn) run(ctx context.Context) {
	delay := parentReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := pc.serveOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		pc.markDisconnected(err)
		log.Printf("[parent] reconnecting to %s model %q (last connection lasted %s)",
			pc.Base, pc.Model, time.Since(start).Round(time.Millisecond))

		if time.Since(start) > parentHealthyFor {
			delay = parentReconnectMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < parentReconnectMax {
			delay *= 2
		}
	}
}

func (pc *ParentConn) serveOnce(ctx context.Context) error {
	streamURL := pc.Base + "/admin/parent/stream?model=" + url.QueryEscape(pc.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	if pc.Token != "" {
		req.Header.Set("Authorization", "Bearer "+pc.Token)
	}

	resp, err := pc.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("parent stream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		log.Printf("[parent] connecting to %s model %q failed: %v", pc.Base, pc.Model, err)
		return err
	}
	pc.markConnected()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), parentStreamMaxLine)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // keepalive comments, blank lines
		}
		var evt tunnelEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
			continue
		}
		if evt.ID == "" {
			continue
		}
		go pc.handleEvent(ctx, &evt)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("parent stream closed")
}

func (pc *ParentConn) markConnected() {
	pc.mu.Lock()
	pc.connected = true
	pc.connects++
	n := pc.connects
	pc.lastErr = ""
	pc.mu.Unlock()
	log.Printf("[parent] connected to parent %s model %q (connection #%d)", pc.Base, pc.Model, n)
	pc.srv.events.emit(topicParents)
}

func (pc *ParentConn) markDisconnected(err error) {
	pc.mu.Lock()
	pc.connected = false
	if err != nil {
		pc.lastErr = err.Error()
	}
	pc.mu.Unlock()
	if err != nil {
		log.Printf("[parent] disconnected from parent %s model %q: %v", pc.Base, pc.Model, err)
	}
	pc.srv.events.emit(topicParents)
}

// ── Serving parent requests ──────────────────────────────────────────────────

func (pc *ParentConn) handleEvent(ctx context.Context, evt *tunnelEvent) {
	ctx, cancel := context.WithTimeout(ctx, parentEventTimeout)
	defer cancel()

	switch evt.Kind {
	case "chat":
		pc.handleChatEvent(ctx, evt)
	case "embed":
		pc.handleEmbedEvent(ctx, evt)
	default:
		pc.postError(ctx, evt.ID, "unknown event kind: "+evt.Kind)
	}
}

func (pc *ParentConn) handleChatEvent(ctx context.Context, evt *tunnelEvent) {
	var req client.ChatRequest
	if err := json.Unmarshal(evt.Body, &req); err != nil {
		pc.postError(ctx, evt.ID, "invalid chat request: "+err.Error())
		return
	}
	pc.rewriteModel(&req.Model)

	if evt.Stream {
		pc.postStream(ctx, evt.ID, func(write func(string) bool) error {
			return pc.srv.execChatStream(ctx, &req, func(sse string) bool {
				chunk := strings.TrimSpace(strings.TrimPrefix(sse, "data:"))
				if chunk == "" || chunk == "[DONE]" {
					return true // parent appends its own [DONE] terminator
				}
				return write(chunk)
			})
		})
		return
	}

	resp, err := pc.srv.execChat(ctx, &req)
	if err != nil {
		pc.postError(ctx, evt.ID, err.Error())
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		pc.postError(ctx, evt.ID, "encode response: "+err.Error())
		return
	}
	if err := pc.postBody(ctx, evt.ID, body); err != nil {
		pc.markDisconnected(err)
	}
}

func (pc *ParentConn) handleEmbedEvent(ctx context.Context, evt *tunnelEvent) {
	var raw struct {
		Model string      `json:"model"`
		Input interface{} `json:"input"`
	}
	if err := json.Unmarshal(evt.Body, &raw); err != nil {
		pc.postError(ctx, evt.ID, "invalid embed request: "+err.Error())
		return
	}
	pc.rewriteModel(&raw.Model)

	embeddings, err := pc.srv.execEmbedding(ctx, raw.Model, raw.Input)
	if err != nil {
		pc.postError(ctx, evt.ID, err.Error())
		return
	}

	// The parent expects OpenAI format over the tunnel.
	data := make([]client.EmbeddingData, len(embeddings))
	for i, e := range embeddings {
		data[i] = client.EmbeddingData{Index: i, Object: "embedding", Embedding: e}
	}
	body, err := json.Marshal(client.EmbeddingResponse{
		Object: "list",
		Model:  raw.Model,
		Data:   data,
	})
	if err != nil {
		pc.postError(ctx, evt.ID, "encode response: "+err.Error())
		return
	}
	if err := pc.postBody(ctx, evt.ID, body); err != nil {
		pc.markDisconnected(err)
	}
}

// rewriteModel applies the optional ?to=<local model> alias from the -parent URL.
func (pc *ParentConn) rewriteModel(model *string) {
	if pc.ToModel != "" {
		*model = pc.ToModel
	}
}


// ── Posting results back to the parent ──────────────────────────────────────

// postBody delivers a single-shot response envelope to the parent.
func (pc *ParentConn) postBody(ctx context.Context, id string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pc.Base+"/admin/parent/response/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if pc.Token != "" {
		req.Header.Set("Authorization", "Bearer "+pc.Token)
	}

	resp, err := pc.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("parent response HTTP %d", resp.StatusCode)
	}
	return nil
}

func (pc *ParentConn) postError(ctx context.Context, id, msg string) {
	log.Printf("[parent] serving tunneled request %s failed: %s", id, msg)
	env, _ := json.Marshal(map[string]string{"error": msg})
	pc.postBody(ctx, id, env) // best effort
}

// postStream streams NDJSON response lines to the parent. produce writes
// lines via the write callback; a trailing [DONE] or error envelope is
// appended automatically.
func (pc *ParentConn) postStream(ctx context.Context, id string, produce func(write func(line string) bool) error) {
	pr, pw := io.Pipe()
	result := make(chan error, 1)

	go func() {
		defer close(result)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			pc.Base+"/admin/parent/response/"+id+"?stream=true", pr)
		if err != nil {
			result <- err
			return
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		if pc.Token != "" {
			req.Header.Set("Authorization", "Bearer "+pc.Token)
		}
		resp, err := pc.http.Do(req)
		if err != nil {
			pw.CloseWithError(err)
			result <- err
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode/100 != 2 {
			result <- fmt.Errorf("parent response HTTP %d", resp.StatusCode)
			return
		}
		result <- nil
	}()

	write := func(line string) bool {
		_, err := pw.Write([]byte(line + "\n"))
		return err == nil
	}

	err := produce(write)
	if err != nil {
		env, _ := json.Marshal(map[string]string{"error": err.Error()})
		pw.Write(append(env, '\n'))
	} else {
		pw.Write([]byte("[DONE]\n"))
	}
	pw.Close()

	if perr := <-result; perr != nil {
		log.Printf("[parent] posting streamed response for request %s failed: %v", id, perr)
		pc.markDisconnected(perr)
	}
}
