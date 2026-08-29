package server

import (
	"log"
	"net/http"
	"time"
)

// Server holds the HTTP state for the cllama proxy.
type Server struct {
	router *Router

	// Parent-side: pending tunneled requests from child cllama servers.
	calls *callRegistry

	// Child-side: outbound connections to parent cllama servers (-parent).
	parents *ParentManager

	// When set, child tunnel connections must present this bearer token.
	parentAuth string

	// When set, LLM API and model-list requests must present this bearer token.
	apiToken string

	// Reported as modified_at in the ollama /api/tags model list.
	startedAt time.Time
}

// New creates a new Server exposing the given mock model names.
// maxQueue caps how many requests are held when no backend is available.
// parentURLs (see -parent) connect this server as a child to parent cllama
// servers, in the form http://[token@]host:port/<parent-model>.
// parentAuth, when non-empty, is the token child cllama servers must present.
// apiToken, when non-empty, is the bearer token required on LLM API requests.
func New(maxQueue int, models []string, parentURLs []string, parentAuth, apiToken string) (*Server, error) {
	s := &Server{
		router:     NewRouter(maxQueue, models),
		calls:      newCallRegistry(),
		parentAuth: parentAuth,
		apiToken:   apiToken,
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
