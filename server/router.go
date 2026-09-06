package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cllama/client"
	"cllama/config"
)

// Errors returned by the router.
var (
	// ErrQueueFull is returned when no backend serves the requested model
	// and the request queue is at capacity.
	ErrQueueFull = errors.New("no backend available and request queue is full")
	// ErrManualResolved is returned by Acquire when an admin answered a
	// manually taken-over request (see ResolveQueued): the caller completes
	// the client with the admin's answer instead of calling a backend. The
	// returned handle is non-nil and carries the answer (ManualAnswer).
	ErrManualResolved = errors.New("answered manually by an admin")
	// ErrQueuedNotFound reports an unknown request id in a queue operation.
	ErrQueuedNotFound = errors.New("queued request not found")
	// ErrNoChatPayload reports that a queued request stores no chat payload
	// to replay (e.g. embedding requests), so it cannot be proxied.
	ErrNoChatPayload = errors.New("queued request stores no chat payload")
	// ErrBackendNotFound reports an unknown backend id in a queue operation.
	ErrBackendNotFound = errors.New("backend not found")
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

	// cfg is the server's in-memory system configuration (queue depth,
	// done/fail retention); it is read live, so admin updates apply
	// without restarts.
	cfg *config.Store

	// waiting counts requests currently holding a no-backend queue slot
	// (guarded by queueMu); the capacity comes from cfg.MaxQueue.
	waiting int

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

	// sweepCh asks the retention sweeper to scan the queue immediately
	// (buffered size 1; a pending sweep is enough).
	sweepCh chan struct{}
}

// RequestHandle is a token for one LLM request tracked in the router's
// request queue. Callers advance the request through its lifecycle:
// MarkProcessing when the backend call starts, then Done on success or Fail
// on failure. All methods are safe to call on a nil handle.
type RequestHandle struct {
	router *Router
	entry  *queuedRequest
	ctx    context.Context
}

// Context returns the call context for this request. It derives from the
// incoming request context and is additionally cancelled when an admin
// removes the request from the queue, so upstream backend calls must use it
// instead of the caller's original context.
func (h *RequestHandle) Context() context.Context {
	if h == nil || h.ctx == nil {
		return context.Background()
	}
	return h.ctx
}

// RequestState is the lifecycle state of an LLM request tracked in the
// router's request queue.
type RequestState string

const (
	// StatePending: the request is enqueued and the backend selector is
	// looking for a backend to serve it.
	StatePending RequestState = "pending"
	// StateTakeaway: a backend has been assigned ("taken away" for serving)
	// but the upstream call has not started yet. An admin can also set this
	// state manually (POST /admin/queue/<id>/takeover) to pin a pending
	// request so the selector never routes it: the request then waits for a
	// hand-written answer (resolve) or to be released back to pending.
	// Manually taken-over requests have no AssignedTo backend.
	StateTakeaway RequestState = "takeaway"
	// StateProcessing: a backend is actively serving the request.
	StateProcessing RequestState = "processing"
	// StateDone: the backend answered successfully; the entry lingers in
	// the queue per the configured DoneRetention so it can be reviewed.
	StateDone RequestState = "done"
	// StateFail: the backend request failed; the entry lingers in the
	// queue per the configured FailedRetention before being purged.
	StateFail RequestState = "fail"
)

// retentionSweepInterval is how often expired done/failed requests are
// purged.
const retentionSweepInterval = time.Second

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
	// cancel aborts the request's in-flight work (queue wait and upstream
	// generation). Set by Acquire; invoked when an admin removes the entry
	// from the queue (see RemoveQueued) or it is released. Calling it after
	// the request finished is a no-op. Guarded by queueMu once stored.
	cancel context.CancelFunc

	// req is the original chat payload, kept so an admin can proxy it to a
	// chosen backend while drafting a manual answer (ProxyQueuedToBackend).
	// Nil for non-chat requests (embeddings). Set once at Acquire; the
	// pointed-to value is only mutated by the handler after Acquire returns.
	req *client.ChatRequest
	// manual marks a request pinned by an admin for manual handling: the
	// selector must not route it while set. Guarded by queueMu.
	manual bool
	// manualCh delivers the admin's hand-written answer to the queue wait
	// loop in Acquire (buffered size 1 so ResolveQueued never blocks).
	manualCh chan string
	// manualAnswer holds the delivered answer once Acquire has returned
	// ErrManualResolved; read by the handler via RequestHandle.ManualAnswer.
	manualAnswer string
}

// terminalRetention returns how long a request in the given terminal state
// lingers in the queue before being purged, per the live configuration
// (config.RetentionForever never expires, 0 expires immediately, >0 keeps
// that long); non-terminal states do not linger.
func (r *Router) terminalRetention(state RequestState) time.Duration {
	cfg := r.cfg.Get()
	switch state {
	case StateDone:
		return cfg.DoneRetention
	case StateFail:
		return cfg.FailedRetention
	default:
		return 0
	}
}

// NewRouter creates a router exposing the given mock models, governed by
// the given in-memory configuration store (nil uses the defaults). events,
// when non-nil, receives change topics for the web UI.
func NewRouter(cfg *config.Store, models []string, events *eventHub) *Router {
	if cfg == nil {
		cfg = config.NewStore(config.Default())
	}
	r := &Router{
		models: models,
		cfg:    cfg,
		events: events,
		queued:  make(map[string]*queuedRequest),
		stopCh:  make(chan struct{}),
		sweepCh: make(chan struct{}, 1),
	}
	r.startRetentionSweeper()
	return r
}

// startRetentionSweeper launches the background purge of expired done and
// failed requests. It always runs because retentions live in the mutable
// configuration store.
func (r *Router) startRetentionSweeper() {
	go func() {
		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case <-r.sweepCh:
				r.sweepRetention(time.Now())
			case now := <-ticker.C:
				r.sweepRetention(now)
			}
		}
	}()
}

// sweepRetention purges terminal requests whose retention has expired. A
// retention of 0 (drop immediately) expires at once, which is what makes
// shrinking a retention (e.g. 60s -> 0) take effect on entries already in
// the queue; config.RetentionForever never expires. Requests still in
// flight (no EndedAt) are never touched.
func (r *Router) sweepRetention(now time.Time) {
	var changed bool
	r.queueMu.Lock()
	for id, q := range r.queued {
		if q.EndedAt.IsZero() {
			continue // not terminal yet: nothing to expire
		}
		ret := r.terminalRetention(q.State)
		if ret >= 0 && now.Sub(q.EndedAt) >= ret {
			delete(r.queued, id)
			changed = true
		}
	}
	r.queueMu.Unlock()
	if changed {
		r.events.emit(topicQueue)
	}
}

// TriggerRetentionSweep asks the retention sweeper to run an immediate
// scan, so a retention shrink (e.g. 60s -> 0) drops already-expired
// entries without waiting for the next ticker tick. Cheap to call: the
// pending-sweep signal is coalesced.
func (r *Router) TriggerRetentionSweep() {
	select {
	case r.sweepCh <- struct{}{}:
	default: // a sweep is already pending
	}
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

	capacity := r.cfg.Get().MaxQueue
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
// terminal states linger in the queue for their configured retention (see
// config.Config) for review before being purged or removed by hand. When no
// backend is available the call waits (holding a queue slot, up to the
// configured MaxQueue) until one is registered, the context is cancelled, or
// the queue is full. On any other error the handle is nil and nothing
// remains queued. The sole exception is ErrManualResolved: the admin
// answered the request by hand while it waited, the handle is non-nil and
// carries the answer (ManualAnswer) to return to the client.
//
// payload is the original chat request (nil for embeddings); it is stored on
// the queue entry so the admin UI can proxy it to a backend while drafting a
// manual answer.
func (r *Router) Acquire(ctx context.Context, mockModel string, payload *client.ChatRequest) (*BackendEntry, string, *RequestHandle, error) {
	// Every request enters the queue as soon as it arrives. callCtx derives
	// from the caller's context and is additionally cancelled when the entry
	// is removed from the queue, aborting queue waits and in-flight
	// generation (see RequestHandle.Context and RemoveQueued).
	callCtx, cancel := context.WithCancel(ctx)
	entry := &queuedRequest{
		ID:       generateID(),
		Model:    mockModel,
		State:    StatePending,
		At:       time.Now(),
		cancel:   cancel,
		req:      payload,
		manualCh: make(chan string, 1),
	}
	defer func() {
		// Safety net: no leak if the entry never stays queued (queue-full,
		// cancelled wait). When it is still live the later release, Done or
		// Fail path cancels callCtx instead.
		if !r.queuedLive(entry) {
			cancel()
		}
	}()
	r.queueMu.Lock()
	r.queued[entry.ID] = entry
	r.queueMu.Unlock()
	r.events.emit(topicQueue)
	h := &RequestHandle{router: r, entry: entry, ctx: callCtx}

	if be, up, ok := r.NextBackendForModel(mockModel); ok && r.takeIfOpen(entry, be) {
		return be, up, h, nil
	}
	// Take a queue wait slot or reject immediately when queueing is
	// disabled or the queue is full; the capacity is read from the live
	// configuration so admin updates apply to new admissions at once.
	r.queueMu.Lock()
	capacity := r.cfg.Get().MaxQueue
	if capacity <= 0 || r.waiting >= capacity {
		r.queueMu.Unlock()
		r.release(entry)
		if capacity <= 0 {
			log.Printf("[route] model %q: no backend available and queueing disabled", mockModel)
		} else {
			log.Printf("[route] model %q: no backend available and request queue is full", mockModel)
		}
		return nil, "", nil, ErrQueueFull
	}
	r.waiting++
	r.queueMu.Unlock()
	defer func() {
		r.queueMu.Lock()
		r.waiting--
		r.queueMu.Unlock()
	}()

	log.Printf("[route] model %q: no backend available, request %s queued", mockModel, entry.ID)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-callCtx.Done():
			r.release(entry)
			return nil, "", nil, callCtx.Err()
		case answer := <-entry.manualCh:
			// An admin answered this request by hand (see ResolveQueued):
			// hand the answer back to the caller instead of routing it.
			entry.manualAnswer = answer
			return nil, "", h, ErrManualResolved
		case <-ticker.C:
			if r.isManual(entry) {
				continue // pinned for manual handling: the selector must not route it
			}
			if be, up, ok := r.NextBackendForModel(mockModel); ok && r.takeIfOpen(entry, be) {
				return be, up, h, nil
			}
		}
	}
}

// takeIfOpen marks a request as taken away for the given backend and reports
// whether it succeeded. It refuses (returning false) when an admin pinned
// the request for manual handling in the meantime, so a concurrent takeover
// always wins over the selector.
func (r *Router) takeIfOpen(entry *queuedRequest, be *BackendEntry) bool {
	taken := false
	r.transition(entry, func(q *queuedRequest) {
		if q.manual {
			return
		}
		q.State = StateTakeaway
		q.AssignedTo = be.ID
		taken = true
	})
	return taken
}

// isManual reports whether a still-queued request is pinned for manual
// handling (see TakeoverQueued).
func (r *Router) isManual(entry *queuedRequest) bool {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	q, live := r.queued[entry.ID]
	return live && q.manual
}

// ManualAnswer returns the admin-authored answer attached to this handle
// when Acquire returned ErrManualResolved; empty otherwise.
func (h *RequestHandle) ManualAnswer() string {
	if h == nil {
		return ""
	}
	return h.entry.manualAnswer
}

// TakeoverQueued pins a pending request for manual handling: it moves to
// "takeaway" with no backend assigned and the selector stops routing it.
// The request then waits until an admin answers it (ResolveQueued) or hands
// it back to the selector (ReleaseQueued). Only pending requests can be
// taken over; it reports whether one was found.
func (r *Router) TakeoverQueued(id string) bool {
	var ok bool
	r.queueMu.Lock()
	if q, live := r.queued[id]; live && q.State == StatePending && !q.manual {
		q.manual = true
		q.State = StateTakeaway
		ok = true
	}
	r.queueMu.Unlock()
	if ok {
		log.Printf("[queue] request %s taken over for manual handling", id)
		r.events.emit(topicQueue)
	}
	return ok
}

// ReleaseQueued hands a manually taken-over request back to the selector:
// it becomes pending again and may be routed to a backend as usual. It
// reports whether a manually taken-over request was found.
func (r *Router) ReleaseQueued(id string) bool {
	var ok bool
	r.queueMu.Lock()
	if q, live := r.queued[id]; live && q.manual && q.State == StateTakeaway {
		q.manual = false
		q.State = StatePending
		ok = true
	}
	r.queueMu.Unlock()
	if ok {
		log.Printf("[queue] request %s released back to pending", id)
		r.events.emit(topicQueue)
	}
	return ok
}

// ResolveQueued delivers a hand-written answer to a manually taken-over
// request; the blocked client call completes with it (see ErrManualResolved).
// It reports whether the answer was delivered (the request is taken over and
// no answer was queued yet).
func (r *Router) ResolveQueued(id, answer string) bool {
	delivered := false
	r.queueMu.Lock()
	if q, live := r.queued[id]; live && q.manual && q.State == StateTakeaway {
		select {
		case q.manualCh <- answer:
			delivered = true
		default: // an answer was already delivered and not picked up yet
		}
	}
	r.queueMu.Unlock()
	if delivered {
		log.Printf("[queue] request %s manually answered (%d chars)", id, len(answer))
	}
	return delivered
}

// ProxyQueuedToBackend replays the original chat payload of a queued request
// against the chosen backend without touching the request's lifecycle: it is
// the drafting path for manual answers (the admin reviews the generated text
// before resolving the request). The request's mock model is rewritten to
// the backend's bound upstream model, falling back to the backend's first
// binding when the mock model is not bound there.
func (r *Router) ProxyQueuedToBackend(ctx context.Context, id, backendID string) (*client.ChatResponse, error) {
	r.queueMu.Lock()
	q := r.queued[id]
	r.queueMu.Unlock()
	if q == nil {
		return nil, ErrQueuedNotFound
	}
	if q.req == nil || len(q.req.Messages) == 0 {
		return nil, ErrNoChatPayload
	}
	var be *BackendEntry
	for _, b := range r.Backends() {
		if b.ID == backendID {
			be = b
			break
		}
	}
	if be == nil {
		return nil, ErrBackendNotFound
	}
	upstream, ok := be.UpstreamModel(q.Model)
	if !ok {
		// Deterministic fallback so a manual proxy works even for backends
		// not bound to the request's mock model.
		if names := make([]string, 0, len(be.Bindings)); len(be.Bindings) > 0 {
			for mock := range be.Bindings {
				names = append(names, mock)
			}
			sort.Strings(names)
			upstream = be.Bindings[names[0]]
		}
	}
	if upstream == "" {
		return nil, fmt.Errorf("backend %q has no model bindings", backendID)
	}

	// Shallow copy: only the copy's routing fields are rewritten, the
	// original stays untouched for the still-blocked client request.
	req := *q.req
	req.Model = upstream
	req.Stream = false
	if be.client == nil {
		return nil, fmt.Errorf("backend %q cannot serve chat requests", backendID)
	}
	return be.client.Chat(ctx, &req)
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

// release removes a request from the queue entirely. If the request was
// still in flight this also cancels its call context, aborting any queue
// wait or upstream generation still running for it.
func (r *Router) release(entry *queuedRequest) {
	r.queueMu.Lock()
	_, live := r.queued[entry.ID]
	delete(r.queued, entry.ID)
	r.queueMu.Unlock()
	if live {
		entry.cancelNow()
		r.events.emit(topicQueue)
	}
}

// queuedLive reports whether the entry is still tracked in the queue.
func (r *Router) queuedLive(entry *queuedRequest) bool {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	_, ok := r.queued[entry.ID]
	return ok
}

// cancelNow invokes the entry's cancel func; safe to call more than once
// and after the request finished.
func (q *queuedRequest) cancelNow() {
	if q.cancel != nil {
		q.cancel()
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
// request is not dropped silently: unless the configured DoneRetention is
// zero (leave the queue immediately, the default) it moves to the "done"
// state and lingers in the queue so completed requests can be reviewed —
// config.RetentionForever keeps it until an admin removes it, a positive
// retention purges it once expired via DELETE /admin/queue/<id).
func (h *RequestHandle) Done() {
	if h == nil {
		return
	}
	if h.router.cfg.Get().DoneRetention == 0 {
		h.router.release(h.entry)
		return
	}
	h.router.transition(h.entry, func(q *queuedRequest) {
		q.State = StateDone
		q.EndedAt = time.Now()
	})
	// Terminal: release the call context even though the entry lingers.
	h.entry.cancelNow()
}

// Fail marks the request as failed. It then lingers in the queue per the
// configured FailedRetention (forever at config.RetentionForever, dropped
// immediately at zero, kept that long when positive) so the admin API and
// UI can show what went wrong.
func (h *RequestHandle) Fail(cause error) {
	if h == nil {
		return
	}
	if h.router.cfg.Get().FailedRetention == 0 {
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
	// Terminal: release the call context even though the entry lingers.
	h.entry.cancelNow()
}

// RemoveQueued drops a request from the queue regardless of its state
// (manual admin action, e.g. purging a failed request kept forever);
// it reports whether one was found. Removing a request that is still
// pending or processing additionally cancels its call context, which
// aborts the queue wait or stops the in-flight upstream generation.
func (r *Router) RemoveQueued(id string) bool {
	r.queueMu.Lock()
	q, ok := r.queued[id]
	delete(r.queued, id)
	r.queueMu.Unlock()
	if ok {
		q.cancelNow()
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
