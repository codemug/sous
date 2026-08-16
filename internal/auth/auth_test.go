package auth

import (
	"net/http"
	"net/http/httptest"
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

// A browser needs the challenge to show its prompt; a script does not, and
// curl hangs waiting for input when it receives one.
func TestChallengesBrowsersOnly(t *testing.T) {
	c := Config{User: "u", Password: "p"}

	html := do(t, c, func(r *http.Request) { r.Header.Set("Accept", "text/html") })
	if html.Header().Get("WWW-Authenticate") == "" {
		t.Error("browser request got no WWW-Authenticate challenge")
	}
	api := do(t, c, func(r *http.Request) { r.Header.Set("Accept", "application/json") })
	if api.Header().Get("WWW-Authenticate") != "" {
		t.Error("API request got a WWW-Authenticate challenge; curl will hang on it")
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
