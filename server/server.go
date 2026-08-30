package server

import (
	"io/fs"
	"log"
	"net/http"
	"time"

	"cllama/config"
	"cllama/ui"
)

// Server holds the HTTP state for the cllama proxy.
type Server struct {
	// cfg is the in-memory system configuration, shared with the router
	// and managed via GET/PUT /admin/config.
	cfg *config.Store

	router *Router

	// events broadcasts state changes to the web UI over SSE.
	events *eventHub

	// uiFS serves the /ui/ static assets; defaults to the embedded copy
	// and can be replaced via UseUI (see the -debug-ui flag).
	uiFS fs.FS

	// Parent-side: pending tunneled requests from child cllama servers.
	calls *callRegistry

	// Child-side: outbound connections to parent cllama servers (-parent).
	parents *ParentManager

	// Reported as modified_at in the ollama /api/tags model list.
	startedAt time.Time
}

// New creates a new Server exposing the given mock model names.
// maxQueue caps how many requests are held when no backend is available.
// parentURLs (see -parent) connect this server as a child to parent cllama
// servers, in the form http://[token@]host:port/<parent-model>.
// parentAuth, when non-empty, is the token child cllama servers must present;
// apiToken, when non-empty, is the bearer token required on LLM API requests.
// Both seed the in-memory config and stay adjustable via PUT /admin/config.
// genTimeout bounds each upstream backend request (0 = no timeout); it
// seeds the in-memory config and stays adjustable via PUT /admin/config.
func New(maxQueue int, models []string, parentURLs []string, parentAuth, apiToken string, genTimeout time.Duration) (*Server, error) {
	events := newEventHub()
	appCfg := config.Default()
	appCfg.MaxQueue = maxQueue
	appCfg.GenTimeout = genTimeout
	appCfg.ParentAuth = parentAuth
	appCfg.APIToken = apiToken
	store := config.NewStore(appCfg)
	s := &Server{
		cfg:        store,
		router:     NewRouter(store, models, events),
		events:     events,
		uiFS:       ui.FS(),
		calls:      newCallRegistry(),
		startedAt:  time.Now(),
	}
	s.parents = NewParentManager(s)

	for _, raw := range parentURLs {
		if _, err := s.parents.Add(raw); err != nil {
			return nil, err
		}
	}
	s.parents.Start()
	return s, nil
}

// UseUI serves the web UI from the given filesystem (same layout as the
// embedded assets: index.html, css/, js/) instead of the embedded copy.
// Call before Handler. A nil fsys restores the embedded assets.
func (s *Server) UseUI(fsys fs.FS) {
	if fsys == nil {
		fsys = ui.FS()
	}
	s.uiFS = fsys
}

// Handler returns the HTTP handler with all routes registered.
// Route registration itself lives in api.go (Server.HandleFunc).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.HandleFunc(mux)
	return logRequests(mux)
}

// logRequests logs one line per HTTP request (method, path, status, elapsed
// time). Long-lived streaming and tunnel requests are logged when they finish.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		d := time.Since(start).Round(time.Millisecond)
		if lw.status >= http.StatusBadRequest {
			log.Printf("[http] %s %s from %s -> %d in %s", r.Method, r.URL.Path, r.RemoteAddr, lw.status, d)
		} else {
			log.Printf("[http] %s %s -> %d in %s", r.Method, r.URL.Path, lw.status, d)
		}
	})
}

// statusRecorder captures the response status code and still supports
// http.Flusher so streaming handlers keep working through the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ShutDown gracefully stops the server.
func (s *Server) ShutDown() {
	s.parents.Stop()
	s.router.Stop()
}
