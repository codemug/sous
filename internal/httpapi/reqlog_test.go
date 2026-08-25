package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/reqlog"
)

// END TO END: a real chat-completions request, through the real mux, lands on
// disk with the sender identity and the exact payload. Deploying against the
// fake runtime means the actual proxy hop fails (nothing is listening on the
// fake port) - which is fine, since logging happens on RECEIPT, before that
// hop, and this test is about the audit trail rather than the proxy.
func TestChatCompletionRequestIsLoggedToDisk(t *testing.T) {
	h, dir := newTestServerWithReqLogDir(t)
	if rr := post(t, h, "/api/deploy/qwen38", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("deploy failed: %d", rr.Code)
	}

	body := `{"model":"qwen38","messages":[{"role":"user","content":"what is a mutex"}]}`
	post(t, h, "/v1/chat/completions", "application/json", body)

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("log files = %d, want 1", len(files))
	}
	raw, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var e reqlog.Entry
	if err := json.Unmarshal(raw[:len(raw)-1], &e); err != nil { // trailing \n
		t.Fatalf("not one JSON line: %s: %v", raw, err)
	}
	if e.Model != "qwen38" {
		t.Errorf("Model = %q", e.Model)
	}
	if string(e.Body) != body {
		t.Errorf("Body = %s, want %s", e.Body, body)
	}
	if e.Sender == "" {
		t.Error("no sender recorded")
	}
}

func TestAdminPageShowsRetentionAndFiles(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/admin", "", "").Body.String()
	if !strings.Contains(body, "Request log") {
		t.Fatal("no request-log panel on the admin page")
	}
	if !strings.Contains(body, `name="days"`) {
		t.Error("no retention input on the admin page")
	}
	// The default, so the page shows something sane before anyone touches it.
	if !strings.Contains(body, `value="30"`) {
		t.Errorf("does not show the default retention: %s", body)
	}
}

func TestSetRetentionPersists(t *testing.T) {
	h := newTestServer(t)
	rr := post(t, h, "/admin/reqlog-retention", "application/x-www-form-urlencoded", "days=7")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := send(t, h, http.MethodGet, "/admin", "", "").Body.String()
	if !strings.Contains(body, `value="7"`) {
		t.Errorf("retention did not persist: %s", body)
	}
}

// wantsHTML gates every browser-facing error onto a 303 with err=1 in the
// query string rather than a 4xx status - the same pattern every other
// destructive form in this codebase uses, so the redirect target and its
// query string are what a browser-path test has to check, not the status
// code alone.
func TestSetRetentionRejectsGarbage(t *testing.T) {
	h := newTestServer(t)
	for _, bad := range []string{"days=abc", "days=-1", "days="} {
		rr := post(t, h, "/admin/reqlog-retention", "application/x-www-form-urlencoded", bad)
		loc := rr.Header().Get("Location")
		if !strings.Contains(loc, "err=1") {
			t.Errorf("%q was accepted: status %d, Location %q", bad, rr.Code, loc)
		}
	}
	// The bad values must not have overwritten the default.
	body := send(t, h, http.MethodGet, "/admin", "", "").Body.String()
	if !strings.Contains(body, `value="30"`) {
		t.Errorf("a rejected value changed the stored retention: %s", body)
	}
}

// The JSON/API path answers with a real status code, not a redirect - a
// script checking rr.Code should not have to know about err=1.
func TestSetRetentionRejectsGarbageOverTheAPI(t *testing.T) {
	h := newTestServer(t)
	rr := send(t, h, http.MethodPost, "/admin/reqlog-retention", "application/json", "")
	if rr.Code < 400 {
		t.Errorf("empty body over the API path was accepted: %d", rr.Code)
	}
}

func TestGetReqLogReportsState(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/api/reqlog", "", "").Body.String()
	if !strings.Contains(body, `"RetentionDays":30`) {
		t.Errorf("body = %s", body)
	}
}
