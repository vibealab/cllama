package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"cllama/config"
)

// Admin API for the in-memory system configuration (queue depth, request
// retention, auth tokens). Changes apply immediately but are not persisted:
// a restart restores the defaults (see the config package).
//
// GET  /admin/config             - show the current settings (tokens are
//                                  reduced to has_* flags)
// PUT  /admin/config             - update settings; api_token / parent_auth
//                                  keys change the tokens (empty clears)
// GET  /admin/config/secret/<n>  - reveal one token in plain text (for the
//                                  web UI eye toggle)
//
// Wire format uses whole seconds; -1 means "forever" for the retentions,
// and gen_timeout_sec is 0 (no timeout) or a positive number of seconds.
// Tokens are kept in plain memory; like the rest of /admin the reveal
// endpoint is unauthenticated, so keep it off untrusted networks.
type configView struct {
	MaxQueue           int   `json:"max_queue"`
	DoneRetentionSec   int64 `json:"done_retention_sec"`
	FailedRetentionSec int64 `json:"failed_retention_sec"`
	GenTimeoutSec      int64 `json:"gen_timeout_sec"`
	HasAPIToken        bool  `json:"has_api_token"`
	HasParentAuth      bool  `json:"has_parent_auth"`
}

func configToView(cfg config.Config) configView {
	return configView{
		MaxQueue:           cfg.MaxQueue,
		DoneRetentionSec:   retentionToSec(cfg.DoneRetention),
		FailedRetentionSec: retentionToSec(cfg.FailedRetention),
		GenTimeoutSec:      int64(cfg.GenTimeout / time.Second),
		HasAPIToken:        cfg.APIToken != "",
		HasParentAuth:      cfg.ParentAuth != "",
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
// current values, so the admin can patch one setting at a time. The tokens
// work the same way: the api_token / parent_auth JSON keys must be present
// to change them, and an empty string disables the respective auth.
func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	// Decode over the current view so absent JSON keys are kept.
	view := configToView(s.cfg.Get())
	var body struct {
		configView
		APIToken   *string `json:"api_token"`
		ParentAuth *string `json:"parent_auth"`
	}
	body.configView = view
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[admin] config update rejected: invalid JSON body from %s: %v", r.RemoteAddr, err)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	view = body.configView
	if err := view.validate(); err != nil {
		log.Printf("[admin] config update rejected: %v", err)
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	next := s.cfg.Get()
	next.MaxQueue = view.MaxQueue
	next.DoneRetention = secToRetention(view.DoneRetentionSec)
	next.FailedRetention = secToRetention(view.FailedRetentionSec)
	next.GenTimeout = genTimeoutFromSec(view.GenTimeoutSec)
	if body.APIToken != nil {
		next.APIToken = *body.APIToken
	}
	if body.ParentAuth != nil {
		next.ParentAuth = *body.ParentAuth
	}
	s.cfg.Update(next)
	// Log presence only — never the token values.
	log.Printf("[admin] config updated via API: max_queue=%d done_retention=%ds failed_retention=%ds gen_timeout=%ds api_token_set=%t parent_auth_set=%t",
		view.MaxQueue, view.DoneRetentionSec, view.FailedRetentionSec, view.GenTimeoutSec,
		next.APIToken != "", next.ParentAuth != "")

	s.events.emit(topicConfig)
	respondJSON(w, http.StatusOK, configToView(s.cfg.Get()))
}

// handleAdminConfigSecret serves GET /admin/config/secret/<name>, returning
// one auth token in plain text for the web UI's eye toggle: name is
// api_token or parent_auth.
func (s *Server) handleAdminConfigSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/config/secret/")
	cfg := s.cfg.Get()
	var value string
	switch name {
	case "api_token":
		value = cfg.APIToken
	case "parent_auth":
		value = cfg.ParentAuth
	default:
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "unknown secret"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"name":  name,
		"set":   value != "",
		"value": value,
	})
}
