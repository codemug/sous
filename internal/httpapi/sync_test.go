package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The route has to exist and answer POST. A sync endpoint that 404s is
// indistinguishable from one that silently did nothing.
func TestSyncRecipesEndpointAnswers(t *testing.T) {
	h := newTestServer(t)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recipes/sync", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

// v0.1.1 shipped /api/recipes serving Go field names while every other endpoint
// served snake_case, and it was only caught by using the thing. Assert both
// halves: the keys that should be there, and the Go names that must not be.
func TestSyncRecipesSpeaksSnakeCase(t *testing.T) {
	h := newTestServer(t)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recipes/sync", nil))

	body := rr.Body.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}

	for _, want := range []string{"added", "updated", "current", "kept"} {
		if _, ok := parsed[want]; !ok {
			t.Errorf("missing key %q in %s", want, body)
		}
	}
	for _, bad := range []string{"Added", "Updated", "Current", "Kept"} {
		if strings.Contains(body, `"`+bad+`"`) {
			t.Errorf("Go field name %q leaked into the response: %s", bad, body)
		}
	}
}

// A GET must not silently do nothing that looks like success - a sync that
// mutates the catalog should not be reachable by a link or a prefetch.
func TestSyncRecipesRejectsGET(t *testing.T) {
	h := newTestServer(t)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/recipes/sync", nil))

	if rr.Code == http.StatusOK {
		t.Errorf("GET /api/recipes/sync returned 200; it should not be a safe-method route")
	}
}

// The endpoint exists so a node with no shell access can still take a corrected
// recipe. On a server whose catalog was seeded at startup there is nothing to
// change, and the honest answer is "everything already current" - not a claim
// of work done.
func TestSyncRecipesOnASeededServerChangesNothing(t *testing.T) {
	h := newTestServer(t)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/recipes/sync", nil))

	var res struct {
		Added   []string `json:"added"`
		Updated []string `json:"updated"`
		Kept    []string `json:"kept"`
		Current []string `json:"current"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Updated) != 0 {
		t.Errorf("added=%v updated=%v, want both empty on an already-seeded server",
			res.Added, res.Updated)
	}
	if len(res.Current) == 0 {
		t.Errorf("current is empty; the seeded catalog should have been recognised")
	}
}
