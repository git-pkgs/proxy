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
