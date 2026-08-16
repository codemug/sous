// Package auth guards the HTTP surface.
//
// Sous generates container configuration and runs it, which makes it
// root-equivalent on its node by construction. config.go already refuses to
// bind 0.0.0.0 for that reason, treating the network boundary as the
// protection. This package is the second half of that argument: reaching the
// port should not be sufficient to drive it.
//
// TWO CREDENTIALS, because the two callers are different.
//
//	basic auth  a person in a browser. Browsers know how to prompt for it, and
//	            it needs no client changes.
//	bearer      a script. Adhoc tooling, health checks and any client written
//	            against the API cannot answer a WWW-Authenticate challenge
//	            usefully, and embedding a password in every curl is worse than
//	            a token that can be rotated on its own.
//
// Either credential satisfies any route. The split is about how callers
// present themselves, not about what they are allowed to do - an API token
// that could not deploy would be useless, and one that can deploy is already
// as powerful as the password.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"
)

// Config is read from the environment rather than flags: flags are visible in
// ps output to every user on the box, and this project's compose files live in
// git where a flag value would be committed.
type Config struct {
	User     string
	Password string
	Token    string
	// Disabled is an EXPLICIT opt-out. Absent credentials are an error, not an
	// invitation - the same split between policy and safety that deploy and
	// larder deletion already use, where the dangerous path exists but has to
	// be asked for by name.
	Disabled bool
}

func FromEnv() (Config, error) {
	c := Config{
		User:     os.Getenv("SOUS_AUTH_USER"),
		Password: os.Getenv("SOUS_AUTH_PASS"),
		Token:    os.Getenv("SOUS_API_TOKEN"),
		Disabled: strings.EqualFold(os.Getenv("SOUS_AUTH"), "none"),
	}
	if c.Disabled {
		return c, nil
	}
	if c.User == "" && c.Token == "" {
		return c, errors.New(
			"auth: no credentials configured. Set SOUS_AUTH_USER and SOUS_AUTH_PASS " +
				"for the browser, and/or SOUS_API_TOKEN for scripts. To run without " +
				"authentication anyway, set SOUS_AUTH=none - an unauthenticated Sous " +
				"is root-equivalent to anyone who can reach its port")
	}
	if c.User != "" && c.Password == "" {
		return c, errors.New("auth: SOUS_AUTH_USER is set but SOUS_AUTH_PASS is empty")
	}
	return c, nil
}

// eq compares in constant time. A plain == leaks length and first-difference
// position through timing, which is exactly enough to guess a token byte by
// byte. Hashing first makes the comparison fixed-width regardless of input.
func eq(a, b string) bool {
	if b == "" {
		return false
	}
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// Middleware wraps a handler, rejecting anything without a valid credential.
//
// /healthz is deliberately exempt: a container healthcheck runs before any
// operator has configured anything, and a health probe that cannot answer
// makes the container restart-loop for a reason unrelated to its health. It
// discloses nothing but liveness.
func (c Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.Disabled || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if c.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Only challenge browsers. Sending WWW-Authenticate to a script makes
		// curl retry interactively and hang; a bare 401 is what a client can
		// actually act on.
		if wantsBrowserChallenge(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="sous", charset="UTF-8"`)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (c Config) authorized(r *http.Request) bool {
	if tok, ok := bearer(r); ok && eq(tok, c.Token) {
		return true
	}
	if u, p, ok := r.BasicAuth(); ok {
		// Both must match. Comparing only the password would let any username
		// through, and comparing only the user is obviously worse.
		if eq(u, c.User) && eq(p, c.Password) {
			return true
		}
		// A token presented as the basic-auth password is accepted too, because
		// that is how curl -u and most HTTP libraries expose a secret when the
		// caller has no username to give.
		if c.Token != "" && eq(p, c.Token) {
			return true
		}
	}
	return false
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return h[len(p):], true
	}
	// X-API-Token is accepted as a convenience for callers that already use
	// Authorization for something else.
	if v := r.Header.Get("X-API-Token"); v != "" {
		return v, true
	}
	return "", false
}

func wantsBrowserChallenge(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
