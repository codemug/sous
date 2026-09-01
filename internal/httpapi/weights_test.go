package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

const formCT = "application/x-www-form-urlencoded"

// ---------- deleteWeightsFromNode (package-level function, real gRPC round
// trip via a faked souslet) ----------
//
// Mirrors deploy_grpc_test.go's own split: TestDeployTriggersAFetchFirst...
// and friends drive deployToNode directly against a standalone
// grpcserver.New(nodes, nil) + dialFakeSousletRecording, rather than through the
// HTTP layer - httpapi.Server's gsrv field is unexported, so there is no way
// to recover the real *grpcserver.Server a request built via
// newTestServerWithNodes is actually using (Task 8's own report disclosed
// exactly this same gap: "no direct httpapi-level test of the true success
// round-trip, covered indirectly via grpcserver's own tests"). The
// HTTP-route-level tests below stay scoped to what post()/newTestServer...
// can actually exercise: routing, 404/405/502 on the paths that do not need
// a live fake souslet, plus a handful that DO use newTestServerWithGRPC for
// a genuine end-to-end round trip where that matters (the form-submission
// path in particular, since that is exactly what a review round found
// broken).

func TestDeleteWeightsFromNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New(), nil)
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
	gsrv := grpcserver.New(nodes, nil)
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
	gsrv := grpcserver.New(nodes, nil)
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

// ---------- node-scoped route, JSON/API surface (Content-Type other than
// application/x-www-form-urlencoded - wantsHTML(r) is false, so these never
// touch requireConfirm or the redirect path; see the form-submission tests
// further down for that surface). ----------

func TestDeleteWeightsNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	// qwen36, not qwen38: the seed catalog's qwen38 and qwen38-dflash2 share
	// a model on purpose (a draft-model pairing), which is exactly the
	// active-reference case the POLICY guard now refuses unconditionally -
	// qwen36 has no such sibling, so it is a clean "nothing else references
	// this repo" baseline for tests that are not themselves about that guard.
	rr := post(t, h, "/api/weights/qwen36/asus-gx10/delete", "", "")
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

func TestModelsPageOffersClearWeightsForAResidentNodeCachePair(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:            "asus-gx10",
		CachedWeightRepos: []string{"Qwen/Qwen3.6-35B-A3B-FP8"}, // qwen36's model, uniquely its own in the seed catalog
	})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, `action="/api/weights/qwen36/asus-gx10/delete"`) {
		t.Fatal("expected a plain clear-weights action for qwen36 on asus-gx10, got none")
	}
	if !strings.Contains(body, "asus-gx10: weights cached") {
		t.Fatal("expected a resident chip naming the node")
	}
}

func TestModelsPageOffersNoClearWeightsWhenNothingIsCached(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if strings.Contains(body, "/api/weights/") {
		t.Fatal("did not expect any clear-weights action when nothing is cached")
	}
}

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

// ---------- POLICY guard: two severity tiers ----------
//
// See weights.go's package doc comment for why this lives at the httpapi
// layer (sous-api has the recipe catalog; souslet does not), and for the
// StateReferenced (active, unconditional)/StateProtected (archived, force
// overrides) split this mirrors from internal/larder.

// archiveRecipeSharingModel creates a second recipe, archived, naming the
// same model as an existing one.
func archiveRecipeSharingModel(t *testing.T, h http.Handler, id, model string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"kind":"vllm","modality":"text","image":"x","model":%q,"archived":true}`, id, model)
	rr := post(t, h, "/api/recipes", "application/json", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seeding archived recipe %s: status = %d: %s", id, rr.Code, rr.Body)
	}
}

// activeRecipeSharingModel creates a second recipe, NOT archived, naming the
// same model as an existing one - the StateReferenced-equivalent case.
func activeRecipeSharingModel(t *testing.T, h http.Handler, id, model string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"kind":"vllm","modality":"text","image":"x","model":%q}`, id, model)
	rr := post(t, h, "/api/recipes", "application/json", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seeding active recipe %s: status = %d: %s", id, rr.Code, rr.Body)
	}
}

func TestDeleteWeightsNodeRouteRefusesWithoutForceWhenAnArchivedRecipeStillReferencesTheRepo(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	archiveRecipeSharingModel(t, h, "qwen36-old", "Qwen/Qwen3.6-35B-A3B-FP8") // qwen36's model

	rr := post(t, h, "/api/weights/qwen36/asus-gx10/delete", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (refused by the archived-recipe POLICY guard, before ever contacting the node): %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "qwen36-old") {
		t.Fatalf("expected the refusal to name the protecting archived recipe, got: %s", rr.Body)
	}
}

func TestDeleteWeightsNodeRouteSucceedsWithForceWhenAnArchivedRecipeStillReferencesTheRepo(t *testing.T) {
	h, nodes, gsrv := newTestServerWithGRPC(t)
	archiveRecipeSharingModel(t, h, "qwen36-old", "Qwen/Qwen3.6-35B-A3B-FP8")
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

	rr := post(t, h, "/api/weights/qwen36/asus-gx10/delete?force=true", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with force=true: %s", rr.Code, rr.Body)
	}
	if !gotForce {
		t.Fatal("souslet did not receive Force: true")
	}
}

// TestDeleteWeightsNodeRouteRefusesEvenWithForceWhenAnActiveRecipeStillReferencesTheRepo
// is Finding 2's own test: an ACTIVE (non-archived) other recipe still
// naming the repo is the StateReferenced-equivalent tier - unconditional,
// force must NOT override it, unlike the archived tier above. Per
// recipe.Archived's own doc comment, this is the MORE likely case to be
// redeployed soon, so it gets the STRONGER guard, not a weaker one.
func TestDeleteWeightsNodeRouteRefusesEvenWithForceWhenAnActiveRecipeStillReferencesTheRepo(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	activeRecipeSharingModel(t, h, "qwen38-alt", "Inferact/Qwen3.8-27B-NVFP4")

	rr := post(t, h, "/api/weights/qwen38/asus-gx10/delete?force=true", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (an active reference is never overridden by force): %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "qwen38-alt") {
		t.Fatalf("expected the refusal to name the referencing active recipe, got: %s", rr.Body)
	}
}

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

	rr := post(t, h, "/api/weights/qwen36/asus-gx10/delete", "", "") // no force
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without force (nothing archived or active references this repo): %s", rr.Code, rr.Body)
	}
}

// TestClassifyProtectionExcludesTheRequestingRecipeItselfAndSplitsByArchived
// is a direct unit test of the pure function against a standalone catalog
// (no HTTP server needed): a recipe does not count as its own protection,
// an archived other recipe lands in archivedBy, and an active other recipe
// lands in activeBy - the exact split Finding 2 asked for.
func TestClassifyProtectionExcludesTheRequestingRecipeItselfAndSplitsByArchived(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New(st)
	const model = "Inferact/Qwen3.8-27B-NVFP4"
	for _, r := range []recipe.Recipe{
		{ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "x", Model: model},
		{ID: "qwen38-rollback", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "x", Model: model, Archived: true},
		{ID: "qwen38-alt", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "x", Model: model},
	} {
		if err := cat.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// From qwen38's own perspective: the other two recipes split cleanly into
	// activeBy (qwen38-alt) and archivedBy (qwen38-rollback).
	activeBy, archivedBy, err := repoProtection(cat, "qwen38", model)
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedBy) != 1 || archivedBy[0] != "qwen38-rollback" {
		t.Fatalf("expected qwen38-rollback in archivedBy, got %v", archivedBy)
	}
	if len(activeBy) != 1 || activeBy[0] != "qwen38-alt" {
		t.Fatalf("expected qwen38-alt in activeBy, got %v", activeBy)
	}

	// From qwen38-rollback's own perspective (an archived recipe checking
	// its OWN repo): it must not count itself as protection, but the OTHER
	// two (qwen38 and qwen38-alt, both active) still show up in activeBy -
	// excludeID only ever removes itself, never anyone else.
	activeBy, archivedBy, err = repoProtection(cat, "qwen38-rollback", model)
	if err != nil {
		t.Fatal(err)
	}
	if len(archivedBy) != 0 {
		t.Fatalf("a recipe must not count itself as protection: archivedBy=%v", archivedBy)
	}
	if len(activeBy) != 2 {
		t.Fatalf("expected both qwen38 and qwen38-alt in activeBy, got %v", activeBy)
	}
}

// ---------- Finding 1: the real browser form-submission surface ----------
//
// Every test above posts with an empty or JSON Content-Type, which only
// exercises wantsHTML(r) == false - the JSON/API path. The confirm-button
// partial models.html actually renders posts
// application/x-www-form-urlencoded with a full-page navigation, which is a
// DIFFERENT code path (wantsHTML(r) == true) that a review round found was
// never exercised at all, and never handled: the handler unconditionally
// wrote JSON, so a real click navigated the whole page to a raw JSON blob.

// formPost simulates the confirm-button partial's actual POST: form-encoded,
// confirm=yes always present (see confirm.html - the hidden field is
// unconditional in the real form, this is not simulating a first, unconfirmed
// click).
func formPost(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if body != "" {
		body += "&"
	}
	body += "confirm=yes"
	return post(t, h, path, formCT, body)
}

func TestDeleteWeightsNodeRouteFormSubmissionRedirectsOnSuccess(t *testing.T) {
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

	rr := formPost(t, h, "/api/weights/qwen36/asus-gx10/delete", "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (a real form submission must redirect, never render raw JSON): %s", rr.Code, rr.Body)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/models?") {
		t.Fatalf("Location = %q, want a redirect back to /models", loc)
	}
	if strings.Contains(loc, "err=1") {
		t.Fatalf("Location = %q, a success must not carry err=1", loc)
	}
	// http.Redirect's own body is a short plain-text stub; a raw JSON object
	// (what the bug this guards against actually rendered to the browser) is
	// unmistakably different.
	if strings.HasPrefix(strings.TrimSpace(rr.Body.String()), "{") {
		t.Fatalf("body looks like the raw JSON response, not a redirect: %s", rr.Body)
	}
}

func TestDeleteWeightsNodeRouteFormSubmissionRedirectsOnPolicyRefusal(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	archiveRecipeSharingModel(t, h, "qwen36-old", "Qwen/Qwen3.6-35B-A3B-FP8")

	rr := formPost(t, h, "/api/weights/qwen36/asus-gx10/delete", "")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (a real form submission must redirect, never render raw JSON): %s", rr.Code, rr.Body)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/models?") {
		t.Fatalf("Location = %q, want a redirect back to /models", loc)
	}
	if !strings.Contains(loc, "err=1") {
		t.Fatalf("Location = %q, a refusal must carry err=1", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if msg := u.Query().Get("msg"); !strings.Contains(msg, "qwen36-old") {
		t.Fatalf("Location msg %q does not name the protecting recipe", msg)
	}
}

// TestDeleteWeightsNodeRouteFormSubmissionRequiresConfirmation proves
// requireConfirm actually gates this route on the html path, matching every
// other confirm-button-backed route (s.deleteWeights, keys.go's revoke,
// recipes.go's delete, hftoken.go's clear) - a form POST missing confirm=yes
// must be refused, and must never reach the node.
func TestDeleteWeightsNodeRouteFormSubmissionRequiresConfirmation(t *testing.T) {
	h, nodes, gsrv := newTestServerWithGRPC(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"})
	var contacted bool
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if env.GetDeleteWeights() != nil {
			contacted = true
		}
		return nil
	})
	defer stop()

	rr := post(t, h, "/api/weights/qwen36/asus-gx10/delete", formCT, "") // no confirm=yes
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (redirected with a not-confirmed message): %s", rr.Code, rr.Body)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "err=1") {
		t.Fatalf("Location = %q, an unconfirmed submission must carry err=1", loc)
	}
	if contacted {
		t.Fatal("the node must never be contacted for an unconfirmed request")
	}
}

// ---------- Finding 1(b): the button itself must reflect protection status ----------

// TestModelsPageHidesClearWeightsWhenAnActiveRecipeReferencesTheRepo proves
// the button disappears entirely (matching larder.html's own precedent - "a
// button that exists to say no") when the guard would refuse
// unconditionally, rather than being offered and then refused every time.
func TestModelsPageHidesClearWeightsWhenAnActiveRecipeReferencesTheRepo(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	activeRecipeSharingModel(t, h, "qwen38-alt", "Inferact/Qwen3.8-27B-NVFP4")
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", CachedWeightRepos: []string{"Inferact/Qwen3.8-27B-NVFP4"},
	})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if strings.Contains(body, `action="/api/weights/qwen38/asus-gx10/delete"`) ||
		strings.Contains(body, `action="/api/weights/qwen38/asus-gx10/delete?force=true"`) {
		t.Fatal("did not expect any clear-weights action for qwen38 while an active recipe still references its repo")
	}
	if !strings.Contains(body, "in use elsewhere") {
		t.Fatal("expected a chip explaining why no action is offered")
	}
}

// TestModelsPageOffersForceClearWeightsWhenOnlyAnArchivedRecipeReferencesTheRepo
// proves the force-only affordance actually renders with ?force=true baked
// into its Action, per Finding 1's own ask ("no way to override from the UI
// at all").
func TestModelsPageOffersForceClearWeightsWhenOnlyAnArchivedRecipeReferencesTheRepo(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	archiveRecipeSharingModel(t, h, "qwen36-old", "Qwen/Qwen3.6-35B-A3B-FP8")
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", CachedWeightRepos: []string{"Qwen/Qwen3.6-35B-A3B-FP8"},
	})

	body := send(t, h, http.MethodGet, "/models", "", "").Body.String()
	if !strings.Contains(body, `action="/api/weights/qwen36/asus-gx10/delete?force=true"`) {
		t.Fatal("expected a force-clear action (with ?force=true) for qwen36 on asus-gx10")
	}
	if strings.Contains(body, `action="/api/weights/qwen36/asus-gx10/delete"`) {
		t.Fatal("did not expect the plain (non-force) action while an archived recipe still references the repo")
	}
	if !strings.Contains(body, "Force clear weights") {
		t.Fatal("expected the force-only button's label")
	}
	if !strings.Contains(body, "qwen36-old") {
		t.Fatal("expected the Cost text to name the protecting archived recipe")
	}
}
