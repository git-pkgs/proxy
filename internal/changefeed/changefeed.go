// Package changefeed reads the feeds in which public registries list the
// packages that changed since a given point, so the proxy can revalidate its
// cached metadata for them before metadata_ttl runs out.
package changefeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Feed is one registry's change feed. It remembers how far it has read, in
// memory only: after a restart it starts again from the registry's current
// position.
type Feed interface {
	// Ecosystem is the metadata cache ecosystem the changed names belong to.
	Ecosystem() string
	// Changes returns what changed since the previous call. The first call
	// only finds the feed's current position and reports no changes. On error
	// the position stays where it was, so the next call asks again.
	Changes(ctx context.Context) (Changes, error)
}

// Changes is one read of a feed.
type Changes struct {
	// Names are the cache keys of the documents that changed.
	Names []string
	// All is set when the feed can't say what changed, so every document of
	// the ecosystem has to be treated as changed.
	All bool
}

// maxFeedResponse bounds a feed response read into memory. A full page of
// the npm feed is about 1.5 MB.
const maxFeedResponse = 64 << 20

// getJSON fetches u and decodes its JSON body into v. A status other than 200
// is an error unless allowStatus also accepts it.
func getJSON(ctx context.Context, client *http.Client, u string, v any, allowStatus ...int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && !slices.Contains(allowStatus, resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxFeedResponse))
		return fmt.Errorf("%s: unexpected status %d", redact(u), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedResponse+1))
	if err != nil {
		return fmt.Errorf("%s: reading response: %w", redact(u), err)
	}
	if len(body) > maxFeedResponse {
		return fmt.Errorf("%s: response larger than %d bytes", redact(u), maxFeedResponse)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: decoding response: %w", redact(u), err)
	}
	return nil
}

// redact drops credentials and the query from u for error messages.
func redact(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "feed"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	return parsed.String()
}

// withQuery returns base with the query parameters in q added.
func withQuery(base string, q url.Values) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// ComposerFeedURL is where a Packagist instance at apiURL, such as
// https://packagist.org, publishes its metadata change feed. Packagist
// advertises it as "metadata-changes-url" in its packages.json.
func ComposerFeedURL(apiURL string) string {
	return strings.TrimSuffix(apiURL, "/") + "/metadata/changes.json"
}

// ComposerFeed reads Packagist's metadata change feed
// (https://packagist.org/apidoc#track-package-updates). Its position is a
// timestamp in units of 1/10000 s. Asked without one it answers 400 with the
// current timestamp; asked with one older than its 24 hours of history it
// answers with a single "resync" action. Each update names the file that
// changed, "vendor/package" or "vendor/package~dev", which is also the key
// the Composer handler caches it under; a delete names the package, and
// covers both files.
type ComposerFeed struct {
	client *http.Client
	url    string
	since  int64
}

// NewComposerFeed returns the feed at feedURL, see ComposerFeedURL.
func NewComposerFeed(client *http.Client, feedURL string) *ComposerFeed {
	return &ComposerFeed{client: client, url: feedURL}
}

func (f *ComposerFeed) Ecosystem() string { return "composer" }

type composerChanges struct {
	Actions []struct {
		Type    string `json:"type"`
		Package string `json:"package"`
	} `json:"actions"`
	Timestamp int64  `json:"timestamp"`
	Error     string `json:"error"`
}

func (f *ComposerFeed) Changes(ctx context.Context) (Changes, error) {
	if f.since == 0 {
		var start composerChanges
		if err := getJSON(ctx, f.client, f.url, &start, http.StatusBadRequest); err != nil {
			return Changes{}, err
		}
		if start.Timestamp <= 0 {
			return Changes{}, errors.New("composer change feed: no timestamp to start from")
		}
		f.since = start.Timestamp
		return Changes{}, nil
	}

	var page composerChanges
	u := withQuery(f.url, url.Values{"since": {strconv.FormatInt(f.since, 10)}})
	if err := getJSON(ctx, f.client, u, &page); err != nil {
		return Changes{}, err
	}
	if page.Error != "" || page.Timestamp <= 0 {
		return Changes{}, fmt.Errorf("composer change feed: %q", page.Error)
	}
	var changes Changes
	for _, a := range page.Actions {
		switch a.Type {
		case "update":
			changes.Names = append(changes.Names, a.Package)
		case "delete":
			// A deleted package takes its dev versions with it.
			changes.Names = append(changes.Names, a.Package, a.Package+"~dev")
		case "resync":
			// The position is too old for the feed to list what changed.
			changes.All = true
		}
	}
	f.since = page.Timestamp
	return changes, nil
}

// NPMFeedURL is the change feed of the public npm registry.
const NPMFeedURL = "https://replicate.npmjs.com/registry/_changes"

// npmPageLimit is the most changes the npm feed returns in one response.
const npmPageLimit = 10000

// npmMaxPages bounds how many pages one Changes call reads. The rest is read
// on the next call.
const npmMaxPages = 10

// NPMFeed reads the npm registry's CouchDB-style changes feed at
// replicate.npmjs.com. Its position is a sequence, which npm asks followers
// to treat as opaque; the database document at the feed's parent URL, with
// its trailing slash (without it the URL is 404), gives the current one as
// update_seq. A page holds at most 10000 changes (1000 when no limit is
// given). Each result's id is a package name, which is also the key the npm
// handler caches its packument under.
type NPMFeed struct {
	client *http.Client
	url    string
	// since is the sequence exactly as the feed returned it.
	since json.RawMessage
}

// NewNPMFeed returns the feed at feedURL, see NPMFeedURL.
func NewNPMFeed(client *http.Client, feedURL string) *NPMFeed {
	return &NPMFeed{client: client, url: feedURL}
}

func (f *NPMFeed) Ecosystem() string { return "npm" }

type npmChanges struct {
	Results []struct {
		ID string `json:"id"`
	} `json:"results"`
	LastSeq json.RawMessage `json:"last_seq"`
}

func (f *NPMFeed) Changes(ctx context.Context) (Changes, error) {
	if f.since == nil {
		var info struct {
			UpdateSeq json.RawMessage `json:"update_seq"`
		}
		if err := getJSON(ctx, f.client, strings.TrimSuffix(f.url, "_changes"), &info); err != nil {
			return Changes{}, err
		}
		if len(info.UpdateSeq) == 0 {
			return Changes{}, errors.New("npm change feed: no update_seq to start from")
		}
		f.since = info.UpdateSeq
		return Changes{}, nil
	}

	var changes Changes
	for range npmMaxPages {
		var page npmChanges
		u := withQuery(f.url, url.Values{
			"since": {seqParam(f.since)},
			"limit": {strconv.Itoa(npmPageLimit)},
		})
		if err := getJSON(ctx, f.client, u, &page); err != nil {
			// Changes already read are reported, and the position kept
			// after them, so they aren't lost to a later page's failure.
			if len(changes.Names) > 0 {
				return changes, nil
			}
			return Changes{}, err
		}
		if len(page.LastSeq) == 0 {
			return changes, errors.New("npm change feed: response without last_seq")
		}
		for _, r := range page.Results {
			if r.ID != "" && !strings.HasPrefix(r.ID, "_design/") {
				changes.Names = append(changes.Names, r.ID)
			}
		}
		f.since = page.LastSeq
		if len(page.Results) < npmPageLimit {
			break
		}
	}
	return changes, nil
}

// seqParam returns a sequence as the since parameter takes it: a number as
// is, a string without its quotes.
func seqParam(seq json.RawMessage) string {
	var s string
	if json.Unmarshal(seq, &s) == nil {
		return s
	}
	return string(seq)
}
