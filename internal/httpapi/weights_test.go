package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
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

// ---------- POLICY guard: an archived recipe still protects its repo ----------
//
// See weights.go's package doc comment for why this lives at the httpapi
// layer (sous-api has the recipe catalog; souslet does not) rather than in
// grpcclient alongside the SAFETY guard.

// archiveRecipeSharingModel creates a second recipe, archived, naming the
// same model as an existing one - the exact shape internal/larder's own
// StateProtected classification used to require (an archived recipe still
// referencing a repo that is otherwise unreferenced).
func archiveRecipeSharingModel(t *testing.T, h http.Handler, id, model string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"kind":"vllm","modality":"text","image":"x","model":%q,"archived":true}`, id, model)
	rr := post(t, h, "/api/recipes", "application/json", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seeding archived recipe %s: status = %d: %s", id, rr.Code, rr.Body)
	}
}

// TestDeleteWeightsNodeRouteRefusesWithoutForceWhenAnArchivedRecipeStillReferencesTheRepo
// proves the refusal happens BEFORE any node is contacted at all: no fake
// souslet is dialed here, and the node is not even known to the catalog -
// if this test passed only because deleteWeightsFromNode itself failed
// (e.g. "not connected"), it would be a false positive for the wrong
// reason, so the 409 (not 502) is what actually proves the POLICY guard
// fired first.
func TestDeleteWeightsNodeRouteRefusesWithoutForceWhenAnArchivedRecipeStillReferencesTheRepo(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	archiveRecipeSharingModel(t, h, "qwen38-old", "Inferact/Qwen3.8-27B-NVFP4") // qwen38's model

	rr := post(t, h, "/api/weights/qwen38/asus-gx10/delete", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (refused by the archived-recipe POLICY guard, before ever contacting the node): %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "qwen38-old") {
		t.Fatalf("expected the refusal to name the protecting archived recipe, got: %s", rr.Body)
	}
}

// TestDeleteWeightsNodeRouteSucceedsWithForceWhenAnArchivedRecipeStillReferencesTheRepo
// proves force=true actually reaches the node: with a real (faked) souslet
// connected and answering success, the SAME archived-recipe setup that
// TestDeleteWeightsNodeRouteRefusesWithoutForceWhenAnArchivedRecipeStill...
// above rejected outright now succeeds end to end, and the DeleteWeightsCommand
// souslet receives carries Force: true - souslet's own separate SAFETY guard
// (never delete something currently deployed) still stands as the final
// backstop, unchanged by this test.
func TestDeleteWeightsNodeRouteSucceedsWithForceWhenAnArchivedRecipeStillReferencesTheRepo(t *testing.T) {
	h, nodes, gsrv := newTestServerWithGRPC(t)
	archiveRecipeSharingModel(t, h, "qwen38-old", "Inferact/Qwen3.8-27B-NVFP4")
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})

	var gotForce bool
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if d := env.GetDeleteWeights(); d != nil {
			gotForce = d.Force
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
				DeleteWeightsResult: &pb.DeleteWeightsResult{Repo: d.Repo, BytesFreed: 24 << 30},
			}}
		}
		return nil
	})
	defer stop()

	rr := post(t, h, "/api/weights/qwen38/asus-gx10/delete?force=true", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with force=true: %s", rr.Code, rr.Body)
	}
	if !gotForce {
		t.Fatal("souslet did not receive Force: true")
	}
}

// TestDeleteWeightsNodeRouteDeletesCleanlyWithoutForceWhenNoArchivedRecipeReferencesTheRepo
// is the negative case: force is only required when the archived-reference
// condition actually holds, matching the old system's semantics - a repo
// nothing archived references must delete without needing force at all.
func TestDeleteWeightsNodeRouteDeletesCleanlyWithoutForceWhenNoArchivedRecipeReferencesTheRepo(t *testing.T) {
	h, nodes, gsrv := newTestServerWithGRPC(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})

	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if d := env.GetDeleteWeights(); d != nil {
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
				DeleteWeightsResult: &pb.DeleteWeightsResult{Repo: d.Repo, BytesFreed: 4096},
			}}
		}
		return nil
	})
	defer stop()

	rr := post(t, h, "/api/weights/qwen38/asus-gx10/delete", "", "") // no force
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without force (nothing archived references this repo): %s", rr.Code, rr.Body)
	}
}

// TestArchivedRecipesProtectingExcludesTheRequestingRecipeItself is a direct
// unit test of the pure catalog-reading function, against a standalone
// catalog (no HTTP server needed): an archived recipe does not protect its
// OWN repo against a delete request made for itself (see
// archivedRecipesProtecting's own doc comment for why excludeID is not
// eligible to grant its own protection), but a DIFFERENT archived recipe
// naming the same model does.
func TestArchivedRecipesProtectingExcludesTheRequestingRecipeItself(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New(st)
	const model = "Inferact/Qwen3.8-27B-NVFP4"
	for _, r := range []recipe.Recipe{
		{ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "x", Model: model},
		{ID: "qwen38-rollback", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "x", Model: model, Archived: true},
	} {
		if err := cat.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	protecting, err := archivedRecipesProtecting(cat, "qwen38-rollback", model)
	if err != nil {
		t.Fatal(err)
	}
	if len(protecting) != 0 {
		t.Fatalf("a recipe must not count itself as protection: %v", protecting)
	}

	protecting, err = archivedRecipesProtecting(cat, "qwen38", model)
	if err != nil {
		t.Fatal(err)
	}
	if len(protecting) != 1 || protecting[0] != "qwen38-rollback" {
		t.Fatalf("expected qwen38-rollback to protect qwen38's repo, got %v", protecting)
	}
}
