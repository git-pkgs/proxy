package handler

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	registryclient "github.com/git-pkgs/registries/client"
)

// writeUpstreamHeld answers a request the rate limit gate refused to send
// with 503 and the Retry-After the upstream asked for, and reports whether
// err was such a refusal.
func writeUpstreamHeld(w http.ResponseWriter, err error) bool {
	var held *registryclient.HeldError
	if !errors.As(err, &held) {
		return false
	}
	secs := int(math.Ceil(held.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, held.Error(), http.StatusServiceUnavailable)
	return true
}
