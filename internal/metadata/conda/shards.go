package conda

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// Sharded repodata (CEP 16) maps each package name to the sha256 of its
// shard. With cooldown the proxy filters shards, which changes their bytes,
// but clients cache shards by hash and never fetch the same hash again. So
// the rewritten index XORs every hash with a mask that changes with each
// bucket: filtered shards get hashes of their own, and clients fetch them
// again once versions have left cooldown.
//
// The functions here take and return the decompressed msgpack documents;
// the zstd framing is the caller's.
const (
	// ShardSuffix ends a shard's file name, after its hex hash.
	ShardSuffix = ".msgpack.zst"
	// CooldownShardDir is the directory, below a subdir, that a rewritten
	// index points shards_base_url at.
	CooldownShardDir = "cooldown-shards"
	// ShardsSubdir is the one shards directory below a subdir the proxy
	// routes besides the subdir itself.
	ShardsSubdir = "shards/"
	// ShardHashLen is the length of a shard hash, a sha256.
	ShardHashLen      = sha256.Size
	shardIndexFormat  = 1
	shardMaskDomain   = "git-pkgs proxy conda cooldown shard\x00"
	shardsBaseURLKey  = "shards_base_url"
	shardIndexInfoKey = "info"
)

// ErrShardLayout is returned for a sharded index whose base_url or
// shards_base_url leads outside the proxy's routes.
var ErrShardLayout = errors.New("unsupported sharded repodata layout")

// IsShardFile reports whether name is a shard file name: a lowercase hex
// sha256 followed by ShardSuffix.
func IsShardFile(name string) bool {
	hash, ok := strings.CutSuffix(name, ShardSuffix)
	if !ok || len(hash) != 2*ShardHashLen {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil && strings.ToLower(hash) == hash
}

// ShardMask is XORed with shard hashes in a rewritten index. It keeps
// filtered shards apart from upstream ones in client caches, which are
// keyed by hash alone.
func ShardMask(channel, arch, bucket string) [ShardHashLen]byte {
	return sha256.Sum256([]byte(shardMaskDomain + channel + "\x00" + arch + "\x00" + bucket))
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

// ShardIndex is a decoded repodata_shards.msgpack document.
type ShardIndex struct {
	top  []msgpackEntry
	info []msgpackEntry
}

// DecodeShardIndex decodes a decompressed repodata_shards.msgpack. Only
// format version 1 is read.
func DecodeShardIndex(raw []byte) (*ShardIndex, error) {
	top, err := decodeMsgpackMap(raw)
	if err != nil {
		return nil, err
	}
	i, ok := lookupMsgpackEntry(top, "version")
	if !ok {
		return nil, errors.New("sharded index has no version")
	}
	var version int
	if err := msgpack.Unmarshal(top[i].value, &version); err != nil || version != shardIndexFormat {
		return nil, fmt.Errorf("unsupported sharded index version %v", top[i].value)
	}
	i, ok = lookupMsgpackEntry(top, shardIndexInfoKey)
	if !ok {
		return nil, errors.New("sharded index has no info")
	}
	info, err := decodeMsgpackMap(top[i].value)
	if err != nil {
		return nil, err
	}
	return &ShardIndex{top: top, info: info}, nil
}

func (idx *ShardIndex) infoString(key string) (string, error) {
	i, ok := lookupMsgpackEntry(idx.info, key)
	if !ok {
		return "", nil
	}
	var s string
	err := msgpack.Unmarshal(idx.info[i].value, &s)
	return s, err
}

// ShardsLayout checks that packages and shards sit where the proxy routes
// them, below the subdir of indexURL, and returns the shards directory
// relative to the subdir: "" or ShardsSubdir. Clients resolve base_url and
// shards_base_url against the proxy's URL, so only relative paths stay on
// the proxy: a scheme or host leads clients around it, a leading / outside
// its mount.
func (idx *ShardIndex) ShardsLayout(indexURL string) (string, error) {
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
			return "", ErrShardLayout
		}
		return base.ResolveReference(u).String(), nil
	}

	baseURL, err := idx.infoString("base_url")
	if err != nil {
		return "", err
	}
	if resolved, err := resolve(baseURL); err != nil || resolved != subdir {
		return "", fmt.Errorf("%w: base_url %q", ErrShardLayout, baseURL)
	}

	shardsBaseURL, err := idx.infoString(shardsBaseURLKey)
	if err != nil {
		return "", err
	}
	resolved, err := resolve(shardsBaseURL)
	rel, ok := strings.CutPrefix(resolved, subdir)
	if err != nil || !ok || (rel != "" && rel != ShardsSubdir) {
		return "", fmt.Errorf("%w: shards_base_url %q", ErrShardLayout, shardsBaseURL)
	}
	return rel, nil
}

// RewriteShardIndex masks every shard hash in a decompressed sharded index
// and points shards_base_url at the cooldown shard route for bucket. It
// returns the decompressed rewritten index.
func RewriteShardIndex(raw []byte, indexURL string, mask [ShardHashLen]byte, bucket string) ([]byte, error) {
	idx, err := DecodeShardIndex(raw)
	if err != nil {
		return nil, err
	}
	layout, err := idx.ShardsLayout(indexURL)
	if err != nil {
		return nil, err
	}

	shardsBase, err := msgpack.Marshal("./" + CooldownShardDir + "/" + bucket + "/" + layout)
	if err != nil {
		return nil, err
	}
	if i, ok := lookupMsgpackEntry(idx.info, shardsBaseURLKey); ok {
		idx.info[i].value = shardsBase
	} else {
		idx.info = append(idx.info, msgpackEntry{key: shardsBaseURLKey, value: shardsBase})
	}
	info, err := encodeMsgpackMap(idx.info)
	if err != nil {
		return nil, err
	}
	i, _ := lookupMsgpackEntry(idx.top, shardIndexInfoKey)
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
		if err := msgpack.Unmarshal(shards[j].value, &hash); err != nil || len(hash) != ShardHashLen {
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
	return encodeMsgpackMap(idx.top)
}

// shardRecord holds the fields cooldown needs from a shard record.
// Timestamps are an integer or a float, in seconds or milliseconds as Time
// reads them.
type shardRecord struct {
	Name      string `msgpack:"name"`
	Version   string `msgpack:"version"`
	Timestamp any    `msgpack:"timestamp"`
}

// FilterShard leaves out records in packages and packages.conda of a
// decompressed shard that are still inside their cooldown, as of
// o.EvaluatedAt. Kept records keep their upstream bytes. It returns the
// decompressed filtered shard.
func FilterShard(raw []byte, o Options) ([]byte, error) {
	top, err := decodeMsgpackMap(raw)
	if err != nil {
		return nil, err
	}
	for i := range top {
		if !packagesMember(top[i].key) {
			continue
		}
		records, err := decodeMsgpackMap(top[i].value)
		if err != nil {
			return nil, err
		}
		kept := records[:0]
		for _, rec := range records {
			var fields shardRecord
			if err := msgpack.Unmarshal(rec.value, &fields); err != nil {
				return nil, fmt.Errorf("record %q: %w", rec.key, err)
			}
			if _, withheld := o.decide(fields.Name, msgpackNumber(fields.Timestamp)); withheld {
				o.filtered(rec.key, fields.Name, fields.Version)
				continue
			}
			kept = append(kept, rec)
		}
		if top[i].value, err = encodeMsgpackMap(kept); err != nil {
			return nil, err
		}
	}
	return encodeMsgpackMap(top)
}

// msgpackNumber returns a decoded msgpack integer or float as float64, and
// 0 for anything else.
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
