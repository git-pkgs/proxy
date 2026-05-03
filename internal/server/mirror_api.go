package server

import (
	"encoding/json"
	"net/http"

	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/go-chi/chi/v5"
)

// MirrorAPIHandler handles mirror API requests.
type MirrorAPIHandler struct {
	jobs *mirror.JobStore
}

// NewMirrorAPIHandler creates a new mirror API handler.
func NewMirrorAPIHandler(jobs *mirror.JobStore) *MirrorAPIHandler {
	return &MirrorAPIHandler{jobs: jobs}
}

// HandleCreate starts a new mirror job.
func (h *MirrorAPIHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req mirror.JobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid request body")
		return
	}

	id, err := h.jobs.Create(req)
	if err != nil {
		badRequest(w, "invalid mirror job request")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"id": id})
}

// HandleGet returns the status of a mirror job.
func (h *MirrorAPIHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job := h.jobs.Get(id)
	if job == nil {
		notFound(w, "job not found")
		return
	}

	writeJSON(w, job)
}

// HandleCancel cancels a running mirror job.
func (h *MirrorAPIHandler) HandleCancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.jobs.Cancel(id) {
		writeJSON(w, map[string]string{"status": "canceled"})
	} else {
		notFound(w, "job not found or not running")
	}
}
