package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestFetchAPIStartsAndRefusesJunk(t *testing.T) {
	h := newTestServer(t)

	rr := post(t, h, "/api/fetch", "application/json", `{"repo":"Qwen/Qwen3.6-35B-A3B-FP8"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "downloading") {
		t.Errorf("body does not report the phase: %s", rr.Body.String())
	}

	// It must appear in the listing while it runs.
	list := send(t, h, http.MethodGet, "/api/fetch", "", "").Body.String()
	if !strings.Contains(list, "Qwen3.6") {
		t.Errorf("started fetch absent from the listing: %s", list)
	}

	// And a value that is not a repo id must never reach a container.
	for _, bad := range []string{`{"repo":"nonsense"}`, `{"repo":"owner/name; rm -rf /"}`, `{"repo":""}`} {
		if rr := post(t, h, "/api/fetch", "application/json", bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%s accepted with %d, want 400", bad, rr.Code)
		}
	}
}

// "model" is accepted as well as "repo": it is the noun the rest of this API
// uses, and a caller should not have to remember which one an endpoint chose.
func TestFetchAcceptsModelAsAnAlias(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/api/fetch", "application/json", `{"model":"Qwen/Qwen3.6-35B-A3B-FP8"}`)
	if rr.Code != http.StatusAccepted {
		t.Errorf("model alias = %d, want 202: %s", rr.Code, rr.Body.String())
	}
}
