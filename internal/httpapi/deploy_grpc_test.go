package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
)

// recipeYAMLFixture is a minimal, valid recipe rendered to YAML the way
// deployToNode ships it to a node - the whole recipe, not just its ID.
func recipeYAMLFixture(t *testing.T) string {
	t.Helper()
	rec := recipe.Recipe{ID: "dflash2", Kind: recipe.KindVLLM, Model: "Inferact/Qwen3.8-27B-NVFP4"}
	out, err := recipeToYAML(rec)
	if err != nil {
		t.Fatalf("recipeToYAML: %v", err)
	}
	return out
}

func TestDeployToNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := deployToNode(gsrv, "asus-gx10", recipeYAMLFixture(t), 18000, false)
	if err == nil {
		t.Fatal("expected an error deploying to a node with no live connection")
	}
}

func TestUndeployFromNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := undeployFromNode(gsrv, "asus-gx10", "dflash2")
	if err == nil {
		t.Fatal("expected an error undeploying from a node with no live connection")
	}
}

// TestPlanOnNodeUsesTheCatalogSnapshotNotALiveCall proves planOnNode never
// touches gRPC at all: a node the catalog has never heard of - so there is no
// live connection to even attempt - still gets a normal "not known" error
// rather than hanging or requiring a connection.
func TestPlanOnNodeReturnsErrorWhenNodeIsUnknown(t *testing.T) {
	cat := nodecatalog.New()
	_, err := planOnNode(cat, "dflash2", "asus-gx10", 24.5)
	if err == nil {
		t.Fatal("expected an error planning against a node the catalog has never seen")
	}
}

// TestPlanOnNodeComputesMarginFromTheCatalogSnapshot is the success path:
// once a node has reported a snapshot, planOnNode must answer from it
// synchronously, with the resident recipe itself excluded from its own
// footprint accounting.
func TestPlanOnNodeComputesMarginFromTheCatalogSnapshot(t *testing.T) {
	cat := nodecatalog.New()
	cat.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{
			{RecipeId: "already-resident", WeightsGib: 20, KvGib: 5},
		},
	})
	res, err := planOnNode(cat, "incoming-model", "asus-gx10", 60)
	if err != nil {
		t.Fatalf("planOnNode: %v", err)
	}
	// committed = 60 (incoming) + 25 (resident) = 85; usable = 121.6-24 = 97.6
	if !res.Fits {
		t.Fatalf("expected the plan to fit, got %+v", res)
	}
	wantMargin := (121.6 - 24) - 85
	if diff := res.MarginGiB - wantMargin; diff > 0.01 || diff < -0.01 {
		t.Fatalf("MarginGiB = %v, want ~%v (got %+v)", res.MarginGiB, wantMargin, res)
	}

	// Re-planning the ALREADY-resident recipe itself must not double-count
	// its own footprint against itself.
	res, err = planOnNode(cat, "already-resident", "asus-gx10", 25)
	if err != nil {
		t.Fatalf("planOnNode: %v", err)
	}
	if !res.Fits || res.CommittedGiB != 25 {
		t.Fatalf("re-planning a resident recipe must exclude its own prior entry, got %+v", res)
	}
}

// ---------- node-scoped routes, end to end ----------
//
// These exercise the actual HTTP wiring (route registration, the deploy/
// undeploy/plan handlers' nodeID branch, JSON response shapes) rather than
// deploy_grpc.go's package-level functions directly, on a server whose gsrv
// has no live souslet connection - proving the new routes fail the way a
// real disconnected/unknown node would, and that the pre-existing
// non-node-scoped routes keep working unchanged on the very same server.

func TestDeployNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
	})
	// kokoro is a 3 GiB recipe (well under this node's margin), so this
	// clears the capacity check and fails only because nothing ever
	// connected to gsrv as "asus-gx10" - the catalog knowing about a node
	// from a past snapshot is not the same as gsrv having a live stream for
	// it, exactly like a node that reported once and then dropped.
	rr := post(t, h, "/api/deploy/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (capacity fits, but no live connection): %s", rr.Code, rr.Body)
	}
}

func TestDeployNodeRouteReturnsConflictWhenCapacityDoesNotFit(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	// A tiny pool: qwen38 alone (24.87+45.67 GiB declared) cannot fit in 10.
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 10, ReserveGib: 0,
	})
	rr := post(t, h, "/api/deploy/qwen38/asus-gx10", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["margin_gib"]; !ok {
		t.Fatalf("refusal must report a margin: %v", got)
	}
}

func TestDeployNodeRouteReturns404ForUnknownNode(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/deploy/kokoro/never-seen-node", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body)
	}
}

func TestUndeployNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/undeploy/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rr.Code, rr.Body)
	}
}

func TestPlanNodeRouteReportsMargin(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
	})
	rr := send(t, h, http.MethodGet, "/api/plan/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["margin_gib"]; !ok {
		t.Fatalf("plan must report a margin, got %v", got)
	}
}

// TestLegacyDeployRouteStillWorksAlongsideNodeScoped proves the two routes
// genuinely coexist rather than one accidentally shadowing the other: the
// pre-existing, non-node-scoped route still deploys through the local
// deploy.Manager exactly as before, on a server that also has a real
// gsrv/nodes pair wired in for the new route.
func TestLegacyDeployRouteStillWorksAlongsideNodeScoped(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/deploy/kokoro", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy deploy: %d %s", rr.Code, rr.Body)
	}
}
