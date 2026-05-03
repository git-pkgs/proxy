package server

import (
	"encoding/json"
	"net/http"
)

// Error codes returned in API error responses. These are stable identifiers
// that clients can match on; the message text is for humans and may change.
const (
	ErrCodeBadRequest = "BAD_REQUEST"
	ErrCodeNotFound   = "NOT_FOUND"
	ErrCodeUpstream   = "UPSTREAM_ERROR"
	ErrCodeInternal   = "INTERNAL_ERROR"
)

// ErrorResponse is the JSON body returned for API errors.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError sends a JSON error response with the given status, code and
// user-facing message. Internal error details should be logged separately
// by the caller, never passed as the message.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Code: code, Message: message})
}

func badRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, ErrCodeBadRequest, message)
}

func notFound(w http.ResponseWriter, message string) {
	writeError(w, http.StatusNotFound, ErrCodeNotFound, message)
}

func internalError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusInternalServerError, ErrCodeInternal, message)
}
