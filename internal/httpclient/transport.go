// Package httpclient provides authentication-aware HTTP transports for upstream requests.
package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultTokenLifetime  = 60 * time.Second
	tokenExpirySkew       = 5 * time.Second
	maxTokenResponseSize  = 1 << 20
	shortTokenSkewDivisor = 10
)

// AuthFunc returns a configured authentication header for a URL.
type AuthFunc func(url string) (headerName, headerValue string)

// Transport adds configured authentication and follows OCI Bearer challenges.
type Transport struct {
	base       http.RoundTripper
	authForURL AuthFunc

	mu         sync.Mutex
	tokens     map[string]cachedToken
	challenges map[string]bearerChallenge
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type bearerChallenge struct {
	realm   string
	service string
	scopes  []string
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// NewTransport creates an authentication-aware transport around base.
func NewTransport(base http.RoundTripper, authForURL AuthFunc) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{
		base:       base,
		authForURL: authForURL,
		tokens:     make(map[string]cachedToken),
		challenges: make(map[string]bearerChallenge),
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	outbound := cloneRequest(req)
	t.applyAuthentication(outbound, req.Header.Get("Authorization") != "")

	resp, err := t.base.RoundTrip(outbound)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if registryProtectionSpace(req.URL) == "" {
		return resp, nil
	}

	challenge, ok := parseBearerChallenge(resp.Header.Values("WWW-Authenticate"))
	if !ok || !canReplay(req) {
		return resp, nil
	}

	drainAndClose(resp.Body)
	token, err := t.token(req.Context(), challenge)
	if err != nil {
		return nil, fmt.Errorf("registry authentication: %w", err)
	}
	t.rememberChallenge(req.URL, challenge)

	retry, err := cloneRequestForRetry(req)
	if err != nil {
		return nil, err
	}
	t.applyConfiguredAuthentication(retry)
	retry.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(retry)
}

func (t *Transport) applyAuthentication(req *http.Request, hasExplicitAuthorization bool) {
	t.applyConfiguredAuthentication(req)
	if hasExplicitAuthorization {
		return
	}
	if token := t.cachedTokenForRequest(req.URL); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (t *Transport) applyConfiguredAuthentication(req *http.Request) {
	if t.authForURL == nil {
		return
	}
	name, value := t.authForURL(req.URL.String())
	if name != "" && value != "" && req.Header.Get(name) == "" {
		req.Header.Set(name, value)
	}
}

func (t *Transport) token(ctx context.Context, challenge bearerChallenge) (string, error) {
	key := challenge.key()
	if token := t.cachedToken(key); token != "" {
		return token, nil
	}

	token, expiresAt, err := t.fetchToken(ctx, challenge)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	t.tokens[key] = cachedToken{value: token, expiresAt: expiresAt}
	t.mu.Unlock()
	return token, nil
}

func (t *Transport) fetchToken(ctx context.Context, challenge bearerChallenge) (string, time.Time, error) {
	tokenURL, err := url.Parse(challenge.realm)
	if err != nil || !tokenURL.IsAbs() || (tokenURL.Scheme != "https" && tokenURL.Scheme != "http") {
		return "", time.Time{}, fmt.Errorf("invalid token realm %q", challenge.realm)
	}

	query := tokenURL.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	for _, scope := range challenge.scopes {
		query.Add("scope", scope)
	}
	query.Set("client_id", "git-pkgs-proxy")
	tokenURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", time.Time{}, err
	}

	client := &http.Client{Transport: configuredTransport{parent: t}}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("requesting token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseSize))
		return "", time.Time{}, fmt.Errorf("token service returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseSize)).Decode(&payload); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding token response: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", time.Time{}, fmt.Errorf("token response did not contain a token")
	}

	issuedAt := time.Now()
	if payload.IssuedAt != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, payload.IssuedAt); parseErr == nil {
			issuedAt = parsed
		}
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultTokenLifetime
	}
	expiresAt := issuedAt.Add(lifetime).Add(-expirySkew(lifetime))
	return token, expiresAt, nil
}

type configuredTransport struct {
	parent *Transport
}

func (t configuredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	outbound := cloneRequest(req)
	t.parent.applyConfiguredAuthentication(outbound)
	return t.parent.base.RoundTrip(outbound)
}

func (t *Transport) cachedTokenForRequest(requestURL *url.URL) string {
	space := registryProtectionSpace(requestURL)
	if space == "" {
		return ""
	}

	t.mu.Lock()
	challenge, ok := t.challenges[space]
	t.mu.Unlock()
	if !ok {
		return ""
	}
	return t.cachedToken(challenge.key())
}

func (t *Transport) cachedToken(key string) string {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	token, ok := t.tokens[key]
	if !ok {
		return ""
	}
	if !now.Before(token.expiresAt) {
		delete(t.tokens, key)
		return ""
	}
	return token.value
}

func (t *Transport) rememberChallenge(requestURL *url.URL, challenge bearerChallenge) {
	space := registryProtectionSpace(requestURL)
	if space == "" {
		return
	}
	t.mu.Lock()
	t.challenges[space] = challenge
	t.mu.Unlock()
}

func (c bearerChallenge) key() string {
	return c.realm + "\x00" + c.service + "\x00" + strings.Join(c.scopes, "\x00")
}

func registryProtectionSpace(u *url.URL) string {
	const registryPrefix = "/v2/"
	if u == nil || !strings.HasPrefix(u.Path, registryPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(u.Path, registryPrefix)
	end := len(rest)
	for _, marker := range []string{"/blobs/", "/manifests/", "/tags/", "/referrers/"} {
		if index := strings.Index(rest, marker); index >= 0 && index < end {
			end = index
		}
	}
	if end == len(rest) || end == 0 {
		return ""
	}
	return u.Scheme + "://" + u.Host + registryPrefix + rest[:end]
}

func parseBearerChallenge(values []string) (bearerChallenge, bool) {
	for _, value := range values {
		params, ok := bearerParameters(value)
		if !ok || params["realm"] == "" {
			continue
		}
		challenge := bearerChallenge{
			realm:   params["realm"],
			service: params["service"],
		}
		if scope := params["scope"]; scope != "" {
			challenge.scopes = append(challenge.scopes, scope)
		}
		return challenge, true
	}
	return bearerChallenge{}, false
}

func bearerParameters(value string) (map[string]string, bool) {
	start := findAuthScheme(value, "Bearer")
	if start < 0 {
		return nil, false
	}
	rest := value[start+len("Bearer"):]
	params := make(map[string]string)
	for {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			break
		}

		keyEnd := strings.IndexAny(rest, "= \t,")
		if keyEnd <= 0 {
			break
		}
		key := strings.ToLower(rest[:keyEnd])
		rest = strings.TrimLeft(rest[keyEnd:], " \t")
		if rest == "" || rest[0] != '=' {
			break
		}
		rest = strings.TrimLeft(rest[1:], " \t")

		parsed, remaining, ok := parseAuthValue(rest)
		if !ok {
			return nil, false
		}
		params[key] = parsed
		rest = remaining
	}
	return params, true
}

func findAuthScheme(value, scheme string) int {
	inQuote := false
	escaped := false
	for index := 0; index+len(scheme) <= len(value); index++ {
		char := value[index]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && inQuote {
			escaped = true
			continue
		}
		if char == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote || !strings.EqualFold(value[index:index+len(scheme)], scheme) {
			continue
		}
		beforeOK := index == 0 || value[index-1] == ',' || value[index-1] == ' ' || value[index-1] == '\t'
		after := index + len(scheme)
		afterOK := after < len(value) && (value[after] == ' ' || value[after] == '\t')
		if beforeOK && afterOK {
			return index
		}
	}
	return -1
}

func parseAuthValue(value string) (parsed, remaining string, ok bool) {
	if value == "" {
		return "", "", false
	}
	if value[0] != '"' {
		end := strings.IndexAny(value, " \t,")
		if end < 0 {
			return value, "", true
		}
		return value[:end], value[end:], end > 0
	}

	var builder strings.Builder
	escaped := false
	for index := 1; index < len(value); index++ {
		char := value[index]
		if escaped {
			builder.WriteByte(char)
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			return builder.String(), value[index+1:], true
		}
		builder.WriteByte(char)
	}
	return "", "", false
}

func cloneRequest(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	return clone
}

func canReplay(req *http.Request) bool {
	return req.Body == nil || req.GetBody != nil
}

func cloneRequestForRetry(req *http.Request) (*http.Request, error) {
	clone := cloneRequest(req)
	if req.Body == nil {
		return clone, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("replaying authenticated request: %w", err)
	}
	clone.Body = body
	return clone, nil
}

func expirySkew(lifetime time.Duration) time.Duration {
	if lifetime < tokenExpirySkew*2 {
		return lifetime / shortTokenSkewDivisor
	}
	return tokenExpirySkew
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxTokenResponseSize))
	_ = body.Close()
}
