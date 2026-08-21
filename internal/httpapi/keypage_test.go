package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if strings.Contains(again, "sk-sous-") {
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
