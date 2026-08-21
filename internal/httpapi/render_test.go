package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The two pages that changed shape most. Rendering them is worth a test of its
// own: a template that fails part way through leaves a half-written page with a
// 200 status, which reads as success everywhere except the browser.
func TestStage1PagesRenderWhole(t *testing.T) {
	h := newTestServer(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy: %d", rr.Code)
	}

	for _, path := range []string{"/", "/models"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		// A truncated render is the failure mode here, so check the page got
		// all the way to its closing markup.
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s did not render to completion: %.160s", path, tailOf(body, 160))
		}
		if strings.Contains(body, "executing") && strings.Contains(body, "template:") {
			t.Errorf("%s contains a template error", path)
		}
	}

	node := send(t, h, http.MethodGet, "/", "", "").Body.String()
	// The bar and the card must agree, which is the whole point of stage 1.
	if !strings.Contains(node, "seg-starting") && !strings.Contains(node, "seg-ready") {
		t.Error("no phase-coloured segment in the pool bar")
	}
	if !strings.Contains(node, "pool-ruler") {
		t.Error("no ruler under the bar")
	}
	// The old binary flag must be gone from the markup entirely.
	if strings.Contains(node, "seg-drift") || strings.Contains(node, "seg-run") {
		t.Error("the old Drifted-based segment classes are still rendered")
	}
}

// Named to avoid shadowing the builtin max, which status.go uses on floats.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
