package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/auth"
	"unicode/utf8"
)

func statusOf(t *testing.T, h http.Handler) NodeStatus {
	t.Helper()
	rr := send(t, h, http.MethodGet, "/api/status", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var n NodeStatus
	if err := json.NewDecoder(rr.Body).Decode(&n); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return n
}

func TestStatusOnAnIdleNode(t *testing.T) {
	h := newTestServer(t)
	n := statusOf(t, h)

	if n.PoolGiB <= 0 {
		t.Errorf("pool_gib = %v, want the node's real pool", n.PoolGiB)
	}
	if n.RecipeCount == 0 {
		t.Error("recipe_count = 0 on a seeded catalog")
	}
	if n.DeployedCount != 0 || len(n.Models) != 0 {
		t.Errorf("expected nothing deployed, got %d", n.DeployedCount)
	}
	// An idle node's whole pool minus the reserve is available.
	if want := n.PoolGiB - n.ReserveGiB; n.MarginGiB != want {
		t.Errorf("margin = %v, want %v", n.MarginGiB, want)
	}
	if n.PortLow == 0 || n.PortHigh == 0 {
		t.Error("port range not reported; 'which port' is the question this answers")
	}
}

func TestStatusReportsPortAndRunningState(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d %s", rr.Code, rr.Body.String())
	}
	n := statusOf(t, h)
	if len(n.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(n.Models))
	}
	m := n.Models[0]
	if m.RecipeID != "qwen38" {
		t.Errorf("recipe_id = %q", m.RecipeID)
	}
	if m.Port == 0 {
		t.Error("port = 0; the dashboard exists to answer which port a model is on")
	}
	if !m.Running {
		t.Error("running = false for a container the runtime reports as up")
	}
	if m.Drifted {
		t.Error("drifted = true for a healthy deployment")
	}
	if n.RunningCount != 1 || n.DriftedCount != 0 {
		t.Errorf("counts: running=%d drifted=%d", n.RunningCount, n.DriftedCount)
	}
	if n.CommittedGiB <= 0 {
		t.Error("committed_gib = 0 with a model deployed")
	}
}

// The property the whole endpoint exists for: a deployment RECORD is not a
// running container, and a dashboard that conflates them lies exactly when it
// matters. Simulates the container dying under Sous - OOM reaper, docker rm, a
// crash loop that ran out of restarts.
func TestStatusFlagsDriftWhenTheContainerIsGone(t *testing.T) {
	h, rt := newTestServerWithRuntime(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	// Kill it behind Sous's back, leaving the record intact.
	rt.mu.Lock()
	rt.running = map[string]bool{}
	rt.mu.Unlock()

	n := statusOf(t, h)
	if len(n.Models) != 1 {
		t.Fatalf("models = %d, want 1 - the record should survive", len(n.Models))
	}
	if n.Models[0].Running {
		t.Error("running = true for a container that is gone")
	}
	if !n.Models[0].Drifted {
		t.Error("drifted = false; a record without a container is exactly drift")
	}
	if n.DriftedCount != 1 {
		t.Errorf("drifted_count = %d, want 1", n.DriftedCount)
	}
}

func TestLogsReturnsPlainText(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	rr := send(t, h, http.MethodGet, "/api/logs/qwen38", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if rr.Body.Len() == 0 {
		t.Error("empty log body")
	}
}

func TestLogsRejectsBadTail(t *testing.T) {
	h := newTestServer(t)
	for _, bad := range []string{"0", "-5", "99999", "abc"} {
		rr := send(t, h, http.MethodGet, "/api/logs/qwen38?tail="+bad, "", "")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("tail=%s got %d, want 400", bad, rr.Code)
		}
	}
}

func TestLogsRejectsInvalidID(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/api/logs/..%2Fescape", "", "")
	if rr.Code == http.StatusOK {
		t.Error("a traversal-shaped id returned 200")
	}
}

func TestLastLinesReturnsTheEndNotTheStart(t *testing.T) {
	b := []byte("one\ntwo\nthree\nfour\nfive\n")
	got := string(lastLines(b, 2))
	if got != "four\nfive\n" {
		t.Errorf("lastLines(2) = %q, want the FINAL two lines", got)
	}
	// Asking for more lines than exist returns everything, not an error.
	if all := lastLines(b, 100); !bytes.Equal(all, b) {
		t.Errorf("lastLines(100) = %q, want the whole buffer", all)
	}
	if none := lastLines(nil, 5); len(none) != 0 {
		t.Errorf("lastLines(nil) = %q", none)
	}
}

func TestNodePageRenders(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Node", "pool-bar", "GiB free", "Sous"} {
		if !strings.Contains(body, want) {
			t.Errorf("node page missing %q", want)
		}
	}
	// An idle node must say so rather than render an empty bar with no
	// explanation.
	if !strings.Contains(body, "Nothing deployed") {
		t.Error("idle node page has no empty state")
	}
}

// "GET /" is a catch-all in Go's ServeMux. Without a guard, every mistyped URL
// renders the dashboard with a 200 and a typo becomes indistinguishable from a
// working call.
func TestNodePageDoesNotSwallowUnknownPaths(t *testing.T) {
	h := newTestServer(t)
	for _, p := range []string{"/nope", "/api/nope", "/catalogue"} {
		if rr := send(t, h, http.MethodGet, p, "", ""); rr.Code != http.StatusNotFound {
			t.Errorf("%s got %d, want 404", p, rr.Code)
		}
	}
}

func TestNodePageDrawsSegmentsToScale(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	body := send(t, h, http.MethodGet, "/", "", "").Body.String()
	if !strings.Contains(body, "qwen38") {
		t.Error("deployed model absent from the pool diagram")
	}
	// The segment must carry a computed width, or the bar is decorative.
	if !strings.Contains(body, "style=\"width:") {
		t.Error("no segment widths rendered; the diagram is not drawn to scale")
	}
	// The card must encode the PHASE, not merely that a container exists.
	// "running" was true throughout the eight to ten minutes a model spends
	// loading, which is exactly when it must not be sent traffic - so the
	// assertion moved with the semantics.
	if !strings.Contains(body, "chip is-") {
		t.Error("no phase encoded on the model card")
	}
	if !strings.Contains(body, "is-starting") && !strings.Contains(body, "is-ready") {
		t.Errorf("expected a starting or ready phase on the card")
	}
}

func TestModelPageRendersConfigTelemetryAndLogs(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	rr := send(t, h, http.MethodGet, "/model/qwen38", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Configuration", "Telemetry", "Logs", "Edit",
		"qwen38", "chip is-"} {
		if !strings.Contains(body, want) {
			t.Errorf("model page missing %q", want)
		}
	}
	// The edit box must be pre-filled, or "edit" means "retype from scratch".
	if !strings.Contains(body, "kind: vllm") {
		t.Error("edit textarea not pre-filled with the recipe YAML")
	}
	// Telemetry parsed from the fake runtime's boot log.
	if !strings.Contains(body, "FLASHINFER") {
		t.Error("observed attention backend not shown")
	}
}

func TestModelPageForUndeployedRecipe(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodGet, "/model/kokoro", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not deployed") {
		t.Error("undeployed state not shown")
	}
	if !strings.Contains(body, "no running container") {
		t.Error("missing explanation for absent logs")
	}
}

func TestModelPageUnknownIs404(t *testing.T) {
	h := newTestServer(t)
	if rr := send(t, h, http.MethodGet, "/model/nosuchmodel", "", ""); rr.Code == http.StatusOK {
		t.Errorf("unknown model returned 200")
	}
}

// The form path is what the browser actually uses; the REST verb is for API
// clients. Both must reach the same handler.
func TestFormUpdateAppliesAndRedirects(t *testing.T) {
	h := newTestServer(t)
	y := "id: kokoro\nkind: container\nmodality: tts\nimage: ghcr.io/remsky/kokoro-fastapi-gpu:CHANGED\n"
	rr := post(t, h, "/model/kokoro/update", "application/x-www-form-urlencoded",
		"body="+url.QueryEscape(y))
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusFound && rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	after := send(t, h, http.MethodGet, "/model/kokoro", "", "").Body.String()
	if !strings.Contains(after, "CHANGED") {
		t.Error("form update did not persist")
	}
}

// Docker frames non-TTY log output with an 8-byte binary header per chunk.
// Passing that through put raw bytes into the HTML: the page became invalid
// UTF-8 and the logs panel rendered nothing, with no error to explain it.
func TestLogOutputIsValidUTF8(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	for _, path := range []string{"/api/logs/qwen38", "/model/qwen38"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.Bytes()
		if !utf8.Valid(body) {
			t.Errorf("%s emitted invalid UTF-8", path)
		}
		if bytes.ContainsRune(body, 0) {
			t.Errorf("%s emitted a NUL byte", path)
		}
	}
}

func TestSafeTextRepairsBinary(t *testing.T) {
	got := safeText([]byte{0x01, 0x00, 0x00, 0x00, 0x89, 'h', 'i'})
	if !utf8.ValidString(got) {
		t.Errorf("safeText returned invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, 0) {
		t.Errorf("safeText kept a NUL: %q", got)
	}
	if !strings.Contains(got, "hi") {
		t.Errorf("safeText dropped readable text: %q", got)
	}
}

// ---- login flow ----------------------------------------------------------

func guardedServer(t *testing.T) http.Handler {
	t.Helper()
	return buildServerAuth(t, auth.Config{User: "op", Password: "s3cret"})
}

func TestLoginPageRenders(t *testing.T) {
	h := guardedServer(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Sign in", `name="username"`, `name="password"`, "Bearer"} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	// It must not leak what it is guarding.
	if strings.Contains(body, "GiB pool") {
		t.Error("login page discloses node capacity to an unauthenticated visitor")
	}
}

func TestLoginSetsSessionAndRedirects(t *testing.T) {
	h := guardedServer(t)
	form := url.Values{"username": {"op"}, "password": {"s3cret"}, "next": {"/catalog"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/catalog" {
		t.Errorf("Location = %q, want /catalog", got)
	}
	var ck *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.CookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no session cookie issued")
	}
	if !ck.HttpOnly {
		t.Error("session cookie is not HttpOnly; script-readable session cookies are stealable")
	}
	if ck.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie is not SameSite=Lax")
	}

	// And it must actually authenticate a subsequent request.
	req2 := httptest.NewRequest(http.MethodGet, "/api/recipes", nil)
	req2.AddCookie(ck)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("session did not authenticate: %d", rr2.Code)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	h := guardedServer(t)
	form := url.Values{"username": {"op"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Wrong username or password") {
		t.Error("no error shown on the returned form")
	}
	// Must not say WHICH was wrong - that confirms whether a username exists.
	if strings.Contains(strings.ToLower(body), "no such user") ||
		strings.Contains(strings.ToLower(body), "unknown username") {
		t.Error("the error distinguishes a bad username from a bad password")
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.CookieName && c.Value != "" {
			t.Error("a session cookie was issued for a failed login")
		}
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	h := guardedServer(t)
	c := auth.Config{User: "op", Password: "s3cret"}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: c.MintSession("op")})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	cleared := false
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == auth.CookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not expire the session cookie")
	}
}

// An open redirect would bounce an operator who has just typed their password
// onto someone else's page.
func TestLoginRefusesOffOriginNext(t *testing.T) {
	h := guardedServer(t)
	for _, evil := range []string{"https://evil.example/x", "//evil.example/x"} {
		form := url.Values{"username": {"op"}, "password": {"s3cret"}, "next": {evil}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if loc := rr.Header().Get("Location"); loc != "/" {
			t.Errorf("next=%q redirected to %q, want /", evil, loc)
		}
	}
}

// The gateway has to be reachable through the real server, behind the real
// auth middleware. Unit tests on the gateway package prove the routing; this
// proves it is actually wired in and guarded.
func TestGatewayIsMountedAndGuarded(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}
	rr := send(t, h, http.MethodGet, "/v1/models", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"object":"list"`) {
		t.Errorf("not an OpenAI list envelope: %s", body)
	}
	// The recipe's alias, not its id, is what a client should see offered.
	if !strings.Contains(body, "qwen38") {
		t.Errorf("deployed model absent from /v1/models: %s", body)
	}
	if !strings.Contains(body, `"phase"`) {
		t.Errorf("no phase on the model listing; a client cannot tell if it is usable: %s", body)
	}
}

// An unauthenticated caller must not reach the inference surface. The gateway
// proxies to a GPU; leaving it outside the guard would make authentication on
// everything else pointless.
func TestGatewayRequiresAuth(t *testing.T) {
	h := buildServerAuth(t, auth.Config{User: "u", Password: "p"})
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", path, rr.Code)
		}
	}
}
