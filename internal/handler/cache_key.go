package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// hashedCacheKey derives a metadata cache key from parts such as the
// upstream URL, the request path and the query. Each part is length-prefixed
// before hashing, so no two different part lists share a key, whatever bytes
// the parts contain. Keys built by replacing '/' with '_' were ambiguous:
// foo/bar_1.0 and foo_bar/1.0 mapped to the same entry.
func hashedCacheKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
