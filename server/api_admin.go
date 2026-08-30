package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/client"
)

// Admin API for registering/unregistering upstream LLM servers and binding
// their models to this proxy's mock models (see the -name flag).
//
// GET    /admin/backends          - list all registered backends
// POST   /admin/backends          - register a backend (one model binding per call)
// DELETE /admin/backends/<id>     - unregister a backend
// POST   /admin/backends/<id>/bindings  - add another model binding to a backend
// POST   /admin/backends/<id>/enabled   - enable/disable a backend {"enabled": bool}
// GET    /admin/models            - list mock models with their bindings
// GET    /admin/queue             - requests waiting for a backend

// registerRequest is the admin body for registering an upstream server.
type registerRequest struct {
	Type     string `json:"type"`     // "ollama" | "openai"
	Endpoint string `json:"endpoint"` // e.g. http://host:11434
	Token    string `json:"token"`    // optional auth token (ollama or openai)

	Model         string `json:"model"`          // mock model in this proxy (must be in -name list)
	UpstreamModel string `json:"upstream_model"` // actual model name upstream (defaults to model)

	// Capabilities advertised for this backend's models via /api/show,
	// e.g. ["completion", "vision", "tools", "thinking", "embedding"].
	// Empty means server.DefaultCapabilities.
	Capabilities []string `json:"capabilities"`
}

// handleAdminBackends serves GET/POST /admin/backends.
func (s *Server) handleAdminBackends(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listBackends(w)
	case http.MethodPost:
		s.registerBackend(w, r)
	default:
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleAdminBackendDetail serves /admin/backends/<id>[/bindings].
func (s *Server) handleAdminBackendDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/backends/")
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "missing backend id"})
		return
	}

	switch {
	case sub == "" && r.Method == http.MethodDelete:
		log.Printf("[admin] unregister backend %q requested", id)
		if s.router.RemoveBackend(id) {
			respondJSON(w, http.StatusOK, map[string]string{"status": "unregistered", "id": id})
		} else {
			respondJSON(w, http.StatusNotFound, map[string]string{"error": "backend not found"})
		}
	case sub == "bindings" && r.Method == http.MethodPost:
		s.bindModel(w, r, id)
	case sub == "enabled" && r.Method == http.MethodPost:
		s.setBackendEnabled(w, r, id)
	default:
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// setBackendEnabled enables or disables a backend (including connected child
// cllama tunnel backends) without unregistering it.
func (s *Server) setBackendEnabled(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if !s.router.SetBackendDisabled(id, !body.Enabled) {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "backend not found"})
		return
	}
	for _, be := range s.router.Backends() {
		if be.ID == id {
			respondJSON(w, http.StatusOK, backendView(be))
			return
		}
	}
}

// ── Queue inspection ──────────────────────────────────────────────────────

// handleAdminQueue lists the requests currently waiting because no backend
// serves their model (see Router.Acquire).
func (s *Server) handleAdminQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	items, capacity := s.router.QueueStatus()
	requests := make([]map[string]interface{}, 0, len(items))
	for _, q := range items {
		requests = append(requests, map[string]interface{}{
			"id":      q.ID,
			"model":   q.Model,
			"wait_ms": time.Since(q.At).Milliseconds(),
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"requests": requests,
		"queued":   len(requests),
		"capacity": capacity,
	})
}

// ── List backends ────────────────────────────────────────────────────────────

func (s *Server) listBackends(w http.ResponseWriter) {
	items := make([]map[string]interface{}, 0)
	for _, be := range s.router.Backends() {
		items = append(items, backendView(be))
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"backends": items,
		"count":    len(items),
	})
}

// ── Register backend ─────────────────────────────────────────────────────────

func (s *Server) registerBackend(w http.ResponseWriter, r *http.Request) {
	var body registerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[admin] register rejected: invalid JSON body from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	switch client.BackendType(body.Type) {
	case client.BackendOllama, client.BackendOpenAI:
	default:
		log.Printf("[admin] register rejected: invalid type %q", body.Type)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "type must be 'ollama' or 'openai'"})
		return
	}
	if body.Endpoint == "" {
		log.Printf("[admin] register rejected: missing endpoint")
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "endpoint is required"})
		return
	}
	if body.Model == "" {
		log.Printf("[admin] register rejected: missing model")
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if !s.router.KnownModel(body.Model) {
		log.Printf("[admin] register rejected: model %q not in this server's model list %v", body.Model, s.router.Models())
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "model is not in this server's model list",
			"models": strings.Join(s.router.Models(), ","),
		})
		return
	}
	upstream := body.UpstreamModel
	if upstream == "" {
		upstream = body.Model
	}

	id := generateID()
	entry := &BackendEntry{
		ID:           id,
		Type:         client.BackendType(body.Type),
		Endpoint:     body.Endpoint,
		Token:        body.Token,
		Bindings:     map[string]string{body.Model: upstream},
		Capabilities: body.Capabilities,
		client:       s.buildClient(client.BackendType(body.Type), body.Endpoint, body.Token),
	}
	if len(entry.Capabilities) == 0 {
		entry.Capabilities = DefaultCapabilities
	}
	s.router.AddBackend(entry)

	respondJSON(w, http.StatusCreated, backendView(entry))
}

// ── Bind another model ───────────────────────────────────────────────────────

func (s *Server) bindModel(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Model         string `json:"model"`
		UpstreamModel string `json:"upstream_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[admin] bind rejected: invalid JSON body from %s", r.RemoteAddr)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if !s.router.KnownModel(body.Model) {
		log.Printf("[admin] bind rejected: model %q not in this server's model list %v", body.Model, s.router.Models())
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "model is not in this server's model list",
			"models": strings.Join(s.router.Models(), ","),
		})
		return
	}
	upstream := body.UpstreamModel
	if upstream == "" {
		upstream = body.Model
	}
	if !s.router.Bind(id, body.Model, upstream) {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "backend not found"})
		return
	}

	for _, be := range s.router.Backends() {
		if be.ID == id {
			respondJSON(w, http.StatusOK, backendView(be))
			return
		}
	}
}

// ── Admin model list ─────────────────────────────────────────────────────────

// handleAdminModels lists each mock model and the backends bound to it.
func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	backends := s.router.Backends()
	models := make([]map[string]interface{}, 0)
	for _, name := range s.router.Models() {
		bound := make([]map[string]interface{}, 0)
		for _, be := range backends {
			if upstream, ok := be.UpstreamModel(name); ok {
				bound = append(bound, map[string]interface{}{
					"id":             be.ID,
					"type":           be.Type,
					"endpoint":       be.Endpoint,
					"upstream_model": upstream,
					"disabled":       be.Disabled,
				})
			}
		}
		models = append(models, map[string]interface{}{
			"name":     name,
			"backends": bound,
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"models": models})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// backendView renders a backend for admin output; the token itself is never
// exposed, only whether one is configured.
func backendView(be *BackendEntry) map[string]interface{} {
	bindings := make([]map[string]string, 0, len(be.Bindings))
	for mock, upstream := range be.Bindings {
		bindings = append(bindings, map[string]string{
			"model":          mock,
			"upstream_model": upstream,
		})
	}
	return map[string]interface{}{
		"id":           be.ID,
		"type":         be.Type,
		"endpoint":     be.Endpoint,
		"has_token":    be.Token != "",
		"bindings":     bindings,
		"capabilities": be.Capabilities,
		"healthy":      be.Healthy(),
		"disabled":     be.Disabled,
	}
}

// buildClient constructs the upstream client, applying the server's
// -gen-timeout (0 = no timeout).
func (s *Server) buildClient(t client.BackendType, endpoint, token string) client.Client {
	switch t {
	case client.BackendOllama:
		return client.NewOllamaClient(endpoint, token, s.genTimeout)
	case client.BackendOpenAI:
		return client.NewOpenAIClient(endpoint, token, s.genTimeout)
	default:
		return nil
	}
}

func generateID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "be_" + time.Now().Format("20060102150405000")
	}
	return "be_" + hex.EncodeToString(b)
}
