package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/auth"
)

// browserPost mimics a form submitted from the dashboard: the Accept header is
// what selects the HTML path, and without it the handler answers JSON.
func browserPost(t *testing.T, h http.Handler, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func browserGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// THE ONE-SHOT PROPERTY. The secret is shown at creation and is not
// recoverable afterwards - not from a reload, not from the store, not by
// whoever runs the server. A page that could show it again would make the
// hashing pointless.
// A minted secret is the prefix plus 43 base64url characters. The usage example
// on the page is the prefix plus an ellipsis, and the two must not be confused.
var realSecret = regexp.MustCompile(`sk-sous-[A-Za-z0-9_-]{20,}`)

func TestKeysPageShowsTheSecretExactlyOnce(t *testing.T) {
	h := newTestServer(t)

	rr := browserPost(t, h, "/keys", "name=voice+demo")
	if rr.Code != http.StatusOK {
		t.Fatalf("create returned %d: %.200s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sk-sous-") {
		t.Fatalf("the fresh secret was not shown: %.300s", body)
	}
	if !strings.Contains(body, "not shown again") {
		t.Error("the page does not warn the key cannot be recovered")
	}

	again := browserGet(t, h, "/keys").Body.String()
	// A REAL secret, not the placeholder. The page shows "sk-sous-…" as an
	// example in the usage snippet, so a bare prefix match reports the example
	// as a leak - and worse, it passed for years only because the page was
	// truncating before it reached that snippet.
	if realSecret.MatchString(again) {
		t.Error("the secret came back on a later page load")
	}
	if !strings.Contains(again, "voice demo") {
		t.Error("the issued key is not listed by name")
	}
}

func TestKeysAPICreateAndRevoke(t *testing.T) {
	h := newTestServer(t)

	rr := post(t, h, "/api/keys", "application/json", `{"name":"ci"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body.String())
	}
	var made struct {
		Key    map[string]any `json:"key"`
		Secret string         `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(made.Secret, "sk-sous-") {
		t.Fatalf("secret = %q", made.Secret)
	}
	id, _ := made.Key["id"].(string)
	if id == "" {
		t.Fatal("no id returned; the key cannot be revoked")
	}

	// The listing must never carry the secret or its hash.
	list := send(t, h, http.MethodGet, "/api/keys", "", "").Body.String()
	if strings.Contains(list, made.Secret) {
		t.Error("the listing contains the plaintext secret")
	}
	if strings.Contains(list, "hash") {
		t.Error("the listing exposes the stored hash")
	}

	if rr := send(t, h, http.MethodDelete, "/api/keys/"+id, "", ""); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", rr.Code, rr.Body.String())
	}
	after := send(t, h, http.MethodGet, "/api/keys", "", "").Body.String()
	if !strings.Contains(after, `"disabled":true`) {
		t.Errorf("the key was not marked disabled: %s", after)
	}
}

// A key with no name must be refused: an unattributable credential is one
// nobody dares revoke.
func TestCreatingAnUnnamedKeyIsRefused(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/api/keys", "application/json", `{"name":""}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unnamed key = %d, want 400", rr.Code)
	}
}

// ---- permissions ----------------------------------------------------------

// tokenServer is a server with auth ON and an admin token, because these tests
// are about what a credential may reach - with auth off everything is
// reachable and they would prove nothing.
func tokenServer(t *testing.T) http.Handler {
	t.Helper()
	return buildServerAuth(t, auth.Config{Token: "admin-tok"})
}

func as(t *testing.T, h http.Handler, bearer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// issue creates a key with the admin token and returns its id, secret and the
// permission the API reported for it.
func issue(t *testing.T, h http.Handler, body string) (id, secret, permission string) {
	t.Helper()
	rr := as(t, h, "admin-tok", http.MethodPost, "/api/keys", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %s = %d: %s", body, rr.Code, rr.Body.String())
	}
	var made struct {
		Key struct {
			ID         string `json:"id"`
			Permission string `json:"permission"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	return made.Key.ID, made.Secret, made.Key.Permission
}

// THE FEATURE, END TO END: a key issued with the admin permission drives the
// admin API through the real middleware, and one issued without it still
// cannot.
func TestAnAdminKeyIssuedOverTheAPIReachesTheAdminAPI(t *testing.T) {
	h := tokenServer(t)

	_, admin, perm := issue(t, h, `{"name":"fleet automation","permission":"admin"}`)
	if perm != "admin" {
		t.Fatalf("the API reported permission %q for an admin key", perm)
	}
	for _, p := range []string{"/api/keys", "/api/status", "/v1/models"} {
		if rr := as(t, h, admin, http.MethodGet, p, ""); rr.Code != http.StatusOK {
			t.Errorf("GET %s with the admin key = %d, want 200: %.120s", p, rr.Code, rr.Body.String())
		}
	}
	// It can issue ordinary keys, as the token can.
	if rr := as(t, h, admin, http.MethodPost, "/api/keys", `{"name":"made by a key"}`); rr.Code != http.StatusCreated {
		t.Errorf("POST /api/keys with the admin key = %d, want 201", rr.Code)
	}

	_, plain, perm := issue(t, h, `{"name":"notebook"}`)
	if perm != "inference" {
		t.Fatalf("the API reported permission %q for an ordinary key, want inference", perm)
	}
	for _, p := range []string{"/api/keys", "/api/status"} {
		if rr := as(t, h, plain, http.MethodGet, p, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("SCOPE HOLE: GET %s with an inference key = %d, want 401", p, rr.Code)
		}
	}
	if rr := as(t, h, plain, http.MethodPost, "/api/keys", `{"name":"escalation","permission":"admin"}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("SCOPE HOLE: an inference key minted a key: %d", rr.Code)
	}
	if rr := as(t, h, plain, http.MethodGet, "/v1/models", ""); rr.Code != http.StatusOK {
		t.Errorf("GET /v1/models with the inference key = %d, want 200", rr.Code)
	}
}

// Saying "inference" out loud is the same as saying nothing.
func TestAskingForInferenceByNameIsTheDefault(t *testing.T) {
	h := tokenServer(t)
	_, secret, perm := issue(t, h, `{"name":"notebook","permission":"inference"}`)
	if perm != "inference" {
		t.Fatalf("permission = %q", perm)
	}
	if rr := as(t, h, secret, http.MethodGet, "/api/keys", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("SCOPE HOLE: an explicit inference key reached /api/keys: %d", rr.Code)
	}
}

// Revoking is the point of an admin KEY over the shared token.
func TestRevokingAnAdminKeyTakesTheAdminAPIAway(t *testing.T) {
	h := tokenServer(t)
	id, secret, _ := issue(t, h, `{"name":"fleet automation","permission":"admin"}`)
	if rr := as(t, h, secret, http.MethodGet, "/api/keys", ""); rr.Code != http.StatusOK {
		t.Fatalf("the admin key did not work before it was revoked: %d", rr.Code)
	}
	if rr := as(t, h, "admin-tok", http.MethodDelete, "/api/keys/"+id, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", rr.Code, rr.Body.String())
	}
	for _, p := range []string{"/api/keys", "/v1/models"} {
		if rr := as(t, h, secret, http.MethodGet, p, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a revoked admin key = %d, want 401", p, rr.Code)
		}
	}
}

// An allowlist on a key that can deploy any model would promise a limit that
// nothing enforces. Refuse it rather than store a restriction that is not one.
func TestAnAdminKeyWithAModelListIsRefused(t *testing.T) {
	h := tokenServer(t)
	rr := as(t, h, "admin-tok", http.MethodPost, "/api/keys", `{"name":"confused","permission":"admin","models":["asr"]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("admin + models = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if list := as(t, h, "admin-tok", http.MethodGet, "/api/keys", "").Body.String(); strings.Contains(list, "confused") {
		t.Errorf("a refused key was stored anyway: %s", list)
	}
}

func TestAnUnknownPermissionIsRefused(t *testing.T) {
	h := tokenServer(t)
	rr := as(t, h, "admin-tok", http.MethodPost, "/api/keys", `{"name":"typo","permission":"root"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("permission=root = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if list := as(t, h, "admin-tok", http.MethodGet, "/api/keys", "").Body.String(); strings.Contains(list, "typo") {
		t.Errorf("a key was issued for a permission that does not exist: %s", list)
	}
}

// The page has to offer the choice, default it to the safe one, and say plainly
// which keys in the list are the dangerous kind.
func TestKeysPageOffersThePermissionAndMarksAdminKeys(t *testing.T) {
	h := newTestServer(t)

	form := browserGet(t, h, "/keys").Body.String()
	if !strings.Contains(form, `name="permission"`) {
		t.Fatal("the new-key form has no permission field")
	}
	// Inference must be the option a hurried operator gets.
	inf, adm := strings.Index(form, `value="inference"`), strings.Index(form, `value="admin"`)
	if inf < 0 || adm < 0 || inf > adm {
		t.Errorf("the permission field does not list inference first: inference@%d admin@%d", inf, adm)
	}
	if strings.Contains(form, `value="admin" selected`) || strings.Contains(form, `value="admin" checked`) {
		t.Error("admin is preselected on the new-key form")
	}

	rr := browserPost(t, h, "/keys", "name=fleet+automation&permission=admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("create returned %d: %.200s", rr.Code, rr.Body.String())
	}
	fresh := rr.Body.String()
	if !realSecret.MatchString(fresh) {
		t.Fatal("the fresh admin secret was not shown")
	}
	// The one moment the operator is holding the secret is the moment to say
	// what it can do.
	if !strings.Contains(fresh, "can deploy") {
		t.Error("the page does not warn that the fresh key is an admin one")
	}

	browserPost(t, h, "/keys", "name=notebook")
	list := browserGet(t, h, "/keys").Body.String()
	if strings.Count(list, "chip is-admin") != 1 {
		t.Errorf("want exactly one key marked admin in the list, found %d", strings.Count(list, "chip is-admin"))
	}
}

// From the form, an admin key with a model list is refused with a message the
// operator can read, not a silent drop of half of what they asked for.
func TestKeysPageRefusesAnAdminKeyWithModels(t *testing.T) {
	h := newTestServer(t)
	rr := browserPost(t, h, "/keys", "name=confused&permission=admin&models=asr")
	if rr.Code == http.StatusOK && realSecret.MatchString(rr.Body.String()) {
		t.Fatal("an admin key with a model list was issued from the form")
	}
	if list := browserGet(t, h, "/keys").Body.String(); strings.Contains(list, "confused") {
		t.Error("a refused key appears in the list")
	}
}

// AN ADMIN KEY CANNOT ISSUE ANOTHER ADMIN KEY. Otherwise revoking a leaked one
// revokes nothing: whoever held it made a second, under a plausible name, and
// the list cannot tell it from a real one. Admin keys come from the operator -
// the password, a session, or the token - and from nothing else.
func TestAnAdminKeyCannotIssueAnotherAdminKey(t *testing.T) {
	h := tokenServer(t)
	_, admin, _ := issue(t, h, `{"name":"fleet automation","permission":"admin"}`)

	rr := as(t, h, admin, http.MethodPost, "/api/keys", `{"name":"successor","permission":"admin"}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("an admin key minting an admin key = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if list := as(t, h, "admin-tok", http.MethodGet, "/api/keys", "").Body.String(); strings.Contains(list, "successor") {
		t.Errorf("the refused key was stored anyway: %s", list)
	}
	// The token still can, which is the point of keeping it.
	if _, _, perm := issue(t, h, `{"name":"second automation","permission":"admin"}`); perm != "admin" {
		t.Errorf("the admin token could not issue an admin key: permission %q", perm)
	}
}

// The permission comes from the BODY of the request. A query string is where
// a crafted link or a form's action would put it, and "admin" must not be
// something a URL can ask for on an operator's behalf.
func TestThePermissionCannotBeSmuggledInTheQueryString(t *testing.T) {
	h := newTestServer(t)
	rr := browserPost(t, h, "/keys?permission=admin", "name=from+a+link")
	if rr.Code != http.StatusOK {
		t.Fatalf("create returned %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "This is an admin key") {
		t.Fatal("a query-string permission produced an admin key")
	}
	if list := browserGet(t, h, "/keys").Body.String(); strings.Contains(list, "chip is-admin") {
		t.Error("a query-string permission produced an admin key in the list")
	}
}

// A key's name is typed by whoever made it and ends up in a log line. A name
// with a line break in it must not be able to write a second, false entry.
func TestAKeyNameCannotForgeALogLine(t *testing.T) {
	h := tokenServer(t)
	_, admin, _ := issue(t, h, `{"name":"ci\napikey: admin key \"x\" (abcd) issued by the operator","permission":"admin"}`)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	// Anything this key does on the keys API is logged under its name: here, a
	// refused attempt to mint an admin key, and an ordinary key it may issue.
	as(t, h, admin, http.MethodPost, "/api/keys", `{"name":"successor","permission":"admin"}`)
	as(t, h, admin, http.MethodPost, "/api/keys", `{"name":"notebook"}`)

	out := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("two events should be two log lines, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "REFUSED") {
		t.Errorf("the refused admin-key request was not logged as one: %s", lines[0])
	}
	for _, l := range lines {
		if !strings.Contains(l, "apikey: ") {
			t.Errorf("a log line that is not ours: %q", l)
		}
	}
}
