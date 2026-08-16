package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, h http.Handler, path, ct, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func send(t *testing.T, h http.Handler, method, path, ct, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// ids parses the catalog listing and returns just the recipe ids.
//
// A substring check on the raw JSON is not good enough: "qwen38" also appears
// inside qwen38-fp8's served_as list and in several recipes' notes, so a naive
// Contains reports a deleted recipe as still present.
func ids(t *testing.T, h http.Handler) []string {
	t.Helper()
	rr := send(t, h, http.MethodGet, "/api/recipes", "", "")
	var rs []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&rs); err != nil {
		t.Fatalf("decoding catalog: %v", err)
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func hasID(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

const newRecipeJSON = `{"id":"testmodel","kind":"vllm","modality":"text",
  "model":"org/Test","image":"vllm/vllm-openai:x",
  "declared":{"weights_gib":10,"kv_gib":4}}`

func TestCreateRecipe(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/api/recipes", "application/json", newRecipeJSON)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}
	if !hasID(ids(t, h), "testmodel") {
		t.Error("created recipe is not in the catalog listing")
	}
}

// Create must never overwrite. Otherwise importing an older copy of a recipe
// silently destroys a tuned one.
func TestCreateRefusesToOverwrite(t *testing.T) {
	h := newTestServer(t)
	dup := `{"id":"qwen38","kind":"vllm","modality":"text","image":"x"}`
	rr := post(t, h, "/api/recipes", "application/json", dup)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "PUT /api/recipes/qwen38") {
		t.Error("409 does not tell the caller how to update instead")
	}
}

// IMPORT means pasting a published recipe file, and every recipe this project
// publishes is YAML.
func TestCreateAcceptsYAMLImport(t *testing.T) {
	h := newTestServer(t)
	y := "id: yamlmodel\nkind: vllm\nmodality: text\nimage: vllm/vllm-openai:x\n" +
		"declared:\n  weights_gib: 5\n  kv_gib: 2\n"
	rr := post(t, h, "/api/recipes", "application/yaml", y)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateRejectsInvalidID(t *testing.T) {
	h := newTestServer(t)
	for _, bad := range []string{`{"id":"../escape","kind":"vllm","modality":"text","image":"x"}`,
		`{"id":"","kind":"vllm","modality":"text","image":"x"}`} {
		if rr := post(t, h, "/api/recipes", "application/json", bad); rr.Code != http.StatusBadRequest {
			t.Errorf("body %s got %d, want 400", bad, rr.Code)
		}
	}
}

func TestCreateRejectsInvalidRecipe(t *testing.T) {
	h := newTestServer(t)
	// No image, which Validate requires.
	rr := post(t, h, "/api/recipes", "application/json",
		`{"id":"noimage","kind":"vllm","modality":"text"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

// The response has to answer the question an operator actually has after
// saving: does the running container now disagree with its recipe?
func TestUpdateReturnsDiffAndRestartVerdict(t *testing.T) {
	h := newTestServer(t)
	body := `{"id":"qwen38","kind":"vllm","modality":"text","image":"vllm/vllm-openai:CHANGED",
	  "declared":{"weights_gib":24.87,"kv_gib":45.67}}`
	rr := send(t, h, http.MethodPut, "/api/recipes/qwen38", "application/json", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Changes       []map[string]any `json:"changes"`
		NeedsRestart  bool             `json:"needs_restart"`
		RestartFields []string         `json:"restart_fields"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) == 0 {
		t.Fatal("no changes reported for a changed image")
	}
	if !res.NeedsRestart {
		t.Error("an image change was not flagged as needing a restart")
	}
	found := false
	for _, f := range res.RestartFields {
		if f == "image" {
			found = true
		}
	}
	if !found {
		t.Errorf("restart_fields = %v, want to contain image", res.RestartFields)
	}
}

// A body naming a different id would write one recipe while the caller
// believed it had edited another.
func TestUpdateRejectsMismatchedID(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodPut, "/api/recipes/qwen38", "application/json",
		`{"id":"kokoro","kind":"vllm","modality":"text","image":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateUnknownRecipeIs404(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodPut, "/api/recipes/nosuchmodel", "application/json", newRecipeJSON)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// Preview must not persist. A diff view that saved would make "look before you
// leap" the thing that leaps.
func TestDiffDoesNotSave(t *testing.T) {
	h := newTestServer(t)
	before := send(t, h, http.MethodGet, "/api/recipes", "", "").Body.String()

	rr := post(t, h, "/api/recipes/qwen38/diff", "application/json",
		`{"id":"qwen38","kind":"vllm","modality":"text","image":"vllm/vllm-openai:PREVIEW"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "PREVIEW") {
		t.Error("diff does not mention the proposed value")
	}
	if after := send(t, h, http.MethodGet, "/api/recipes", "", "").Body.String(); after != before {
		t.Error("diff preview modified the catalog")
	}
}

func TestDeleteRecipe(t *testing.T) {
	h := newTestServer(t)
	if rr := send(t, h, http.MethodDelete, "/api/recipes/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if hasID(ids(t, h), "qwen38") {
		t.Error("deleted recipe still listed")
	}
}

func TestDeleteUnknownRecipeIs404(t *testing.T) {
	h := newTestServer(t)
	if rr := send(t, h, http.MethodDelete, "/api/recipes/nosuchmodel", "", ""); rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// The guard the operator chose: a deployed recipe is not deleted by accident.
func TestDeleteRefusesWhileDeployed(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("setup deploy failed: %d %s", rr.Code, rr.Body.String())
	}
	rr := send(t, h, http.MethodDelete, "/api/recipes/qwen38", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "force=true") {
		t.Error("409 does not name the way out")
	}
	// And it must still be there.
	if !hasID(ids(t, h), "qwen38") {
		t.Error("recipe was deleted despite the refusal")
	}
}

// force does both halves deliberately.
func TestDeleteForceStopsThenDeletes(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("setup deploy failed: %d", rr.Code)
	}
	rr := send(t, h, http.MethodDelete, "/api/recipes/qwen38?force=true", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"undeployed":true`) {
		t.Errorf("response does not report the undeploy: %s", rr.Body.String())
	}
	if got := send(t, h, http.MethodGet, "/api/deployments", "", "").Body.String(); strings.Contains(got, "qwen38") {
		t.Error("container still deployed after force delete")
	}
}

// Recipes are a few KiB. A huge body is a mistake or an attempt to exhaust
// memory, and neither should be read into it.
func TestCreateRejectsOversizedBody(t *testing.T) {
	h := newTestServer(t)
	huge := `{"id":"big","kind":"vllm","modality":"text","image":"x","notes":"` +
		strings.Repeat("A", maxRecipeBody+100) + `"}`
	if rr := post(t, h, "/api/recipes", "application/json", huge); rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}
