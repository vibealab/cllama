// Package config holds the in-memory system configuration of a cllama
// server: queue depth and request-queue retention. Values live only in
// memory (seeded from defaults and command-line flags), are safe for
// concurrent use, and can be inspected or updated at runtime through the
// admin API (GET/PUT /admin/config). Nothing is persisted; a restart
// restores the defaults.
package config

import (
	"sync"
	"time"
)

// RetentionForever is the sentinel retention value meaning "keep the entry
// in the request queue forever; only a manual admin operation (DELETE
// /admin/queue/<id>) removes it".
const RetentionForever time.Duration = -1

// Config is a snapshot of the system configuration.
type Config struct {
	// MaxQueue caps how many requests are held in the queue while no
	// backend serves their model (0 disables queueing).
	MaxQueue int

	// DoneRetention is how long a completed ("done") request lingers in
	// the request queue for review:
	//
	//	RetentionForever (-1)  stay in the queue forever; removed only by
	//	                       a manual admin operation
	//	0                      leave the queue immediately (default)
	//	>0                     stay in the queue for this long, then purge
	DoneRetention time.Duration

	// FailedRetention is DoneRetention for failed requests.
	FailedRetention time.Duration

	// GenTimeout bounds each upstream backend generation request:
	//
	//	0   no timeout (default)
	//	>0  abort the upstream request after this long
	//
	// It applies to clients of backends registered after the value
	// changes; already-registered backends keep the old timeout.
	GenTimeout time.Duration
}

// Default returns the default system configuration: queueing enabled with
// a depth of 100, and completed/failed requests dropped as soon as they
// reach a terminal state.
func Default() Config {
	return Config{
		MaxQueue:        100,
		DoneRetention:   0,
		FailedRetention: 0,
		GenTimeout:      0,
	}
}

// Store is a concurrency-safe holder of the in-memory Config. Get returns
// immutable snapshots; Update replaces the whole configuration.
type Store struct {
	mu  sync.RWMutex
	cfg Config
}

// NewStore creates a store seeded with cfg.
func NewStore(cfg Config) *Store {
	return &Store{cfg: cfg}
}

// Get returns the current configuration snapshot.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update replaces the configuration. Routers pick the new values up on
// their next decision (queue admission, retention expiry, ...).
func (s *Store) Update(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}
