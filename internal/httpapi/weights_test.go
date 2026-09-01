package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

// ---------- deleteWeightsFromNode (package-level function, real gRPC round
// trip via a faked souslet) ----------
//
// Mirrors deploy_grpc_test.go's own split: TestDeployTriggersAFetchFirst...
// and friends drive deployToNode directly against a standalone
// grpcserver.New(nodes) + dialFakeSousletRecording, rather than through the
// HTTP layer - httpapi.Server's gsrv field is unexported, so there is no way
// to recover the real *grpcserver.Server a request built via
// newTestServerWithNodes is actually using (Task 8's own report disclosed
// exactly this same gap: "no direct httpapi-level test of the true success
// round-trip, covered indirectly via grpcserver's own tests"). The
// HTTP-route-level tests below stay scoped to what post()/newTestServer...
// can actually exercise: routing, 404/405/502 on the paths that do not need
// a live fake souslet.

func TestDeleteWeightsFromNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := deleteWeightsFromNode(gsrv, "asus-gx10", "Inferact/Qwen3.8-27B-NVFP4", false)
	if err == nil {
		t.Fatal("expected an error deleting weights on a node with no live connection")
	}
}

// TestDeleteWeightsFromNodeSucceedsAndReportsBytesFreed proves the whole
// wire round trip actually works: a real DeleteWeightsCommand goes out, a
// real DeleteWeightsResult correlated by stream_id comes back.
func TestDeleteWeightsFromNodeSucceedsAndReportsBytesFreed(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})
	gsrv := grpcserver.New(nodes)
	var gotRepo string
	var gotForce bool
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if d := env.GetDeleteWeights(); d != nil {
			gotRepo, gotForce = d.Repo, d.Force
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
				DeleteWeightsResult: &pb.DeleteWeightsResult{Repo: d.Repo, BytesFreed: 4096},
			}}
		}
		return nil
	})
	defer stop()

	res, err := deleteWeightsFromNode(gsrv, "asus-gx10", "Inferact/Qwen3.8-27B-NVFP4", true)
	if err != nil {
		t.Fatalf("deleteWeightsFromNode: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("unexpected error result: %s", res.Error)
	}
	if res.BytesFreed != 4096 {
		t.Fatalf("BytesFreed = %d, want 4096", res.BytesFreed)
	}
	if gotRepo != "Inferact/Qwen3.8-27B-NVFP4" {
		t.Fatalf("souslet received repo %q", gotRepo)
	}
	if !gotForce {
		t.Fatal("souslet did not receive force=true")
	}
}

// TestDeleteWeightsFromNodeSurfacesAGuardRefusal proves a node-side guard
// refusal (DeleteWeightsResult.Error set, not a transport error) comes back
// through deleteWeightsFromNode as a normal result the caller can inspect,
// not swallowed or turned into a Go error.
func TestDeleteWeightsFromNodeSurfacesAGuardRefusal(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})
	gsrv := grpcserver.New(nodes)
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if d := env.GetDeleteWeights(); d != nil {
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
				DeleteWeightsResult: &pb.DeleteWeightsResult{
					Repo:  d.Repo,
					Error: "refusing to delete Inferact/Qwen3.8-27B-NVFP4: a recipe on this node is currently deployed with it",
				},
			}}
		}
		return nil
	})
	defer stop()

	res, err := deleteWeightsFromNode(gsrv, "asus-gx10", "Inferact/Qwen3.8-27B-NVFP4", true)
	if err != nil {
		t.Fatalf("deleteWeightsFromNode: %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected the guard refusal to survive as res.Error")
	}
}

// ---------- node-scoped route, end to end (routing only - see this file's
// own doc comment above for why a live-connection success path is not
// exercised at this layer) ----------

func TestDeleteWeightsNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/weights/qwen38/asus-gx10/delete", "", "")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rr.Code, rr.Body)
	}
}

func TestDeleteWeightsNodeRouteReturns404ForUnknownRecipe(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/weights/never-heard-of-it/asus-gx10/delete", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body)
	}
}

// TestWeightsDeleteRouteReturnsCleanErrorWhenGRPCIsNotConfigured mirrors
// TestNodeScopedRoutesReturnCleanErrorsWhenGRPCIsNotConfigured
// (deploy_grpc_test.go) for the new route: on cmd/sous's real
// nil-gsrv/nil-nodes configuration, this route must not exist at all rather
// than reach code that would nil-panic on a nil s.gsrv.
func TestWeightsDeleteRouteReturnsCleanErrorWhenGRPCIsNotConfigured(t *testing.T) {
	h := newTestServerNilGRPC(t)
	rr := post(t, h, "/api/weights/kokoro/asus-gx10/delete", "", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (route must not be registered): %s", rr.Code, rr.Body)
	}
}

// ---------- models.html wiring ----------

// TestModelsPageOffersClearWeightsForAResidentNodeCachePair proves the
// actual UI wiring, not just the route: a recipe card on /models must offer
// the "Clear weights" action for a node whose last-known snapshot lists that
// recipe's model in CachedWeightRepos, posting to exactly the route this
// task registered.
func TestModelsPageOffersClearWeightsForAResidentNodeCachePair(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:            "asus-gx10",
		CachedWeightRepos: []string{"Inferact/Qwen3.8-27B-NVFP4"}, // qwen38's model
	})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, "/api/weights/qwen38/asus-gx10/delete") {
		t.Fatal("expected a clear-weights action for qwen38 on asus-gx10, got none")
	}
	if !strings.Contains(body, "asus-gx10: weights cached") {
		t.Fatal("expected a resident chip naming the node")
	}
}

// TestModelsPageOffersNoClearWeightsWhenNothingIsCached is the negative
// case: a connected node with an empty CachedWeightRepos must not offer the
// action for any recipe - there is nothing there to clear.
func TestModelsPageOffersNoClearWeightsWhenNothingIsCached(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if strings.Contains(body, "/api/weights/") {
		t.Fatal("did not expect any clear-weights action when nothing is cached")
	}
}

// TestModelsPageOmitsClearWeightsRowOnASingleNodeServer proves the row
// disappears entirely (rather than rendering an empty one) on a server with
// no gRPC fleet configured - the shape cmd/sous runs in production today.
func TestModelsPageOmitsClearWeightsRowOnASingleNodeServer(t *testing.T) {
	h := newTestServer(t)
	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	// Not a bare "node-weights" substring check: that also matches the
	// page's own static .node-weights{...} CSS rule, which renders
	// unconditionally regardless of whether $.Nodes has anything in it. The
	// opening tag only renders inside the {{if and $model $.Nodes}} block.
	if strings.Contains(body, `class="node-weights"`) {
		t.Fatal("did not expect a per-node weights row on a single-node server")
	}
}
