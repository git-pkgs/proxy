package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
)

// Sharded repodata (CEP 16) maps each package name to the sha256 of its
// shard. With cooldown the proxy filters shards, which changes their bytes,
// but clients cache shards by hash and never fetch the same hash again. So
// the rewritten index XORs every hash with a mask that changes each hour:
// filtered shards get hashes of their own, and clients fetch them again once
// versions have left cooldown.
const (
	condaShardIndexFile   = "repodata_shards.msgpack.zst"
	condaShardSuffix      = ".msgpack.zst"
	condaCooldownShardDir = "cooldown-shards"
	condaShardsSubdir     = "shards/"
	condaShardBucket      = time.Hour
	condaShardIndexFormat = 1
	condaShardHashLen     = sha256.Size
	// maxCondaShardSize bounds a decompressed index or shard. conda-forge's
	// largest index is a few MB.
	maxCondaShardSize = 256 << 20
)

var (
	errCondaShardLayout = errors.New("unsupported sharded repodata layout")

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

	body, contentType, contentEncoding, err := h.proxy.fetchOrCacheMetadata(r.Context(), "conda", cacheKey, upstreamURL, "identity", nil, "*/*")
	if err != nil {
		h.proxy.writeMetadataFetchError(w, err)
		return
	}

	if !h.cooldownEnabled() {
		index, err := decodeCondaShardIndex(body)
		if err == nil {
			_, err = index.shardsLayout(upstreamURL)
		}
		if err != nil {
			h.proxy.Logger.Info("serving no sharded repodata", "url", upstreamURL, "error", err)
			http.NotFound(w, r)
			return
		}
		h.proxy.writeMetadataCachedResponseWithEncoding(w, r, "conda", cacheKey, body, contentType, contentEncoding)
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

	body, _, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), "conda", h.metadataCacheKey(upstreamPath), h.upstreamURL+upstreamPath, "identity", nil, "*/*")
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
	hash, ok := strings.CutSuffix(name, condaShardSuffix)
	if !ok || len(hash) != 2*condaShardHashLen {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil && strings.ToLower(hash) == hash
}

// condaShardMask is XORed with shard hashes in a rewritten index. It keeps
// filtered shards apart from upstream ones in client caches, which are keyed
// by hash alone.
func condaShardMask(channel, arch, bucket string) [condaShardHashLen]byte {
	return sha256.Sum256([]byte("git-pkgs proxy conda cooldown shard\x00" + channel + "\x00" + arch + "\x00" + bucket))
}

// msgpackEntry is one member of a msgpack map, kept in its upstream order
// with its value undecoded.
type msgpackEntry struct {
	key   string
	value msgpack.RawMessage
}

func decodeMsgpackMap(b []byte) ([]msgpackEntry, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(b))
	n, err := dec.DecodeMapLen()
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, errors.New("expected a msgpack map")
	}
	entries := make([]msgpackEntry, 0, n)
	for range n {
		key, err := dec.DecodeString()
		if err != nil {
			return nil, err
		}
		value, err := dec.DecodeRaw()
		if err != nil {
			return nil, err
		}
		entries = append(entries, msgpackEntry{key: key, value: value})
	}
	return entries, nil
}

func encodeMsgpackMap(entries []msgpackEntry) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	if err := enc.EncodeMapLen(len(entries)); err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := enc.EncodeString(e.key); err != nil {
			return nil, err
		}
		if err := enc.Encode(e.value); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func lookupMsgpackEntry(entries []msgpackEntry, key string) (int, bool) {
	for i, e := range entries {
		if e.key == key {
			return i, true
		}
	}
	return -1, false
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

// condaShardIndex is a decoded repodata_shards.msgpack.zst.
type condaShardIndex struct {
	top  []msgpackEntry
	info []msgpackEntry
}

func decodeCondaShardIndex(body []byte) (*condaShardIndex, error) {
	raw, err := decompressCondaShard(body)
	if err != nil {
		return nil, err
	}
	top, err := decodeMsgpackMap(raw)
	if err != nil {
		return nil, err
	}
	i, ok := lookupMsgpackEntry(top, "version")
	if !ok {
		return nil, errors.New("sharded index has no version")
	}
	var version int
	if err := msgpack.Unmarshal(top[i].value, &version); err != nil || version != condaShardIndexFormat {
		return nil, fmt.Errorf("unsupported sharded index version %v", top[i].value)
	}
	i, ok = lookupMsgpackEntry(top, "info")
	if !ok {
		return nil, errors.New("sharded index has no info")
	}
	info, err := decodeMsgpackMap(top[i].value)
	if err != nil {
		return nil, err
	}
	return &condaShardIndex{top: top, info: info}, nil
}

func (idx *condaShardIndex) infoString(key string) (string, error) {
	i, ok := lookupMsgpackEntry(idx.info, key)
	if !ok {
		return "", nil
	}
	var s string
	err := msgpack.Unmarshal(idx.info[i].value, &s)
	return s, err
}

// shardsLayout checks that packages and shards sit where the proxy routes
// them, below the subdir of indexURL, and returns the shards directory
// relative to the subdir: "" or "shards/". Clients resolve base_url and
// shards_base_url against the proxy's URL, so only relative paths stay on
// the proxy: a scheme or host leads clients around it, a leading / outside
// its mount.
func (idx *condaShardIndex) shardsLayout(indexURL string) (string, error) {
	base, err := url.Parse(indexURL)
	if err != nil {
		return "", err
	}
	subdir := base.ResolveReference(&url.URL{Path: "./"}).String()
	resolve := func(ref string) (string, error) {
		if ref == "" {
			return subdir, nil
		}
		u, err := url.Parse(ref)
		if err != nil {
			return "", err
		}
		if u.Scheme != "" || u.Host != "" || strings.HasPrefix(u.Path, "/") {
			return "", errCondaShardLayout
		}
		return base.ResolveReference(u).String(), nil
	}

	baseURL, err := idx.infoString("base_url")
	if err != nil {
		return "", err
	}
	if resolved, err := resolve(baseURL); err != nil || resolved != subdir {
		return "", fmt.Errorf("%w: base_url %q", errCondaShardLayout, baseURL)
	}

	shardsBaseURL, err := idx.infoString("shards_base_url")
	if err != nil {
		return "", err
	}
	resolved, err := resolve(shardsBaseURL)
	rel, ok := strings.CutPrefix(resolved, subdir)
	if err != nil || !ok || (rel != "" && rel != condaShardsSubdir) {
		return "", fmt.Errorf("%w: shards_base_url %q", errCondaShardLayout, shardsBaseURL)
	}
	return rel, nil
}

// rewriteCondaShardIndex masks every shard hash and points shards_base_url
// at the cooldown shard route for bucket.
func rewriteCondaShardIndex(body []byte, indexURL string, mask [condaShardHashLen]byte, bucket string) ([]byte, error) {
	idx, err := decodeCondaShardIndex(body)
	if err != nil {
		return nil, err
	}
	layout, err := idx.shardsLayout(indexURL)
	if err != nil {
		return nil, err
	}

	shardsBase, err := msgpack.Marshal("./" + condaCooldownShardDir + "/" + bucket + "/" + layout)
	if err != nil {
		return nil, err
	}
	if i, ok := lookupMsgpackEntry(idx.info, "shards_base_url"); ok {
		idx.info[i].value = shardsBase
	} else {
		idx.info = append(idx.info, msgpackEntry{key: "shards_base_url", value: shardsBase})
	}
	info, err := encodeMsgpackMap(idx.info)
	if err != nil {
		return nil, err
	}
	i, _ := lookupMsgpackEntry(idx.top, "info")
	idx.top[i].value = info

	i, ok := lookupMsgpackEntry(idx.top, "shards")
	if !ok {
		return nil, errors.New("sharded index has no shards")
	}
	shards, err := decodeMsgpackMap(idx.top[i].value)
	if err != nil {
		return nil, err
	}
	for j := range shards {
		var hash []byte
		if err := msgpack.Unmarshal(shards[j].value, &hash); err != nil || len(hash) != condaShardHashLen {
			return nil, fmt.Errorf("shard hash for %q is not a sha256", shards[j].key)
		}
		for k := range hash {
			hash[k] ^= mask[k]
		}
		if shards[j].value, err = msgpack.Marshal(hash); err != nil {
			return nil, err
		}
	}
	if idx.top[i].value, err = encodeMsgpackMap(shards); err != nil {
		return nil, err
	}

	raw, err := encodeMsgpackMap(idx.top)
	if err != nil {
		return nil, err
	}
	return compressCondaShard(raw)
}

// condaShardRecord holds the fields cooldown needs from a shard record.
// Timestamps are milliseconds, encoded as an integer or a float.
type condaShardRecord struct {
	Name      string `msgpack:"name"`
	Version   string `msgpack:"version"`
	Timestamp any    `msgpack:"timestamp"`
}

// filterCondaShard leaves out records in packages and packages.conda that
// are still inside their cooldown. Kept records keep their upstream bytes.
func (h *CondaHandler) filterCondaShard(body []byte) ([]byte, error) {
	raw, err := decompressCondaShard(body)
	if err != nil {
		return nil, err
	}
	top, err := decodeMsgpackMap(raw)
	if err != nil {
		return nil, err
	}
	for i := range top {
		if top[i].key != "packages" && top[i].key != "packages.conda" {
			continue
		}
		records, err := decodeMsgpackMap(top[i].value)
		if err != nil {
			return nil, err
		}
		kept := records[:0]
		for _, rec := range records {
			var fields condaShardRecord
			if err := msgpack.Unmarshal(rec.value, &fields); err != nil {
				return nil, fmt.Errorf("record %q: %w", rec.key, err)
			}
			if h.recordInCooldown(rec.key, fields.Name, fields.Version, msgpackNumber(fields.Timestamp)) {
				continue
			}
			kept = append(kept, rec)
		}
		if top[i].value, err = encodeMsgpackMap(kept); err != nil {
			return nil, err
		}
	}
	out, err := encodeMsgpackMap(top)
	if err != nil {
		return nil, err
	}
	return compressCondaShard(out)
}

// msgpackNumber returns a decoded msgpack integer or float as float64, and 0
// for anything else.
func msgpackNumber(v any) float64 {
	switch n := v.(type) {
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}
