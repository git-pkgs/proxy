package chef

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const proxy = "https://proxy.example"

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

func TestRewriteUniverse(t *testing.T) {
	body := []byte(`{"apt":{"7.4.0":{"location_type":"opscode","location_path":"https://supermarket.chef.io/api/v1","download_url":"https://supermarket.chef.io/api/v1/cookbooks/apt/versions/7.4.0/download","dependencies":{"compat_resource":">= 12.16.3"}},"7.3.0":null}}`)
	out, err := RewriteUniverse(body, Options{ProxyURL: proxy})
	if err != nil {
		t.Fatal(err)
	}
	entry := decode(t, out)["apt"].(map[string]any)["7.4.0"].(map[string]any)
	if got := entry["location_path"]; got != proxy+"/chef/api/v1" {
		t.Errorf("location_path = %v", got)
	}
	if got := entry["download_url"]; got != proxy+"/chef/api/v1/cookbooks/apt/versions/7.4.0/download" {
		t.Errorf("download_url = %v", got)
	}
	if got := entry["location_type"]; got != "opscode" {
		t.Errorf("location_type = %v", got)
	}
	if got := entry["dependencies"].(map[string]any)["compat_resource"]; got != ">= 12.16.3" {
		t.Errorf("dependency = %v", got)
	}
	if !strings.Contains(string(out), `">= 12.16.3"`) {
		t.Errorf("constraint not kept as written in %s", out)
	}
	if v, ok := decode(t, out)["apt"].(map[string]any)["7.3.0"]; !ok || v != nil {
		t.Errorf("null entry = %v, %v", v, ok)
	}
}

func TestRewriteUniverseRefusesOtherShapes(t *testing.T) {
	for _, body := range []string{`[]`, `{"apt":[]}`, `not json`} {
		if _, err := RewriteUniverse([]byte(body), Options{ProxyURL: proxy}); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestRewriteCookbook(t *testing.T) {
	body := []byte(`{"name":"apt","latest_version":"https://supermarket.chef.io/api/v1/cookbooks/apt/versions/7.4.0","versions":["https://supermarket.chef.io/api/v1/cookbooks/apt/versions/7_4_0","https://supermarket.chef.io/api/v1/cookbooks/apt/versions/latest",null],"description":"<b>"}`)
	out, err := RewriteCookbook("apt", body, Options{ProxyURL: proxy})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if got := m["latest_version"]; got != proxy+"/chef/api/v1/cookbooks/apt/versions/7.4.0" {
		t.Errorf("latest_version = %v", got)
	}
	versions := m["versions"].([]any)
	if versions[0] != proxy+"/chef/api/v1/cookbooks/apt/versions/7.4.0" {
		t.Errorf("versions[0] = %v", versions[0])
	}
	if versions[1] != "https://supermarket.chef.io/api/v1/cookbooks/apt/versions/latest" {
		t.Errorf("versions[1] = %v, want unchanged", versions[1])
	}
	if versions[2] != nil {
		t.Errorf("versions[2] = %v", versions[2])
	}
	if !strings.Contains(string(out), `"description":"<b>"`) {
		t.Errorf("description = %v in %s", m["description"], out)
	}
}

func TestRewriteCookbookWithoutVersionFields(t *testing.T) {
	out, err := RewriteCookbook("apt", []byte(`{"name":"apt"}`), Options{ProxyURL: proxy})
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); len(m) != 1 || m["name"] != "apt" {
		t.Errorf("got %s", out)
	}
}

func TestRewriteCookbookRefusesOtherShapes(t *testing.T) {
	for _, body := range []string{`[]`, `{"versions":"x"}`} {
		if _, err := RewriteCookbook("apt", []byte(body), Options{ProxyURL: proxy}); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestRewriteVersion(t *testing.T) {
	body := []byte(`{"version":"7.4.0","cookbook":"https://supermarket.chef.io/api/v1/cookbooks/apt","file":"https://supermarket.chef.io/api/v1/cookbooks/apt/versions/7.4.0/download","license":"Apache-2.0"}`)
	out, err := RewriteVersion("apt", "7.4.0", body, Options{ProxyURL: proxy})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["cookbook"] != proxy+"/chef/api/v1/cookbooks/apt" {
		t.Errorf("cookbook = %v", m["cookbook"])
	}
	if m["file"] != proxy+"/chef/api/v1/cookbooks/apt/versions/7.4.0/download" {
		t.Errorf("file = %v", m["file"])
	}
	if m["license"] != "Apache-2.0" {
		t.Errorf("license = %v", m["license"])
	}
}

func TestRewriteVersionAddsNoFields(t *testing.T) {
	out, err := RewriteVersion("apt", "7.4.0", []byte(`{"version":"7.4.0"}`), Options{ProxyURL: proxy})
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, out); len(m) != 1 {
		t.Errorf("got %s", out)
	}
	if _, err := RewriteVersion("apt", "7.4.0", []byte(`[]`), Options{ProxyURL: proxy}); !errors.Is(err, ErrDocument) {
		t.Errorf("err = %v", err)
	}
}

func TestRewriteVersionURLLeavesOthersAlone(t *testing.T) {
	for _, raw := range []string{`null`, `42`, `"%zz"`, `"https://x/versions/latest"`} {
		out, err := RewriteVersionURL("apt", json.RawMessage(raw), Options{ProxyURL: proxy})
		if err != nil || string(out) != raw {
			t.Errorf("%s: got %s, %v", raw, out, err)
		}
	}
}

func TestURLsEscapeNames(t *testing.T) {
	if got := DownloadURL(proxy, "a b", "1.0"); got != proxy+"/chef/api/v1/cookbooks/a%20b/versions/1.0/download" {
		t.Errorf("DownloadURL = %s", got)
	}
	if got := VersionURL(proxy, "apt", "1/0"); got != proxy+"/chef/api/v1/cookbooks/apt/versions/1%2F0" {
		t.Errorf("VersionURL = %s", got)
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{"1.2.3": "1.2.3", "1_2_3": "1.2.3", "1.2": "1.2", "10_0": "10.0"} {
		if got, ok := NormalizeVersion(in); !ok || got != want {
			t.Errorf("%s: got %s, %v", in, got, ok)
		}
	}
	for _, in := range []string{"1", "1.2.3.4", "latest", "v1.2", "1.2.x", "", "1.2.3 "} {
		if got, ok := NormalizeVersion(in); ok {
			t.Errorf("%s: accepted as %s", in, got)
		}
	}
}

func TestEncodeJSONKeepsHTMLCharacters(t *testing.T) {
	out, err := EncodeJSON(map[string]string{"c": ">= 1.0 & <2"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"c":">= 1.0 & <2"}` {
		t.Errorf("got %s", out)
	}
	if _, err := EncodeJSON(func() {}); err == nil {
		t.Error("encoded a func")
	}
}
