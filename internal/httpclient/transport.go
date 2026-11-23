// Package httpclient provides authentication-aware HTTP transports for upstream requests.
package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	tokenMaxRetries       = 3
	tokenRetryBaseDelay   = 500 * time.Millisecond

	// maxTokenLifetime caps how long a token is reused, whatever lifetime
	// the token service claims.
	maxTokenLifetime = time.Hour
	// maxCachedTokens and maxCachedChallenges bound the token and
	// challenge caches, which grow with every repository and registry
	// host the proxy pulls from.
	maxCachedTokens     = 1024
	maxCachedChallenges = 256
)

// AuthFunc returns a configured authentication header for a URL.
type AuthFunc func(url string) (headerName, headerValue string)

// Transport adds configured authentication and follows OCI Bearer challenges.
type Transport struct {
	base       http.RoundTripper
	authForURL AuthFunc
	retryWait  func(context.Context, time.Duration) error

	mu         sync.Mutex
	tokens     map[string]cachedToken
	challenges map[string]bearerChallenge // by registry origin, without scopes
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
		retryWait:  waitForRetry,
		tokens:     make(map[string]cachedToken),
		challenges: make(map[string]bearerChallenge),
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.redirectKeepsCredentials(req) {
		return t.base.RoundTrip(req)
	}
	hasExplicitAuthorization := req.Header.Get("Authorization") != ""
	outbound := cloneRequest(req)
	t.applyAuthentication(outbound, hasExplicitAuthorization)

	resp, err := t.base.RoundTrip(outbound)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if hasExplicitAuthorization {
		return resp, nil
	}
	repository := registryRepository(req.URL)
	if repository == "" {
		return resp, nil
	}

	challenge, ok := parseBearerChallenge(resp.Header.Values("WWW-Authenticate"))
	// Ask only for pull access to the repository being fetched, whatever
	// scope the challenge names: the proxy only reads.
	challenge.scopes = []string{pullScope(repository)}
	if !ok || !canReplay(req) || !t.trustedWithToken(req.URL, challenge) {
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

// redirectKeepsCredentials reports whether a redirect hop has the same
// configured credentials as the request the redirects started from, the
// only one whose route was checked against generic_paths and git repos.
func (t *Transport) redirectKeepsCredentials(req *http.Request) bool {
	if t.authForURL == nil {
		return true
	}
	name, value := t.authForURL(req.URL.String())
	if name == "" || value == "" {
		return true
	}
	origin := req
	for origin.Response != nil && origin.Response.Request != nil {
		origin = origin.Response.Request
	}
	originName, originValue := t.authForURL(origin.URL.String())
	return sameHeader(name, value, originName, originValue)
}

// trustedWithToken reports whether a token from challenge's realm may be
// fetched for, and sent to, the registry at requestURL. The challenge comes
// from that registry, which can be any host the proxy fetches from, such as
// one named in a Composer dist URL or a Helm index. Credentials configured for
// the realm would otherwise be turned into a token handed to that host. A
// token from a realm with no configured credentials is anonymous, and any
// registry may have it. A credentialed token only goes to the realm's own
// host, to a registry configured with the same credentials as the realm, or
// to a registry in knownTokenServices.
func (t *Transport) trustedWithToken(requestURL *url.URL, challenge bearerChallenge) bool {
	if t.authForURL == nil {
		return true
	}
	realmName, realmValue := t.authForURL(challenge.realm)
	if realmName == "" || realmValue == "" {
		return true
	}
	if name, value := t.authForURL(requestURL.String()); sameHeader(name, value, realmName, realmValue) {
		return true
	}
	realm, err := url.Parse(challenge.realm)
	return err == nil && sameTokenService(requestURL, realm)
}

// sameHeader reports whether two configured authentication headers are the
// same credentials. Header names are case-insensitive.
func sameHeader(name, value, otherName, otherValue string) bool {
	return strings.EqualFold(name, otherName) && value == otherValue
}

// knownTokenServices maps registry hosts to the token service host they
// use where the two differ and the registry cannot be configured with the
// token service's credentials under the same upstream.auth key.
var knownTokenServices = map[string]string{
	"registry-1.docker.io": "auth.docker.io",
	"index.docker.io":      "auth.docker.io",
	"registry.gitlab.com":  "gitlab.com",
}

// sameTokenService reports whether a token service at realm belongs to the
// registry at registry: both on the same scheme, host and port, or a known
// pair of registry and token service hosts.
func sameTokenService(registry, realm *url.URL) bool {
	if strings.EqualFold(registry.Scheme, realm.Scheme) && strings.EqualFold(registry.Host, realm.Host) {
		return true
	}
	if registry.Scheme != "https" || realm.Scheme != "https" || registry.Port() != "" || realm.Port() != "" {
		return false
	}
	want, ok := knownTokenServices[strings.ToLower(registry.Hostname())]
	return ok && strings.EqualFold(realm.Hostname(), want)
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

	t.cacheToken(key, cachedToken{value: token, expiresAt: expiresAt})
	return token, nil
}

func (t *Transport) cacheToken(key string, token cachedToken) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	for cachedKey, cached := range t.tokens {
		if !now.Before(cached.expiresAt) {
			delete(t.tokens, cachedKey)
		}
	}
	if _, ok := t.tokens[key]; !ok && len(t.tokens) >= maxCachedTokens {
		// Drop the token that expires soonest.
		var oldestKey string
		var oldest time.Time
		for cachedKey, cached := range t.tokens {
			if oldestKey == "" || cached.expiresAt.Before(oldest) {
				oldestKey, oldest = cachedKey, cached.expiresAt
			}
		}
		delete(t.tokens, oldestKey)
	}
	t.tokens[key] = token
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

	client := &http.Client{Transport: configuredTransport{parent: t, realm: challenge.realm}}
	for attempt := 0; attempt <= tokenMaxRetries; attempt++ {
		resp, err := client.Do(req.Clone(ctx))
		if err != nil {
			requestErr := fmt.Errorf("requesting token: %w", err)
			if !shouldRetryTokenRequest(ctx, err) || attempt == tokenMaxRetries {
				return "", time.Time{}, requestErr
			}
			if err := t.waitForTokenRetry(ctx, attempt); err != nil {
				return "", time.Time{}, err
			}
			continue
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return decodeTokenResponse(resp)
		}

		responseErr := tokenResponseError(resp)
		if !shouldRetryTokenStatus(resp.StatusCode) || attempt == tokenMaxRetries {
			return "", time.Time{}, responseErr
		}
		if err := t.waitForTokenRetry(ctx, attempt); err != nil {
			return "", time.Time{}, err
		}
	}

	return "", time.Time{}, errors.New("token request retries exhausted")
}

func decodeTokenResponse(resp *http.Response) (string, time.Time, error) {
	defer func() { _ = resp.Body.Close() }()

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
	lifetime = min(lifetime, maxTokenLifetime)
	expiresAt := issuedAt.Add(lifetime).Add(-expirySkew(lifetime))
	return token, expiresAt, nil
}

func tokenResponseError(resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseSize))
	return fmt.Errorf("token service returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func shouldRetryTokenRequest(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var networkErr net.Error
	if !errors.As(err, &networkErr) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	return networkErr.Timeout()
}

func shouldRetryTokenStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func (t *Transport) waitForTokenRetry(ctx context.Context, attempt int) error {
	delay := tokenRetryBaseDelay << attempt
	if t.retryWait != nil {
		return t.retryWait(ctx, delay)
	}
	return waitForRetry(ctx, delay)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// configuredTransport makes a token request to realm, adding the
// credentials upstream.auth has for it. trustedWithToken decided on those
// credentials, so a redirect only gets credentials when they are the same:
// a realm without any could otherwise redirect to a token service the
// proxy has credentials for, and have a token issued with them.
type configuredTransport struct {
	parent *Transport
	realm  string
}

func (t configuredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	outbound := cloneRequest(req)
	if t.sameCredentials(outbound.URL) {
		t.parent.applyConfiguredAuthentication(outbound)
	}
	return t.parent.base.RoundTrip(outbound)
}

// sameCredentials reports whether upstream.auth gives u the credentials it
// gives the realm.
func (t configuredTransport) sameCredentials(u *url.URL) bool {
	if t.parent.authForURL == nil {
		return true
	}
	name, value := t.parent.authForURL(u.String())
	realmName, realmValue := t.parent.authForURL(t.realm)
	return sameHeader(name, value, realmName, realmValue)
}

func (t *Transport) cachedTokenForRequest(requestURL *url.URL) string {
	repository := registryRepository(requestURL)
	if repository == "" {
		return ""
	}

	t.mu.Lock()
	challenge, ok := t.challenges[registryOrigin(requestURL)]
	t.mu.Unlock()
	if !ok {
		return ""
	}
	challenge.scopes = []string{pullScope(repository)}
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

// rememberChallenge records the token service a registry named, so later
// requests to any of its repositories can send a cached token up front.
func (t *Transport) rememberChallenge(requestURL *url.URL, challenge bearerChallenge) {
	origin := registryOrigin(requestURL)
	challenge.scopes = nil
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.challenges[origin]; !ok && len(t.challenges) >= maxCachedChallenges {
		for evict := range t.challenges {
			delete(t.challenges, evict)
			break
		}
	}
	t.challenges[origin] = challenge
}

func (c bearerChallenge) key() string {
	return c.realm + "\x00" + c.service + "\x00" + strings.Join(c.scopes, "\x00")
}

func registryOrigin(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func pullScope(repository string) string {
	return "repository:" + repository + ":pull"
}

// registryRepository returns the repository name from an OCI distribution
// API path, such as library/alpine from /v2/library/alpine/manifests/latest,
// or "" if the path is not one.
func registryRepository(u *url.URL) string {
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
	return rest[:end]
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
