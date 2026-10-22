package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"path"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
)

const (
	bearerScheme = "bearer "
	basicScheme  = "basic "
)

// hostedPushPattern is one "{ecosystem}:{pattern}" entry of a token's push
// list.
type hostedPushPattern struct {
	ecosystem string
	name      string
}

// hostedCredential is a token that may push. A token without patterns, the
// hosted.token one, may push anything.
type hostedCredential struct {
	name     string
	digest   [sha256.Size]byte
	patterns []hostedPushPattern
}

// mayPush reports whether the credential may push or delete name in
// ecosystem. Composer names are case-insensitive, so they are compared
// lowercased.
func (c *hostedCredential) mayPush(ecosystem, name string) bool {
	if c.patterns == nil {
		return true
	}
	if ecosystem == "composer" {
		name = strings.ToLower(name)
	}
	for _, p := range c.patterns {
		pattern := p.name
		if ecosystem == "composer" {
			pattern = strings.ToLower(pattern)
		}
		if p.ecosystem == ecosystem {
			if ok, _ := path.Match(pattern, name); ok {
				return true
			}
		}
	}
	return false
}

// hostedAuth checks the tokens configured under hosted.token and
// hosted.tokens.
type hostedAuth struct {
	credentials []hostedCredential
}

func newHostedAuth(cfg config.HostedConfig) *hostedAuth {
	a := &hostedAuth{}
	if token := cfg.TokenExpanded(); token != "" {
		a.credentials = append(a.credentials, hostedCredential{name: "hosted.token", digest: sha256.Sum256([]byte(token))})
	}
	for _, t := range cfg.Tokens {
		c := hostedCredential{name: t.Name, digest: sha256.Sum256([]byte(t.TokenExpanded())), patterns: []hostedPushPattern{}}
		for _, pattern := range t.Push {
			ecosystem, name, _ := config.SplitHostedPushPattern(pattern)
			c.patterns = append(c.patterns, hostedPushPattern{ecosystem: ecosystem, name: name})
		}
		a.credentials = append(a.credentials, c)
	}
	return a
}

// authenticate returns the credential the request presents. Every
// configured token is compared, each through its hash, so the time taken
// doesn't depend on which one matched or on the presented token's length.
func (a *hostedAuth) authenticate(r *http.Request) (*hostedCredential, bool) {
	presented, ok := presentedToken(r)
	if !ok {
		return nil, false
	}
	got := sha256.Sum256([]byte(presented))
	var match *hostedCredential
	for i := range a.credentials {
		if subtle.ConstantTimeCompare(got[:], a.credentials[i].digest[:]) == 1 {
			match = &a.credentials[i]
		}
	}
	return match, match != nil
}

// presentedToken returns the token in the Authorization header in the forms
// package clients send it: "Bearer <token>" (npm), Basic with the token as the
// password (twine, Maven), or the bare token (cargo).
func presentedToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	switch {
	case len(header) >= len(bearerScheme) && strings.EqualFold(header[:len(bearerScheme)], bearerScheme):
		token := header[len(bearerScheme):]
		return token, token != ""
	case len(header) >= len(basicScheme) && strings.EqualFold(header[:len(basicScheme)], basicScheme):
		decoded, err := base64.StdEncoding.DecodeString(header[len(basicScheme):])
		if err != nil {
			return "", false
		}
		_, password, ok := strings.Cut(string(decoded), ":")
		return password, ok && password != ""
	default:
		return header, header != "" && !strings.Contains(header, " ")
	}
}

type hostedCredentialKey struct{}

// credentialFrom returns the credential requireHostedToken stored.
func credentialFrom(ctx context.Context) *hostedCredential {
	c, _ := ctx.Value(hostedCredentialKey{}).(*hostedCredential)
	return c
}

// requireHostedToken rejects requests that carry no configured token and
// passes the credential on to the handler, which checks it against the
// package the request names.
func requireHostedToken(auth *hostedAuth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential, ok := auth.authenticate(r)
			if !ok {
				w.Header().Add("WWW-Authenticate", `Bearer realm="proxy"`)
				w.Header().Add("WWW-Authenticate", `Basic realm="proxy"`)
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "a valid token is required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hostedCredentialKey{}, credential)))
		})
	}
}

// mayPush reports whether the request's credential may push or delete name
// in ecosystem.
func mayPush(r *http.Request, ecosystem, name string) bool {
	c := credentialFrom(r.Context())
	return c != nil && c.mayPush(ecosystem, name)
}

func mayNotPushMessage(ecosystem, name string) string {
	return "this token may not push " + ecosystem + " package " + name
}
