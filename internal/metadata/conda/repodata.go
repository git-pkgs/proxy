package conda

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/jsonstream"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

// repodataEntry holds the fields cooldown needs from a repodata.json entry.
type repodataEntry struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	Timestamp float64 `json:"timestamp"`
}

// Filter decides which repodata entries cooldown leaves out, all as of
// Options.EvaluatedAt, and records what it left out, which with the
// document fixes the filtered copy.
type Filter struct {
	o Options
	// withheld hashes the filenames left out.
	withheld hash.Hash
	// until is when the first entry left out leaves cooldown.
	until time.Time
	// unknownUntil is set when an entry was left out without a time it
	// leaves cooldown, so until can't be relied on.
	unknownUntil bool
}

// NewFilter returns a filter deciding by o.
func NewFilter(o Options) *Filter {
	return &Filter{o: o, withheld: sha256.New()}
}

// Copy copies repodata.json from r to w as it streams, leaving out entries
// in packages and packages.conda that are still inside their cooldown.
// Kept entries and every other member are written with their upstream
// bytes. The caller bounds r.
func (f *Filter) Copy(r io.Reader, w io.Writer) error {
	return jsonstream.CopyObject(r, w, func(m *jsonstream.Member) error {
		if !packagesMember(m.Key) {
			return m.Copy()
		}
		return m.Object(func(entry *jsonstream.Member) error {
			raw, err := entry.Raw()
			if err != nil {
				return err
			}
			if f.inCooldown(entry.Key, raw) {
				return nil
			}
			return entry.Write(raw)
		})
	})
}

// Withheld is a digest of the filenames the filter left out so far.
func (f *Filter) Withheld() []byte {
	return f.withheld.Sum(nil)
}

// Until is when the first entry left out leaves cooldown, zero when none
// was left out. known is false when an entry was left out without such a
// time, so the filtered copy can't be reused until a given time.
func (f *Filter) Until() (until time.Time, known bool) {
	return f.until, !f.unknownUntil
}

// inCooldown reports whether a repodata entry was published too recently.
// Entries without a name or timestamp are kept.
func (f *Filter) inCooldown(filename string, raw []byte) bool {
	var entry repodataEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	decision, withheld := f.o.decide(entry.Name, entry.Timestamp)
	if !withheld {
		return false
	}
	f.o.filtered(filename, entry.Name, entry.Version)
	_, _ = fmt.Fprintf(f.withheld, "%d:%s", len(filename), filename)
	switch {
	case decision.AvailableAt.IsZero():
		f.unknownUntil = true
	case f.until.IsZero() || decision.AvailableAt.Before(f.until):
		f.until = decision.AvailableAt
	}
	return true
}

// decide evaluates a package record's timestamp, reporting whether cooldown
// holds it back. Records without a name or timestamp, and any record
// without an enabled policy, are kept.
func (o Options) decide(name string, timestamp float64) (cooldown.Decision, bool) {
	if name == "" || timestamp == 0 || o.Cooldown == nil || !o.Cooldown.Enabled() {
		return cooldown.Decision{}, false
	}
	decision := o.Cooldown.Evaluate("conda", packageurl.MakeString(ecosystem, name, ""), Time(timestamp), o.EvaluatedAt)
	return decision, !decision.Allowed
}

func (o Options) filtered(filename, name, version string) {
	if o.OnCooldownFilter != nil {
		o.OnCooldownFilter(filename, name, version)
	}
}

// ReadIndex maps each package file in a repodata.json to its timestamp, or
// the zero time for a file without one, reading the document as it
// streams. Entries that aren't objects are left out. The caller bounds r.
func ReadIndex(r io.Reader) (map[string]time.Time, error) {
	index := make(map[string]time.Time)
	err := jsonstream.CopyObject(r, io.Discard, func(m *jsonstream.Member) error {
		if !packagesMember(m.Key) {
			return m.Copy()
		}
		return m.Object(func(entry *jsonstream.Member) error {
			raw, err := entry.Raw()
			if err != nil {
				return err
			}
			var e repodataEntry
			if json.Unmarshal(raw, &e) != nil {
				return nil
			}
			index[entry.Key] = time.Time{}
			if e.Timestamp != 0 {
				index[entry.Key] = Time(e.Timestamp)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// packagesMember reports whether a top-level repodata member lists package
// files.
func packagesMember(key string) bool {
	return key == "packages" || key == "packages.conda"
}
