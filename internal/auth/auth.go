// Package auth guards the HTTP surface.
//
// Sous generates container configuration and runs it, which makes it
// root-equivalent on its node by construction. config.go already refuses to
// bind 0.0.0.0 for that reason, treating the network boundary as the
// protection. This package is the second half of that argument: reaching the
// port should not be sufficient to drive it.
//
// THREE WAYS IN, because the callers are genuinely different.
//
//	session   a person in a browser, after signing in at /login. A signed
//	          cookie, verified on every later request.
//	bearer    a script. Adhoc tooling and API clients cannot fill in a form,
//	          and embedding the password in every curl is worse than a token
//	          that rotates on its own.
//	basic     retained for `curl -u`, which is how most HTTP libraries expose
//	          a lone secret.
//
// NO WWW-Authenticate IS EVER SENT. That header is what makes a browser render
// its native credential popup - unstyleable, unable to say why a login failed,
// and with no way to sign out short of restarting the browser. Browsers are
// redirected to a real form instead; scripts still get a bare 401, because a
// 302 to HTML would turn a failed API call into a confusing 200.
//
// Either credential satisfies any route. The split is about how callers
// present themselves, not about what they may do - an API token that could not
// deploy would be useless, and one that can deploy is already as powerful as
// the password.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
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
		// The login page itself must stay reachable, or the redirect below
		// bounces forever.
		if r.URL.Path == LoginPath {
			next.ServeHTTP(w, r)
			return
		}
		if c.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		// NO WWW-Authenticate. Sending it is what makes the browser show its
		// native credential popup, which cannot be styled, cannot show why a
		// login failed, and offers no way to log out. Browsers get redirected
		// to a real form instead.
		//
		// Scripts still get a bare 401: they cannot fill in a form, and a 302
		// to HTML would turn a failed API call into a confusing 200 carrying a
		// login page.
		if wantsHTML(r) {
			next := r.URL.RequestURI()
			// Only remember same-origin paths. Echoing an arbitrary ?next=
			// into a redirect is an open-redirect: a link to this panel could
			// bounce an authenticated operator to an attacker's page.
			if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
				next = "/"
			}
			http.Redirect(w, r, LoginPath+"?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// Authorized is exported so the login page can tell an already-signed-in
// visitor apart from a new one and skip a pointless form.
func (c Config) Authorized(r *http.Request) bool {
	if c.Disabled {
		return true
	}
	return c.authorized(r)
}

func (c Config) authorized(r *http.Request) bool {
	// Session cookie first: it is how a browser authenticates on every request
	// after the one login, and checking it before the credential paths avoids
	// re-deriving nothing on the common case.
	if ck, err := r.Cookie(CookieName); err == nil && c.validSession(ck.Value) {
		return true
	}
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

// wantsHTML distinguishes a browser from a script, which decides whether an
// unauthenticated request is redirected to the login form or answered 401.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// LoginPath is exported so the middleware and the handler cannot disagree
// about it - a mismatch there is an infinite redirect.
const LoginPath = "/login"
