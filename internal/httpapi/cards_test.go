package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestNodeUsesTheCardGrid(t *testing.T) {
	h := newTestServer(t)
	post(t, h, "/api/deploy/qwen38", "", "")
	b := send(t, h, http.MethodGet, "/", "", "").Body.String()
	for _, want := range []string{`class="cards"`, `class="card is-`, "card-head", "card-foot", "mlabel"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(b, "</html>") {
		t.Error("page did not render to completion")
	}
}
