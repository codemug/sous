package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func ok(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
}

func do(t *testing.T, c Config, build func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/recipes", nil)
	if build != nil {
		build(req)
	}
	rr := httptest.NewRecorder()
	c.Middleware(ok(t)).ServeHTTP(rr, req)
	return rr
}

// The property the whole package exists for.
func TestRejectsUnauthenticated(t *testing.T) {
	c := Config{User: "u", Password: "p", Token: "t"}
	if got := do(t, c, nil).Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

func TestAcceptsBasicAuth(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	rr := do(t, c, func(r *http.Request) { r.SetBasicAuth("u", "p") })
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// A right password with a wrong username must fail. Checking only the password
// would make the username decorative.
func TestRejectsWrongUsernameWithRightPassword(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	rr := do(t, c, func(r *http.Request) { r.SetBasicAuth("someone-else", "p") })
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAcceptsBearerToken(t *testing.T) {
	c := Config{User: "u", Password: "p", Token: "secret-token"}
	rr := do(t, c, func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret-token") })
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// curl -u :TOKEN and most HTTP libraries put a lone secret in the password
// field. Rejecting that shape would make the token awkward for exactly the
// callers it exists for.
func TestAcceptsTokenAsBasicPassword(t *testing.T) {
	c := Config{User: "u", Password: "p", Token: "secret-token"}
	rr := do(t, c, func(r *http.Request) { r.SetBasicAuth("", "secret-token") })
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestRejectsWrongToken(t *testing.T) {
	c := Config{Token: "secret-token"}
	rr := do(t, c, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") })
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// An empty configured secret must never act as a wildcard. Without the guard
// in eq(), a Config with no Token would accept an empty bearer.
func TestEmptyConfiguredSecretMatchesNothing(t *testing.T) {
	c := Config{User: "u", Password: "p"} // Token deliberately empty
	for _, probe := range []string{"", "anything"} {
		rr := do(t, c, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+probe) })
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q got %d, want 401", probe, rr.Code)
		}
	}
}

// The healthcheck runs before an operator has configured anything, and a probe
// that cannot answer restart-loops the container for a reason unrelated to its
// health.
func TestHealthzIsExempt(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	c.Middleware(ok(t)).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", rr.Code)
	}
}

// The popup is gone ON PURPOSE. Sending WWW-Authenticate is what makes the
// browser render its native credential dialog - unstyleable, unable to explain
// why a login failed, and offering no way to sign out. Browsers now get a real
// form instead, and this test is what stops the header creeping back.
func TestBrowsersAreRedirectedNotChallenged(t *testing.T) {
	c := Config{User: "u", Password: "p"}

	rr := do(t, c, func(r *http.Request) { r.Header.Set("Accept", "text/html") })
	if rr.Header().Get("WWW-Authenticate") != "" {
		t.Error("WWW-Authenticate is back; the browser will show a popup again")
	}
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 to the login form", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, LoginPath) {
		t.Errorf("Location = %q, want a redirect to %s", loc, LoginPath)
	}
	// Where they were going has to survive, or every login dumps you at /.
	if !strings.Contains(loc, url.QueryEscape("/api/recipes")) {
		t.Errorf("Location = %q, does not carry the original path", loc)
	}
}

// A script cannot fill in a form. Redirecting it would turn a failed API call
// into a 200 carrying HTML, which is worse than a clean refusal.
func TestScriptsGetAPlain401(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	rr := do(t, c, func(r *http.Request) { r.Header.Set("Accept", "application/json") })
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") != "" {
		t.Error("a script got a challenge; curl hangs waiting for input on one")
	}
}

// The login page must stay reachable while signed out, or the redirect loops.
func TestLoginPathIsNotGuarded(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	req := httptest.NewRequest(http.MethodGet, LoginPath, nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	c.Middleware(ok(t)).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login page returned %d - the redirect will loop forever", rr.Code)
	}
}

// An unchecked ?next= is an open redirect: a crafted link would bounce an
// operator who has just typed their password onto someone else's page.
func TestRedirectRefusesOffOriginNext(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	for _, evil := range []string{"//evil.example/x", "https://evil.example/x"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", "text/html")
		req.RequestURI = evil
		rr := httptest.NewRecorder()
		c.Middleware(ok(t)).ServeHTTP(rr, req)
		if loc := rr.Header().Get("Location"); strings.Contains(loc, "evil.example") {
			t.Errorf("off-origin next survived into %q", loc)
		}
	}
}

// ---- sessions ------------------------------------------------------------

func TestSessionCookieAuthenticates(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	rr := do(t, c, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: c.MintSession("u")})
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a valid session", rr.Code)
	}
}

func TestForgedSessionIsRejected(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	for _, bad := range []string{"", "garbage", "a.b", c.MintSession("u") + "x"} {
		rr := do(t, c, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: bad})
		})
		if rr.Code == http.StatusOK {
			t.Errorf("forged cookie %q was accepted", bad)
		}
	}
}

// THE PROPERTY THAT MAKES A PASSWORD RESET MEAN SOMETHING: the signing key is
// derived from the credentials, so rotating either one invalidates every
// session that was already issued. Without this a changed password would leave
// existing browsers signed in for the rest of the cookie's life.
func TestRotatingCredentialsInvalidatesExistingSessions(t *testing.T) {
	before := Config{User: "u", Password: "old-password"}
	tok := before.MintSession("u")

	after := Config{User: "u", Password: "new-password"}
	if after.validSession(tok) {
		t.Error("a session minted under the old password still verifies")
	}
	// And the token counts too, since it is equally powerful.
	afterTok := Config{User: "u", Password: "old-password", Token: "new-token"}
	if afterTok.validSession(tok) {
		t.Error("a session survived an API-token rotation")
	}
}

func TestCheckPasswordRequiresBoth(t *testing.T) {
	c := Config{User: "u", Password: "p"}
	if !c.CheckPassword("u", "p") {
		t.Error("the correct pair was rejected")
	}
	for _, bad := range [][2]string{{"u", "wrong"}, {"wrong", "p"}, {"", ""}} {
		if c.CheckPassword(bad[0], bad[1]) {
			t.Errorf("accepted %q/%q", bad[0], bad[1])
		}
	}
}

// Opting out has to be explicit and has to work, or operators will find a
// worse way around it.
func TestDisabledLetsEverythingThrough(t *testing.T) {
	if got := do(t, Config{Disabled: true}, nil).Code; got != http.StatusOK {
		t.Errorf("status = %d, want 200 when disabled", got)
	}
}

// Missing credentials must be an ERROR, not a silent open door. This is the
// difference between an operator who chose to run without auth and one who
// forgot to configure it.
func TestFromEnvRefusesWhenNothingIsConfigured(t *testing.T) {
	t.Setenv("SOUS_AUTH_USER", "")
	t.Setenv("SOUS_AUTH_PASS", "")
	t.Setenv("SOUS_API_TOKEN", "")
	t.Setenv("SOUS_AUTH", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv accepted an empty configuration; it must refuse")
	}
}

func TestFromEnvAllowsExplicitOptOut(t *testing.T) {
	t.Setenv("SOUS_AUTH_USER", "")
	t.Setenv("SOUS_AUTH_PASS", "")
	t.Setenv("SOUS_API_TOKEN", "")
	t.Setenv("SOUS_AUTH", "none")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("explicit opt-out rejected: %v", err)
	}
	if !c.Disabled {
		t.Error("SOUS_AUTH=none did not disable auth")
	}
}

func TestFromEnvRejectsUserWithoutPassword(t *testing.T) {
	t.Setenv("SOUS_AUTH_USER", "u")
	t.Setenv("SOUS_AUTH_PASS", "")
	t.Setenv("SOUS_API_TOKEN", "")
	t.Setenv("SOUS_AUTH", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("a username with an empty password was accepted")
	}
}

func TestFromEnvAcceptsTokenOnly(t *testing.T) {
	t.Setenv("SOUS_AUTH_USER", "")
	t.Setenv("SOUS_AUTH_PASS", "")
	t.Setenv("SOUS_API_TOKEN", "tok")
	t.Setenv("SOUS_AUTH", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatalf("token-only configuration rejected: %v", err)
	}
	if c.Token != "tok" {
		t.Errorf("token = %q", c.Token)
	}
}
