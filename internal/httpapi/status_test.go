package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
	if !strings.Contains(body, "chip is-running") {
		t.Error("running state not encoded on the model card")
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
		"qwen38", "chip is-running"} {
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
