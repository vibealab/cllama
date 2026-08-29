package server

// Admin API for the parent/child tunnel. All routes are under /admin.
//
// Parent side (called by child cllama servers):
//
//	GET  /admin/parent/stream?model=<model>  - child's long-lived SSE channel;
//	                                           parent pushes request events here
//	POST /admin/parent/response/<id>         - child returns a single-shot response
//	POST /admin/parent/response/<id>?stream=true
//	                                       - child streams NDJSON response lines
//
// Child side (manage outbound parent connections):
//
//	GET    /admin/parents       - list parent connections and their status
//	POST   /admin/parents       - add a parent, body {"url": "http://[token@]host:port/model"}
//	DELETE /admin/parents/<id>  - remove a parent connection

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/client"
)

const tunnelPingInterval = 20 * time.Second

// checkParentAuth enforces the -parentauth token on tunnel endpoints.
func (s *Server) checkParentAuth(r *http.Request) bool {
	if s.parentAuth == "" {
		return true
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.parentAuth {
		return true
	}
	log.Printf("[tunnel] %s %s from %s rejected: invalid or missing parent auth", r.Method, r.URL.Path, r.RemoteAddr)
	return false
}

// ── Parent side ──────────────────────────────────────────────────────────────

// handleTunnelStream serves a child's SSE connection. While connected, the
// child is registered as a tunnel backend for the requested model and
// participates in round-robin routing like any other backend.
func (s *Server) handleTunnelStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.checkParentAuth(r) {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing token"})
		return
	}
	model := r.URL.Query().Get("model")
	if !s.router.KnownModel(model) {
		log.Printf("[tunnel] child %s requested unknown model %q", r.RemoteAddr, model)
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "model is not in this server's model list",
			"models": strings.Join(s.router.Models(), ","),
		})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	tc := newTunnelClient(s.calls)
	backendID := generateID()
	s.router.AddBackend(&BackendEntry{
		ID:       backendID,
		Type:     client.BackendTunnel,
		Endpoint: "tunnel://" + r.RemoteAddr,
		Bindings: map[string]string{model: model},
		client:   tc,
	})
	defer func() {
		s.router.RemoveBackend(backendID)
		tc.kill("parent connection closed")
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": cllama tunnel connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(tunnelPingInterval)
	defer ping.Stop()

	for {
		select {
		case evt := <-tc.out:
			fmt.Fprint(w, "data: "+evt+"\n\n")
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleTunnelResponse accepts a child's response for a tunneled request.
func (s *Server) handleTunnelResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.checkParentAuth(r) {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing token"})
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/admin/parent/response/")
	call := s.calls.get(id)
	if id == "" || call == nil {
		log.Printf("[tunnel] response from %s for unknown or already completed request id %q", r.RemoteAddr, id)
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "unknown or already completed request id"})
		return
	}
	defer r.Body.Close()

	if r.URL.Query().Has("stream") {
		scanner := bufio.NewScanner(r.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if !s.calls.feed(call, line) {
				break // consumer gave up
			}
			if line == "[DONE]" || envelopeError(line) != "" {
				break
			}
		}
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024*1024))
		if err != nil {
			log.Printf("[tunnel] failed to read single-shot response for request %q: %v", id, err)
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
			return
		}
		s.calls.feed(call, string(body))
	}

	call.finishCall()
	s.calls.remove(id)
	respondJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
}

// ── Child side: parent connection management ─────────────────────────────────

// handleAdminParents serves GET/POST /admin/parents.
func (s *Server) handleAdminParents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"parents": s.parents.List(),
		})
	case http.MethodPost:
		s.addParent(w, r)
	default:
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) addParent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	pc, err := s.parents.Add(body.URL)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	view, _ := s.parents.Get(pc.ID)
	respondJSON(w, http.StatusCreated, view)
}

// handleAdminParentDetail serves DELETE /admin/parents/<id>.
func (s *Server) handleAdminParentDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/parents/")
	if id == "" || strings.Contains(id, "/") {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "missing parent id"})
		return
	}
	if r.Method != http.MethodDelete {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.parents.Remove(id) {
		respondJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": id})
	} else {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "parent not found"})
	}
}
