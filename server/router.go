package server

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cllama/client"
)

// Errors returned by the router.
var (
	// ErrQueueFull is returned when no backend serves the requested model
	// and the request queue is at capacity.
	ErrQueueFull = errors.New("no backend available and request queue is full")
)

// BackendEntry is a registered upstream LLM server.
//
// A single upstream server can serve several mock models; each entry keeps a
// binding map from the proxy's mock model name (from -name) to the actual
// model name on the upstream server.
type BackendEntry struct {
	ID       string
	Type     client.BackendType
	Endpoint string
	Token    string            // auth token (secret; never exposed by admin APIs)
	Bindings map[string]string // mock model name -> upstream model name
	LastUsed int64
	// Disabled backends stay registered but take no traffic until
	// re-enabled (managed via POST /admin/backends/<id>/enabled).
	Disabled bool
	// Capabilities advertised to clients via /api/show (e.g. "vision",
	// "thinking"); empty means DefaultCapabilities.
	Capabilities []string

	client client.Client
}

// DefaultCapabilities is advertised for a model whose backends do not
// declare their own. Note: including "embedding" makes the ollama CLI treat
// the model as an embedding-only model and refuse interactive chat.
var DefaultCapabilities = []string{"completion", "vision", "tools", "thinking"}

// Healthy reports whether the backend can serve at least one mock model.
func (e *BackendEntry) Healthy() bool { return len(e.Bindings) > 0 }

// UpstreamModel returns the actual model name bound to a mock model.
func (e *BackendEntry) UpstreamModel(mockModel string) (string, bool) {
	m, ok := e.Bindings[mockModel]
	return m, ok
}

// Router maintains the backend registry, routes requests with round-robin
// load balancing per mock model, and holds requests in a queue when no
// backend is available.
type Router struct {
	mu       sync.RWMutex
	backends []*BackendEntry
	rr       atomic.Int64 // round-robin counter

	models []string // mock model names exposed by this proxy

	queueSlots chan struct{} // semaphore limiting concurrently queued requests

	// events broadcasts state changes to the web UI (may be nil in tests).
	events *eventHub

	// queued tracks the requests currently waiting for a backend so the
	// admin API and UI can show what is in the queue.
	queueMu sync.Mutex
	queued  map[string]*queuedRequest
}

// queuedRequest is one request waiting in the no-backend queue.
type queuedRequest struct {
	ID    string
	Model string
	At    time.Time
}

// NewRouter creates a router exposing the given mock models with the given
// maximum queue depth. events, when non-nil, receives change topics for
// the web UI.
func NewRouter(maxQueue int, models []string, events *eventHub) *Router {
	r := &Router{
		models: models,
		events: events,
		queued: make(map[string]*queuedRequest),
	}
	if maxQueue > 0 {
		r.queueSlots = make(chan struct{}, maxQueue)
	}
	return r
}

// QueueStatus returns the requests currently waiting for a backend (newest
// first) and the queue capacity (0 means queueing is disabled).
func (r *Router) QueueStatus() ([]queuedRequest, int) {
	r.queueMu.Lock()
	out := make([]queuedRequest, 0, len(r.queued))
	for _, q := range r.queued {
		out = append(out, *q)
	}
	r.queueMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })

	capacity := 0
	if r.queueSlots != nil {
		capacity = cap(r.queueSlots)
	}
	return out, capacity
}

// Models returns the mock model names this proxy exposes.
func (r *Router) Models() []string {
	out := make([]string, len(r.models))
	copy(out, r.models)
	return out
}

// CapabilitiesForModel returns the capabilities to advertise for a mock
// model: the intersection of the capabilities of every backend bound to it,
// so a feature is only promised when all round-robin peers can serve it.
// Returns DefaultCapabilities when no bound backend declares any.
func (r *Router) CapabilitiesForModel(mockModel string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []string
	for _, be := range r.backends {
		if _, ok := be.UpstreamModel(mockModel); !ok {
			continue
		}
		caps := be.Capabilities
		if len(caps) == 0 {
			caps = DefaultCapabilities
		}
		if result == nil {
			result = append(result, caps...)
			continue
		}
		next := make([]string, 0, len(result))
		for _, c := range result {
			for _, o := range caps {
				if c == o {
					next = append(next, c)
					break
				}
			}
		}
		result = next
	}
	if len(result) == 0 {
		return DefaultCapabilities
	}
	return result
}

// KnownModel reports whether name is one of the proxy's mock models.
func (r *Router) KnownModel(name string) bool {
	for _, m := range r.models {
		if m == name {
			return true
		}
	}
	return false
}

// AddBackend registers a new backend.
func (r *Router) AddBackend(be *BackendEntry) {
	r.mu.Lock()
	r.backends = append(r.backends, be)
	log.Printf("[backend] registered %s type=%s endpoint=%s bindings=%s (%d backends total)",
		be.ID, be.Type, be.Endpoint, formatBindings(be.Bindings), len(r.backends))
	r.mu.Unlock()
	r.events.emit(topicBackends)
}

// RemoveBackend unregisters a backend by ID; it reports whether one was found.
func (r *Router) RemoveBackend(id string) bool {
	r.mu.Lock()
	for i, be := range r.backends {
		if be.ID == id {
			r.backends = append(r.backends[:i], r.backends[i+1:]...)
			log.Printf("[backend] unregistered %s type=%s endpoint=%s (%d backends remain)",
				be.ID, be.Type, be.Endpoint, len(r.backends))
			r.mu.Unlock()
			r.events.emit(topicBackends)
			return true
		}
	}
	log.Printf("[backend] unregister failed: %q not found", id)
	r.mu.Unlock()
	return false
}

// SetBackendDisabled enables or disables a backend without unregistering
// it; it reports whether the backend was found.
func (r *Router) SetBackendDisabled(id string, disabled bool) bool {
	r.mu.Lock()
	for _, be := range r.backends {
		if be.ID == id {
			be.Disabled = disabled
			log.Printf("[backend] %s disabled=%t", id, disabled)
			r.mu.Unlock()
			r.events.emit(topicBackends)
			return true
		}
	}
	r.mu.Unlock()
	return false
}

// Bind adds a mock-model -> upstream-model binding to an existing backend.
func (r *Router) Bind(id, mockModel, upstreamModel string) bool {
	r.mu.Lock()
	for _, be := range r.backends {
		if be.ID == id {
			if be.Bindings == nil {
				be.Bindings = map[string]string{}
			}
			be.Bindings[mockModel] = upstreamModel
			log.Printf("[backend] %s bound model %q -> upstream %q", id, mockModel, upstreamModel)
			r.mu.Unlock()
			r.events.emit(topicBackends)
			return true
		}
	}
	log.Printf("[backend] bind failed: backend %q not found", id)
	r.mu.Unlock()
	return false
}

// Backends returns deep copies of the registered backends, safe for admin
// inspection while Bind/register mutate the live entries.
func (r *Router) Backends() []*BackendEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*BackendEntry, 0, len(r.backends))
	for _, be := range r.backends {
		cp := *be
		cp.Bindings = make(map[string]string, len(be.Bindings))
		for k, v := range be.Bindings {
			cp.Bindings[k] = v
		}
		out = append(out, &cp)
	}
	return out
}

// NextBackendForModel picks the next backend bound to the given mock model
// using round-robin. It returns the backend and the actual upstream model
// name the request should be rewritten to.
func (r *Router) NextBackendForModel(mockModel string) (*BackendEntry, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type match struct {
		be       *BackendEntry
		upstream string
	}
	matches := make([]match, 0, len(r.backends))
	for _, be := range r.backends {
		if be.Disabled {
			continue
		}
		if up, ok := be.UpstreamModel(mockModel); ok {
			matches = append(matches, match{be, up})
		}
	}
	if len(matches) == 0 {
		return nil, "", false
	}

	idx := int(r.rr.Add(1)) % len(matches)
	selected := matches[idx]
	selected.be.LastUsed = time.Now().UnixNano()
	return selected.be, selected.upstream, true
}

// Acquire returns a backend for the given mock model. When none is available,
// the call waits (holding a queue slot) until one is registered, the context
// is cancelled, or the queue is full.
func (r *Router) Acquire(ctx context.Context, mockModel string) (*BackendEntry, string, error) {
	if be, up, ok := r.NextBackendForModel(mockModel); ok {
		return be, up, nil
	}
	if r.queueSlots == nil { // queueing disabled
		log.Printf("[route] model %q: no backend available and queueing disabled", mockModel)
		return nil, "", ErrQueueFull
	}

	// Take a queue slot or reject immediately when the queue is full.
	select {
	case r.queueSlots <- struct{}{}:
		defer func() { <-r.queueSlots }()
	default:
		log.Printf("[route] model %q: no backend available and request queue is full", mockModel)
		return nil, "", ErrQueueFull
	}

	// Publish the request in the queue registry so the admin API and UI
	// can show what is waiting.
	entry := &queuedRequest{ID: generateID(), Model: mockModel, At: time.Now()}
	r.queueMu.Lock()
	r.queued[entry.ID] = entry
	r.queueMu.Unlock()
	r.events.emit(topicQueue)
	defer func() {
		r.queueMu.Lock()
		delete(r.queued, entry.ID)
		r.queueMu.Unlock()
		r.events.emit(topicQueue)
	}()

	log.Printf("[route] model %q: no backend available, request queued", mockModel)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-ticker.C:
			if be, up, ok := r.NextBackendForModel(mockModel); ok {
				return be, up, nil
			}
		}
	}
}

// Stop releases router resources.
func (r *Router) Stop() {}

// formatBindings renders a binding map for log output.
func formatBindings(b map[string]string) string {
	if len(b) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(b))
	for mock, upstream := range b {
		if mock == upstream {
			parts = append(parts, mock)
		} else {
			parts = append(parts, mock+"->"+upstream)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
