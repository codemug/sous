package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
