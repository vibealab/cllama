package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"cllama/config"
)

// Admin API for the in-memory system configuration (queue depth, request
// retention). Changes apply immediately but are not persisted: a restart
// restores the defaults (see the config package).
//
// GET /admin/config - show the current settings
// PUT /admin/config - replace the settings (partial bodies keep omitted
//
//	fields at their current values)
//
// Wire format uses whole seconds; -1 means "forever" for the retentions,
// and gen_timeout_sec is 0 (no timeout) or a positive number of seconds.
type configView struct {
	MaxQueue           int   `json:"max_queue"`
	DoneRetentionSec   int64 `json:"done_retention_sec"`
	FailedRetentionSec int64 `json:"failed_retention_sec"`
	GenTimeoutSec      int64 `json:"gen_timeout_sec"`
}

func configToView(cfg config.Config) configView {
	return configView{
		MaxQueue:           cfg.MaxQueue,
		DoneRetentionSec:   retentionToSec(cfg.DoneRetention),
		FailedRetentionSec: retentionToSec(cfg.FailedRetention),
		GenTimeoutSec:      int64(cfg.GenTimeout / time.Second),
	}
}

// retentionToSec renders a retention duration as whole seconds
// (config.RetentionForever maps to -1).
func retentionToSec(d time.Duration) int64 {
	if d < 0 {
		return -1
	}
	return int64(d / time.Second)
}

// secToRetention converts wire seconds to a retention duration; negative
// values collapse to config.RetentionForever.
func secToRetention(sec int64) time.Duration {
	if sec < 0 {
		return config.RetentionForever
	}
	return time.Duration(sec) * time.Second
}

// genTimeoutFromSec converts wire seconds to a generation timeout
// (0 = no timeout, >0 = timeout after that long).
func genTimeoutFromSec(sec int64) time.Duration {
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// validate rejects nonsensical settings.
func (v configView) validate() error {
	if v.MaxQueue < 0 {
		return errors.New("max_queue must be >= 0")
	}
	if v.DoneRetentionSec < -1 || v.FailedRetentionSec < -1 {
		return errors.New("retention seconds must be -1 (forever), 0 (drop immediately) or a positive duration")
	}
	if v.GenTimeoutSec < 0 {
		return errors.New("gen_timeout_sec must be 0 (no timeout) or a positive duration in seconds")
	}
	return nil
}

// handleAdminConfig serves GET/PUT /admin/config.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		respondJSON(w, http.StatusOK, configToView(s.cfg.Get()))
	case http.MethodPut:
		s.updateConfig(w, r)
	default:
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// updateConfig applies a PUT /admin/config body. Omitted fields keep their
// current values, so the admin can patch one setting at a time.
func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	// Decode over the current view so absent JSON keys are kept.
	next := configToView(s.cfg.Get())
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		log.Printf("[admin] config update rejected: invalid JSON body from %s: %v", r.RemoteAddr, err)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := next.validate(); err != nil {
		log.Printf("[admin] config update rejected: %v", err)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	s.cfg.Update(config.Config{
		MaxQueue:        next.MaxQueue,
		DoneRetention:   secToRetention(next.DoneRetentionSec),
		FailedRetention: secToRetention(next.FailedRetentionSec),
		GenTimeout:      genTimeoutFromSec(next.GenTimeoutSec),
	})
	log.Printf("[admin] config updated via API: max_queue=%d done_retention=%ds failed_retention=%ds gen_timeout=%ds",
		next.MaxQueue, next.DoneRetentionSec, next.FailedRetentionSec, next.GenTimeoutSec)

	s.events.emit(topicConfig)
	respondJSON(w, http.StatusOK, configToView(s.cfg.Get()))
}
