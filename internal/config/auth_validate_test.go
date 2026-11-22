package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnknownUpstreamAuthType(t *testing.T) {
	for _, typ := range []string{"token", "", "bearer "} {
		cfg := Default()
		cfg.Upstream.Auth = map[string]AuthConfig{
			"https://registry.example.com/private": {Type: typ, Token: "t"},
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("type %q: Validate() error = nil, want an unknown type error", typ)
		}
		for _, want := range []string{"upstream.auth", "https://registry.example.com/private", `"` + typ + `"`, "bearer, basic, header or ecr"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("type %q: Validate() error = %q, want it to contain %q", typ, err, want)
			}
		}
	}
}

func TestValidateAcceptsEveryUpstreamAuthType(t *testing.T) {
	for _, typ := range []string{"bearer", "basic", "header", "ecr", "Bearer", "ECR"} {
		cfg := Default()
		cfg.Upstream.Auth = map[string]AuthConfig{
			"https://registry.example.com": {Type: typ},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("type %q: Validate() error = %v", typ, err)
		}
	}
}

func TestWarningsUpstreamAuthWithoutCredentials(t *testing.T) {
	t.Setenv("AUTH_TEST_SET", "secret")
	t.Setenv("AUTH_TEST_EMPTY", "")

	cfg := Default()
	cfg.Upstream.Auth = map[string]AuthConfig{
		"https://ok-bearer.example.com":    {Type: "bearer", Token: "${AUTH_TEST_SET}"},
		"https://ok-basic.example.com":     {Type: "basic", Username: "u"},
		"https://ok-header.example.com":    {Type: "header", HeaderName: "X-Key", HeaderValue: "${AUTH_TEST_SET}"},
		"https://ok-ecr.example.com":       {Type: "ecr"},
		"https://no-token.example.com":     {Type: "bearer", Token: "${AUTH_TEST_EMPTY}"},
		"https://no-user.example.com":      {Type: "basic", Password: "p"},
		"https://no-name.example.com":      {Type: "header", HeaderValue: "v"},
		"https://no-value.example.com":     {Type: "header", HeaderName: "X-Key", HeaderValue: "${AUTH_TEST_EMPTY}"},
		"https://a-no-token.example.com/p": {Type: "Bearer"},
	}

	var got []string
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "upstream.auth") {
			got = append(got, w)
		}
	}
	want := []string{
		"https://a-no-token.example.com/p",
		"https://no-name.example.com",
		"https://no-token.example.com",
		"https://no-user.example.com",
		"https://no-value.example.com",
	}
	if len(got) != len(want) {
		t.Fatalf("Warnings() = %q, want one for each of %q", got, want)
	}
	for i, scope := range want {
		if !strings.Contains(got[i], `"`+scope+`"`) || !strings.Contains(got[i], "without credentials") {
			t.Errorf("warning %d = %q, want it to name %q and say requests go out without credentials", i, got[i], scope)
		}
		if strings.Contains(got[i], "secret") {
			t.Errorf("warning %d = %q leaks a credential", i, got[i])
		}
	}
}

// A scope can carry a credential of its own, which the messages naming the
// scope leave out.
func TestUpstreamAuthMessagesLeaveOutScopeCredentials(t *testing.T) {
	const scope = "https://user:scope-pass@registry.example.com/private?api_key=scope-secret"
	const shown = `"https://registry.example.com/private"`

	cfg := Default()
	cfg.Upstream.Auth = map[string]AuthConfig{scope: {Type: "bearer"}}
	var warning string
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "upstream.auth") {
			warning = w
		}
	}

	cfg.Upstream.Auth = map[string]AuthConfig{scope: {Type: "token"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an unknown type error")
	}

	for name, message := range map[string]string{"warning": warning, "error": err.Error()} {
		if !strings.Contains(message, shown) {
			t.Errorf("%s = %q, want it to name %s", name, message, shown)
		}
		for _, secret := range []string{"scope-pass", "scope-secret", "user:", "api_key"} {
			if strings.Contains(message, secret) {
				t.Errorf("%s = %q, want it without %q", name, message, secret)
			}
		}
	}
}

// An invalid scope is named without its credentials too.
func TestInvalidUpstreamAuthScopeLeavesOutItsCredentials(t *testing.T) {
	for scope, shown := range map[string]string{
		"https://user:scope-pass@/private?api_key=scope-secret": `"https:///private"`,
		"https://user:scope-pass@host/%zz?api_key=scope-secret": `"(invalid URL)"`,
	} {
		cfg := Default()
		cfg.Upstream.Auth = map[string]AuthConfig{scope: {Type: "bearer", Token: "t"}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%s: Validate() error = nil, want an invalid URL error", shown)
		}
		if !strings.Contains(err.Error(), shown) {
			t.Errorf("Validate() error = %q, want it to name %s", err, shown)
		}
		for _, secret := range []string{"scope-pass", "scope-secret", "user:", "api_key"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Validate() error = %q, want it without %q", err, secret)
			}
		}
	}
}
