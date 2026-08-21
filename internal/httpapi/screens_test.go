package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// A template that fails part way through leaves a half-written page under a
// 200, which reads as success everywhere except the browser. Every screen gets
// checked for its closing markup.
func TestEveryScreenRendersWhole(t *testing.T) {
	h := newTestServer(t)
	post(t, h, "/api/deploy/qwen38", "", "")
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/", "/models", "/larder", "/keys", "/sources", "/model/qwen38", "/model/qwen36/plan"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s truncated: …%s", path, tailOf(body, 120))
		}
		if strings.Contains(body, "template:") && strings.Contains(body, "executing") {
			t.Errorf("%s carries a template error", path)
		}
	}
}

// The card grid is the layout the design is built on; a screen still on a table
// has not been ported.
func TestListScreensUseCards(t *testing.T) {
	h := newTestServer(t)
	post(t, h, "/api/deploy/qwen38", "", "")
	post(t, h, "/api/keys", "application/json", `{"name":"probe"}`)

	for _, path := range []string{"/", "/models", "/keys"} {
		body := send(t, h, http.MethodGet, path, "", "").Body.String()
		if !strings.Contains(body, `class="cards`) {
			t.Errorf("%s is not on the card grid", path)
		}
	}
}
