package handler

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/conda"
	"github.com/klauspost/compress/zstd"
)

// Sharded repodata (CEP 16): with cooldown the index is rewritten to point
// at filtered shards under hashes masked per hour (see conda.ShardMask).
const (
	condaShardIndexFile   = "repodata_shards.msgpack.zst"
	condaShardSuffix      = conda.ShardSuffix
	condaCooldownShardDir = conda.CooldownShardDir
	condaShardsSubdir     = conda.ShardsSubdir
	condaShardBucket      = time.Hour
	condaShardHashLen     = conda.ShardHashLen
	// maxCondaShardSize bounds a decompressed index or shard. conda-forge's
	// largest index is a few MB.
	maxCondaShardSize = 256 << 20
)

var (
	condaZstd = sync.OnceValues(func() (*zstd.Decoder, error) {
		return zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxCondaShardSize), zstd.WithDecoderConcurrency(0))
	})
	condaZstdEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
		return zstd.NewWriter(nil)
	})
)

func (h *CondaHandler) cooldownEnabled() bool {
	return h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()
}

// handleShardIndex serves repodata_shards.msgpack.zst.
func (h *CondaHandler) handleShardIndex(w http.ResponseWriter, r *http.Request) {
	channel, arch := r.PathValue("channel"), r.PathValue("arch")
	upstreamURL := h.upstreamURL + r.URL.Path
	cacheKey := h.metadataCacheKey(r.URL.Path)

	res := h.proxy.lookupMetadata(r.Context(), "conda", cacheKey, upstreamURL, "*/*", "identity", nil)
	if res.err != nil {
		h.proxy.writeMetadataFetchError(w, res.err)
		return
	}
	body := res.body

	if !h.cooldownEnabled() {
		index, err := decodeCondaShardIndex(body)
		if err == nil {
			_, err = index.ShardsLayout(upstreamURL)
		}
		if err != nil {
			h.proxy.Logger.Info("serving no sharded repodata", "url", upstreamURL, "error", err)
			http.NotFound(w, r)
			return
		}
		writeMetadataResult(w, r, res)
		return
	}

	now := time.Now()
	bucket := strconv.FormatInt(now.Unix()/int64(condaShardBucket/time.Second), 10)
	rewritten, err := rewriteCondaShardIndex(body, upstreamURL, condaShardMask(channel, arch, bucket), bucket)
	if err != nil {
		h.proxy.Logger.Info("serving no sharded repodata", "url", upstreamURL, "error", err)
		http.NotFound(w, r)
		return
	}

	next := now.Truncate(condaShardBucket).Add(condaShardBucket)
	maxAge := max(int(next.Sub(now)/time.Second), 1)
	w.Header().Set(headerContentType, "application/octet-stream")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	_, _ = w.Write(rewritten)
}

// handleShard serves a shard under its upstream hash. While cooldown is on,
// the index never points here, and the unfiltered shard is refused.
func (h *CondaHandler) handleShard(w http.ResponseWriter, r *http.Request) {
	if h.cooldownEnabled() || !isCondaShardFile(path.Base(r.URL.Path)) {
		http.NotFound(w, r)
		return
	}
	h.proxyCached(w, r)
}

// handleCooldownShard serves a shard named in a rewritten index, filtered by
// cooldown.
func (h *CondaHandler) handleCooldownShard(w http.ResponseWriter, r *http.Request) {
	channel, arch, bucket := r.PathValue("channel"), r.PathValue("arch"), r.PathValue("bucket")
	dir, file := path.Split(r.PathValue("rest"))
	if _, err := strconv.ParseUint(bucket, 10, 64); err != nil || (dir != "" && dir != condaShardsSubdir) || !isCondaShardFile(file) {
		http.NotFound(w, r)
		return
	}

	masked, _ := hex.DecodeString(strings.TrimSuffix(file, condaShardSuffix))
	mask := condaShardMask(channel, arch, bucket)
	for i := range masked {
		masked[i] ^= mask[i]
	}
	upstreamPath := "/" + channel + "/" + arch + "/" + dir + hex.EncodeToString(masked) + condaShardSuffix

	body, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), "conda", h.metadataCacheKey(upstreamPath), h.upstreamURL+upstreamPath, "identity", nil, "*/*")
	if err != nil {
		h.proxy.writeMetadataFetchError(w, err)
		return
	}

	if h.cooldownEnabled() {
		body, err = h.filterCondaShard(body)
		if err != nil {
			h.proxy.Logger.Error("failed to filter conda shard", "path", upstreamPath, "error", err)
			http.Error(w, "failed to filter shard", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set(headerContentType, "application/octet-stream")
	_, _ = w.Write(body)
}

func isCondaShardFile(name string) bool {
	return conda.IsShardFile(name)
}

// condaShardMask is XORed with shard hashes in a rewritten index (see
// conda.ShardMask).
func condaShardMask(channel, arch, bucket string) [condaShardHashLen]byte {
	return conda.ShardMask(channel, arch, bucket)
}

func decompressCondaShard(body []byte) ([]byte, error) {
	dec, err := condaZstd()
	if err != nil {
		return nil, err
	}
	return dec.DecodeAll(body, nil)
}

func compressCondaShard(body []byte) ([]byte, error) {
	enc, err := condaZstdEncoder()
	if err != nil {
		return nil, err
	}
	return enc.EncodeAll(body, nil), nil
}

// decodeCondaShardIndex decodes a repodata_shards.msgpack.zst.
func decodeCondaShardIndex(body []byte) (*conda.ShardIndex, error) {
	raw, err := decompressCondaShard(body)
	if err != nil {
		return nil, err
	}
	return conda.DecodeShardIndex(raw)
}

// rewriteCondaShardIndex masks every shard hash and points shards_base_url
// at the cooldown shard route for bucket (see conda.RewriteShardIndex).
func rewriteCondaShardIndex(body []byte, indexURL string, mask [condaShardHashLen]byte, bucket string) ([]byte, error) {
	raw, err := decompressCondaShard(body)
	if err != nil {
		return nil, err
	}
	rewritten, err := conda.RewriteShardIndex(raw, indexURL, mask, bucket)
	if err != nil {
		return nil, err
	}
	return compressCondaShard(rewritten)
}

// filterCondaShard leaves out records in packages and packages.conda that
// are still inside their cooldown (see conda.FilterShard). Kept records
// keep their upstream bytes.
func (h *CondaHandler) filterCondaShard(body []byte) ([]byte, error) {
	raw, err := decompressCondaShard(body)
	if err != nil {
		return nil, err
	}
	filtered, err := conda.FilterShard(raw, h.cooldownOptions(time.Now()))
	if err != nil {
		return nil, err
	}
	return compressCondaShard(filtered)
}
