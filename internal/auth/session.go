package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie. Prefixed to make its scope obvious in a
// browser's storage inspector alongside cookies from other tailnet services.
const CookieName = "sous_session"

// SessionTTL is how long a browser login lasts.
//
// Seven days rather than a few hours: this is a single-operator ops panel on a
// tailnet, and the realistic threat is not a stolen laptop cookie but being
// locked out mid-incident by an expiry. Rotating either credential invalidates
// every session immediately, which is the control that actually matters here.
const SessionTTL = 7 * 24 * time.Hour

// sessionKey derives the signing key from the configured credentials.
//
// DERIVED, NOT SEPARATE, and that is the useful property: changing the password
// or the token changes the key, so every existing session stops verifying. A
// standalone SOUS_SESSION_KEY would have to be rotated by hand and would
// otherwise let sessions outlive the password they were issued against.
//
// The password is never stored in the cookie - only this key is derived from
// it, and the key never leaves the process.
func (c Config) sessionKey() []byte {
	h := sha256.New()
	h.Write([]byte("sous-session-v1\x00"))
	h.Write([]byte(c.User))
	h.Write([]byte{0})
	h.Write([]byte(c.Password))
	h.Write([]byte{0})
	h.Write([]byte(c.Token))
	return h.Sum(nil)
}

// MintSession returns a signed token carrying the user and an expiry.
//
// Stateless on purpose. A server-side store would need to survive restarts to
// avoid logging everyone out on deploy, and this service redeploys often; an
// HMAC over the expiry gives the same guarantee with nothing to persist.
func (c Config) MintSession(user string) string {
	payload := fmt.Sprintf("%s|%d", user, time.Now().Add(SessionTTL).Unix())
	mac := hmac.New(sha256.New, c.sessionKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validSession reports whether a cookie value is one this process issued and
// has not expired.
func (c Config) validSession(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, c.sessionKey())
	mac.Write(payload)
	// Constant-time: a byte-by-byte comparison here leaks how much of a forged
	// signature was correct, which is enough to construct one.
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	i := strings.LastIndexByte(string(payload), '|')
	if i < 0 {
		return false
	}
	exp, err := strconv.ParseInt(string(payload)[i+1:], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// SetSessionCookie issues the cookie after a successful login.
func (c Config) SetSessionCookie(w http.ResponseWriter, r *http.Request, user string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    c.MintSession(user),
		Path:     "/",
		HttpOnly: true,
		// Lax, not Strict: Strict would drop the cookie when arriving from an
		// external link - including the homepage dashboard tile - and the
		// operator would appear logged out for no reason. Lax still blocks the
		// cross-site POST that CSRF needs.
		SameSite: http.SameSiteLaxMode,
		// Secure ONLY over TLS. This panel is reachable over plain HTTP on the
		// tailnet, and a Secure cookie there is silently dropped by the browser
		// - which presents as a login form that accepts the password and then
		// returns you to the login form.
		Secure:  isTLS(r),
		Expires: time.Now().Add(SessionTTL),
	})
}

// ClearSessionCookie logs the browser out.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isTLS(r),
		MaxAge: -1,
	})
}

// CheckPassword reports whether a submitted login is valid.
func (c Config) CheckPassword(user, pass string) bool {
	return eq(user, c.User) && eq(pass, c.Password)
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Behind `tailscale serve`, TLS terminates upstream and the request reaches
	// this process as plain HTTP; the forwarded header is the only signal.
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
