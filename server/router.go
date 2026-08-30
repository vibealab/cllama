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

	// queued tracks the requests currently in the request queue (pending,
	// takeaway, processing or failed) so the admin API and UI can show
	// their lifecycle state.
	queueMu sync.Mutex
	queued  map[string]*queuedRequest

	// stopCh shuts down the done/failed retention sweeper.
	stopCh  chan struct{}
	stopOne sync.Once
}

// RequestHandle is a token for one LLM request tracked in the router's
// request queue. Callers advance the request through its lifecycle:
// MarkProcessing when the backend call starts, then Done on success or Fail
// on failure. All methods are safe to call on a nil handle.
type RequestHandle struct {
	router *Router
	entry  *queuedRequest
}

// RequestState is the lifecycle state of an LLM request tracked in the
// router's request queue.
type RequestState string

const (
	// StatePending: the request is enqueued and the backend selector is
	// looking for a backend to serve it.
	StatePending RequestState = "pending"
	// StateTakeaway: a backend has been assigned ("taken away" for serving)
	// but the upstream call has not started yet. In the future this state
	// can also be set manually by an admin to pin a request so the selector
	// never routes it and it can be operated on by hand.
	StateTakeaway RequestState = "takeaway"
	// StateProcessing: a backend is actively serving the request.
	StateProcessing RequestState = "processing"
	// StateDone: the backend answered successfully; the entry lingers in
	// the queue per DoneRequestRetention so it can be reviewed.
	StateDone RequestState = "done"
	// StateFail: the backend request failed; the entry lingers in the
	// queue per FailedRequestRetention before being purged.
	StateFail RequestState = "fail"
)

// DoneRequestRetention is how long a successfully completed request stays
// visible in the request queue (in the "done" state, for review) before
// being purged. It is fixed at compile time:
//
//	-1  keep done requests in the queue forever; an admin removes them
//	    manually via DELETE /admin/queue/<id>
//	 0  remove a request from the queue as soon as it completes
//	>0 keep done requests in the queue for this long, then purge them
const DoneRequestRetention = 5 * time.Minute

// FailedRequestRetention is how long a failed request stays visible in the
// request queue (in the "fail" state) before being purged. It is fixed at
// compile time:
//
//	-1  keep failed requests in the queue forever; an admin removes them
//	    manually via DELETE /admin/queue/<id>
//	 0  remove a request from the queue as soon as it fails
//	>0 keep failed requests in the queue for this long, then purge them
const FailedRequestRetention = 5 * time.Minute

// failedSweepInterval is how often expired done/failed requests are purged.
const failedSweepInterval = time.Second

// queuedRequest is one LLM request tracked in the request queue.
type queuedRequest struct {
	ID    string
	Model string
	State RequestState
	At    time.Time
	// AssignedTo is the backend the selector took the request for
	// (meaningful from StateTakeaway onwards).
	AssignedTo string
	// Error records why State == StateFail.
	Error string
	// EndedAt is when the request reached a terminal state (done or
	// fail); it drives retention expiry.
	EndedAt time.Time
}

// terminalRetention returns how long a request in the given terminal state
// lingers in the queue before being purged (-1 forever, 0 remove immediately,
// >0 keep that long); non-terminal states do not linger.
func terminalRetention(state RequestState) time.Duration {
	switch state {
	case StateDone:
		return DoneRequestRetention
	case StateFail:
		return FailedRequestRetention
	default:
		return 0
	}
}

// NewRouter creates a router exposing the given mock models with the given
// maximum queue depth. events, when non-nil, receives change topics for
// the web UI.
func NewRouter(maxQueue int, models []string, events *eventHub) *Router {
	r := &Router{
		models: models,
		events: events,
		queued: make(map[string]*queuedRequest),
		stopCh: make(chan struct{}),
	}
	if maxQueue > 0 {
		r.queueSlots = make(chan struct{}, maxQueue)
	}
	r.startRetentionSweeper()
	return r
}

// startRetentionSweeper launches the background purge of expired done and
// failed requests (a no-op unless either retention is a positive duration).
func (r *Router) startRetentionSweeper() {
	if FailedRequestRetention <= 0 && DoneRequestRetention <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(failedSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case now := <-ticker.C:
				var changed bool
				r.queueMu.Lock()
				for id, q := range r.queued {
					ret := terminalRetention(q.State)
					if ret > 0 && now.Sub(q.EndedAt) >= ret {
						delete(r.queued, id)
						changed = true
					}
				}
				r.queueMu.Unlock()
				if changed {
					r.events.emit(topicQueue)
				}
			}
		}
	}()
}

// QueueStatus returns the requests currently tracked in the queue (newest
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

// Acquire returns a backend for the given mock model and enqueues the
// request in the router's request queue for its whole lifecycle. The request
// starts as "pending" while the backend selector scans the registry; once a
// backend is picked it becomes "takeaway" and the returned handle is handed
// to the caller, which marks it "processing" and finally Done or Fail — both
// terminal states linger in the queue for their retention (see
// DoneRequestRetention and FailedRequestRetention) for review before being
// purged or removed by hand. When no backend is available the call waits
// (holding a queue slot) until one is registered, the context is cancelled,
// or the queue is full. On error the handle is nil and nothing remains
// queued.
func (r *Router) Acquire(ctx context.Context, mockModel string) (*BackendEntry, string, *RequestHandle, error) {
	// Every request enters the queue as soon as it arrives.
	entry := &queuedRequest{ID: generateID(), Model: mockModel, State: StatePending, At: time.Now()}
	r.queueMu.Lock()
	r.queued[entry.ID] = entry
	r.queueMu.Unlock()
	r.events.emit(topicQueue)
	h := &RequestHandle{router: r, entry: entry}

	// take marks the request as taken away for the given backend.
	take := func(be *BackendEntry) {
		r.transition(entry, func(q *queuedRequest) {
			q.State = StateTakeaway
			q.AssignedTo = be.ID
		})
	}

	if be, up, ok := r.NextBackendForModel(mockModel); ok {
		take(be)
		return be, up, h, nil
	}
	if r.queueSlots == nil { // queueing disabled
		r.release(entry)
		log.Printf("[route] model %q: no backend available and queueing disabled", mockModel)
		return nil, "", nil, ErrQueueFull
	}

	// Take a queue slot or reject immediately when the queue is full.
	select {
	case r.queueSlots <- struct{}{}:
		defer func() { <-r.queueSlots }()
	default:
		r.release(entry)
		log.Printf("[route] model %q: no backend available and request queue is full", mockModel)
		return nil, "", nil, ErrQueueFull
	}

	log.Printf("[route] model %q: no backend available, request %s queued", mockModel, entry.ID)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.release(entry)
			return nil, "", nil, ctx.Err()
		case <-ticker.C:
			if be, up, ok := r.NextBackendForModel(mockModel); ok {
				take(be)
				return be, up, h, nil
			}
		}
	}
}

// transition applies a state change to a still-queued request and notifies
// subscribers. Entries already removed from the queue are left untouched.
func (r *Router) transition(entry *queuedRequest, mutate func(*queuedRequest)) {
	r.queueMu.Lock()
	_, live := r.queued[entry.ID]
	if live {
		mutate(entry)
	}
	r.queueMu.Unlock()
	if live {
		r.events.emit(topicQueue)
	}
}

// release removes a request from the queue entirely.
func (r *Router) release(entry *queuedRequest) {
	r.queueMu.Lock()
	_, live := r.queued[entry.ID]
	delete(r.queued, entry.ID)
	r.queueMu.Unlock()
	if live {
		r.events.emit(topicQueue)
	}
}

// MarkProcessing records that the assigned backend has started serving the
// request.
func (h *RequestHandle) MarkProcessing() {
	if h == nil {
		return
	}
	h.router.transition(h.entry, func(q *queuedRequest) { q.State = StateProcessing })
}

// Done records that the backend answered successfully. Like Fail, the
// request is not dropped silently: it moves to the "done" state and lingers
// in the queue for DoneRequestRetention (removed immediately when zero,
// kept forever when negative) so completed requests can be reviewed,
// then is purged or removed by hand via DELETE /admin/queue/<id>.
func (h *RequestHandle) Done() {
	if h == nil {
		return
	}
	if DoneRequestRetention == 0 {
		h.router.release(h.entry)
		return
	}
	h.router.transition(h.entry, func(q *queuedRequest) {
		q.State = StateDone
		q.EndedAt = time.Now()
	})
}

// Fail marks the request as failed. It then lingers in the queue for
// FailedRequestRetention (forever when negative, purged immediately when
// zero) so the admin API and UI can show what went wrong.
func (h *RequestHandle) Fail(cause error) {
	if h == nil {
		return
	}
	if FailedRequestRetention == 0 {
		h.router.release(h.entry)
		return
	}
	msg := "backend request failed"
	if cause != nil {
		msg = cause.Error()
	}
	h.router.transition(h.entry, func(q *queuedRequest) {
		q.State = StateFail
		q.Error = msg
		q.EndedAt = time.Now()
	})
}

// RemoveQueued drops a request from the queue regardless of its state
// (manual admin action, e.g. purging a failed request kept forever);
// it reports whether one was found.
func (r *Router) RemoveQueued(id string) bool {
	r.queueMu.Lock()
	_, ok := r.queued[id]
	delete(r.queued, id)
	r.queueMu.Unlock()
	if ok {
		r.events.emit(topicQueue)
	}
	return ok
}

// Stop releases router resources.
func (r *Router) Stop() {
	r.stopOne.Do(func() { close(r.stopCh) })
}

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
