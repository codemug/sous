package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

func getNodesJSON(t *testing.T, h http.Handler) []nodeJSON {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/nodes = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var out []nodeJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return out
}

func TestAPINodesComputesMarginLikeThePlanner(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{
			{RecipeId: "qwen38", HostPort: 8001, Phase: "running", WeightsGib: 24.87, KvGib: 45.67},
		},
		CachedWeightRepos: []string{"Inferact/Qwen3.8-27B-NVFP4"},
	})

	out := getNodesJSON(t, h)
	if len(out) != 1 {
		t.Fatalf("want 1 node, got %d", len(out))
	}
	n := out[0]
	if n.NodeID != "asus-gx10" || !n.Connected {
		t.Fatalf("node id/connected wrong: %+v", n)
	}
	// committed = 24.87 + 45.67 = 70.54; margin = 121.6 - 24 - 70.54 = 27.06.
	// This is the EXACT arithmetic planOnNode/capacity.Planner use; if this
	// drifts, the board would show a fit the deploy then refuses.
	if got := round2(n.CommittedGiB); got != 70.54 {
		t.Fatalf("committed = %v, want 70.54", got)
	}
	if got := round2(n.MarginGiB); got != 27.06 {
		t.Fatalf("margin = %v, want 27.06", got)
	}
	if len(n.Deployments) != 1 || n.Deployments[0].RecipeID != "qwen38" || n.Deployments[0].HostPort != 8001 {
		t.Fatalf("deployment wrong: %+v", n.Deployments)
	}
	// docker_status carries the raw word; it must NOT be dressed up as a
	// readiness verdict.
	if n.Deployments[0].DockerStatus != "running" {
		t.Fatalf("docker_status = %q, want running", n.Deployments[0].DockerStatus)
	}
	if len(n.CachedWeightRepos) != 1 || n.CachedWeightRepos[0] != "Inferact/Qwen3.8-27B-NVFP4" {
		t.Fatalf("cached repos wrong: %+v", n.CachedWeightRepos)
	}
	// A freshly-landed snapshot is seconds old, never negative or stale.
	if n.SnapshotAgeS < 0 || n.SnapshotAgeS > 5 {
		t.Fatalf("snapshot_age_s = %v, want a small non-negative number", n.SnapshotAgeS)
	}
}

func TestAPINodesSortsNodesAndDeployments(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("zeta-node", &pb.NodeSnapshot{NodeId: "zeta-node", PoolGib: 16, ReserveGib: 2})
	nodes.ReplaceSnapshot("alpha-node", &pb.NodeSnapshot{
		NodeId: "alpha-node", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{
			{RecipeId: "zzz-model", WeightsGib: 1, KvGib: 1},
			{RecipeId: "aaa-model", WeightsGib: 1, KvGib: 1},
		},
	})
	out := getNodesJSON(t, h)
	if len(out) != 2 || out[0].NodeID != "alpha-node" || out[1].NodeID != "zeta-node" {
		t.Fatalf("nodes not sorted by id: %v", []string{out[0].NodeID, out[1].NodeID})
	}
	deps := out[0].Deployments
	if len(deps) != 2 || deps[0].RecipeID != "aaa-model" || deps[1].RecipeID != "zzz-model" {
		t.Fatalf("deployments not sorted by recipe id: %+v", deps)
	}
}

func TestAPINodesReportsDisconnected(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("gx", &pb.NodeSnapshot{NodeId: "gx", PoolGib: 16, ReserveGib: 2})
	nodes.MarkDisconnected("gx")
	out := getNodesJSON(t, h)
	if len(out) != 1 || out[0].Connected {
		t.Fatalf("want one disconnected node, got %+v", out)
	}
}

func TestAPINodesEmptyFleetIsEmptyArrayNotNull(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if body := rec.Body.String(); body != "[]\n" {
		t.Fatalf("empty fleet body = %q, want %q (a JSON client must get an array, never null)", body, "[]\n")
	}
}

func TestAPINodesAbsentWithoutGRPC(t *testing.T) {
	// cmd/sous (nil gsrv/nodes) must not expose this route at all - it reads
	// s.nodes, which would nil-panic. The gate in server.go means the path
	// simply isn't registered, so the "GET /" catch-all answers.
	h := newTestServerNilGRPC(t)
	req := httptest.NewRequest("GET", "/api/nodes", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "application/json" {
		t.Fatalf("GET /api/nodes should not serve JSON on a nil-gRPC server; got %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// round2 rounds to 2 decimals so float noise (24.87+45.67 = 70.53999...)
// doesn't fail an exact-equality assertion on a value a human reads as 70.54.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
