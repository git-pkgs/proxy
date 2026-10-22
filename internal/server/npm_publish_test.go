package server

import (
	"crypto/sha1" //nolint:gosec // npm's legacy dist.shasum is sha1
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

var widgetTarball = npmTarball(`{"name":"@acme/widget","version":"1.0.0","dependencies":{"left-pad":"^1.3.0"}}`)

func sha512Integrity(data string) string {
	sum := sha512.Sum512([]byte(data))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// publishDocument builds the body npm publish sends for one version.
func publishDocument(name, version, tarball string, mutate func(doc map[string]any)) string {
	doc := map[string]any{
		"_id":       name,
		"name":      name,
		"dist-tags": map[string]string{"latest": version},
		"versions": map[string]any{version: map[string]any{
			"name":    name,
			"version": version,
			"dist":    map[string]string{"integrity": sha512Integrity(tarball)},
		}},
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream",
			"data":         base64.StdEncoding.EncodeToString([]byte(tarball)),
			"length":       len(tarball),
		}},
	}
	if mutate != nil {
		mutate(doc)
	}
	out, err := json.Marshal(doc)
	must(err)
	return string(out)
}

func TestNPMPublishIsInstallable(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/npm/@acme%2fwidget", testScopedToken, publishDocument("@acme/widget", "1.0.0", widgetTarball, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body %s", rec.Code, rec.Body)
	}

	rec = s.do(t, http.MethodGet, "/npm/@acme%2fwidget", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("packument status = %d, body %s", rec.Code, rec.Body)
	}
	var packument struct {
		Versions map[string]struct {
			Dependencies map[string]string `json:"dependencies"`
			Dist         struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &packument); err != nil {
		t.Fatalf("decoding packument: %v", err)
	}
	v, ok := packument.Versions["1.0.0"]
	if !ok || v.Dependencies["left-pad"] != "^1.3.0" || v.Dist.Integrity != sha512Integrity(widgetTarball) {
		t.Fatalf("packument = %s", rec.Body)
	}
	rec = s.do(t, http.MethodGet, strings.TrimPrefix(v.Dist.Tarball, "http://proxy.test"), "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != widgetTarball {
		t.Errorf("tarball download status = %d, want 200 and the published tarball", rec.Code)
	}
}

func TestNPMPublishRefusals(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		token      string
		body       string
		wantStatus int
		wantError  string
	}{
		{"no token", "/npm/@acme%2fwidget", "", publishDocument("@acme/widget", "1.0.0", widgetTarball, nil),
			http.StatusUnauthorized, ""},
		{"package outside the token's patterns", "/npm/@other%2fwidget", testScopedToken,
			publishDocument("@other/widget", "1.0.0", npmTarball(`{"name":"@other/widget","version":"1.0.0"}`), nil),
			http.StatusForbidden, "may not push npm package @other/widget"},
		{"not a publish document", "/npm/@acme%2fwidget", testScopedToken, "not json",
			http.StatusBadRequest, "not an npm publish document"},
		{"name differs from the URL", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/other", "1.0.0", widgetTarball, nil), http.StatusBadRequest, "doesn't match the URL"},
		{"metadata change without a tarball", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "1.0.0", widgetTarball, func(doc map[string]any) { delete(doc, "_attachments") }),
			http.StatusBadRequest, "only publishing a new version"},
		{"two versions", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "1.0.0", widgetTarball, func(doc map[string]any) {
				doc["versions"].(map[string]any)["2.0.0"] = map[string]any{}
			}), http.StatusBadRequest, "one version"},
		{"other dist-tag", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "1.0.0", widgetTarball, func(doc map[string]any) {
				doc["dist-tags"] = map[string]string{"beta": "1.0.0"}
			}), http.StatusBadRequest, "only the latest dist-tag"},
		{"integrity mismatch", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "1.0.0", widgetTarball, func(doc map[string]any) {
				doc["versions"].(map[string]any)["1.0.0"] = map[string]any{"dist": map[string]string{"integrity": sha512Integrity("other")}}
			}), http.StatusBadRequest, "doesn't match dist.integrity"},
		{"tarball for another version", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "2.0.0", widgetTarball, nil), http.StatusBadRequest, ""},
		{"too large", "/npm/@acme%2fwidget", testScopedToken,
			publishDocument("@acme/widget", "1.0.0", strings.Repeat("x", testHostedMaxUpload+1), nil),
			http.StatusRequestEntityTooLarge, "max_upload_size"},
		{"other registry endpoint", "/npm/-/user/org.couchdb.user:ci", testScopedToken, "{}",
			http.StatusMethodNotAllowed, "only npm publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newHostedTestServer(t)
			rec := s.do(t, http.MethodPut, tt.target, tt.token, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantError == "" {
				return
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, tt.wantError) {
				t.Errorf("error = %q, want it to contain %q", body.Error, tt.wantError)
			}
		})
	}
}

func TestNPMPublishTwiceIsRefused(t *testing.T) {
	s := newHostedTestServer(t)
	doc := publishDocument("@acme/widget", "1.0.0", widgetTarball, nil)
	if rec := s.do(t, http.MethodPut, "/npm/@acme%2fwidget", testScopedToken, doc); rec.Code != http.StatusCreated {
		t.Fatalf("first publish status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodPut, "/npm/@acme%2fwidget", testScopedToken, doc); rec.Code != http.StatusConflict {
		t.Fatalf("second publish status = %d, want 409, body %s", rec.Code, rec.Body)
	}
}

func TestHostedAPIChecksScopedTokens(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testScopedToken, requestTarball); rec.Code != http.StatusForbidden {
		t.Fatalf("push outside the token's patterns: status = %d, want 403, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodPut, "/api/hosted/npm/@acme/widget/1.0.0", testScopedToken, widgetTarball); rec.Code != http.StatusCreated {
		t.Fatalf("push inside the token's patterns: status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testHostedToken, requestTarball); rec.Code != http.StatusCreated {
		t.Fatalf("push with hosted.token: status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", testScopedToken, ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete outside the token's patterns: status = %d, want 403", rec.Code)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/@acme/widget/1.0.0", testScopedToken, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete inside the token's patterns: status = %d, want 204", rec.Code)
	}
}

func TestNPMDistMatches(t *testing.T) {
	const tarball = "tarball bytes"
	sha1Sum := sha1.Sum([]byte(tarball)) //nolint:gosec // npm's legacy dist.shasum is sha1
	shasum := hex.EncodeToString(sha1Sum[:])
	tests := []struct {
		name, integrity, shasum string
		want                    bool
	}{
		{"sha512 matches", sha512Integrity(tarball), "", true},
		{"one of several matches", "sha1-x " + sha512Integrity("other") + " " + sha512Integrity(tarball), "", true},
		{"sha512 differs", sha512Integrity("other"), shasum, false},
		{"shasum matches", "", shasum, true},
		{"shasum differs", "", strings.Repeat("0", 40), false},
		{"neither given", "", "", true},
	}
	for _, tt := range tests {
		if got := npmDistMatches(tt.integrity, tt.shasum, []byte(tarball)); got != tt.want {
			t.Errorf("%s: npmDistMatches = %v, want %v", tt.name, got, tt.want)
		}
	}
}
