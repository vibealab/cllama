package server

// Server-sent events for the web UI.
//
// The UI subscribes to GET /admin/events and receives a topic line whenever
// relevant state changes (queue depth, backend registry, parent tunnels).
// Events carry no payload: the client simply refetches the matching admin
// endpoint, which keeps this hub trivial (topics only, slow clients dropped).

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Change topics broadcast to UI subscribers.
const (
	topicQueue    = "queue"
	topicBackends = "backends"
	topicParents  = "parents"
)

// uiPingInterval keeps intermediaries from closing idle SSE connections.
const uiPingInterval = 15 * time.Second

// eventHub fans change topics out to SSE subscribers.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan string]struct{})}
}

// subscribe registers a subscriber; the returned unsubscribe func must be
// called when done. Each subscriber has a small buffer; while its buffer is
// full further events are dropped (subscribers refetch full state anyway).
func (h *eventHub) subscribe() (<-chan string, func()) {
	ch := make(chan string, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// emit broadcasts a topic to all subscribers without blocking. Safe on a nil
// hub so callers can run before an event hub is attached.
func (h *eventHub) emit(topic string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- topic:
		default: // slow consumer: drop; it refetches on the next event
		}
	}
}

// handleAdminEvents serves GET /admin/events as a Server-Sent Events stream
// of change topics for the web UI.
func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	events, unsubscribe := s.events.subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// "hello" tells the client to do its initial full refresh.
	fmt.Fprint(w, "data: hello\n\n")
	flusher.Flush()

	ping := time.NewTicker(uiPingInterval)
	defer ping.Stop()
	for {
		select {
		case topic, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", topic)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
