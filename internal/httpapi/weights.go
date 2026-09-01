// weights.go is the node-scoped half of weight deletion: sending a
// DeleteWeightsCommand to a specific connected souslet over grpcserver, the
// same shape deploy_grpc.go's deployToNode/undeployFromNode already
// established for the other node-scoped commands. This is the "recipe-card
// cleanup" the multi-node plan's Task 11 describes, replacing the per-node
// browsing the old single-node larder page did (internal/larder and its
// /larder page - deleted in the multi-node plan's Task 14).
//
// The guard is split across two layers, each enforcing the half it actually
// has the information for:
//
//   - SAFETY (never delete a repo currently backing a live deployment on the
//     target node) lives in grpcclient/weights.go, souslet-side, because only
//     the node has live truth about what is running on it right now. force
//     never overrides this, at any layer.
//   - POLICY lives HERE, sous-api-side, because only sous-api holds the
//     recipe catalog needed to know whether some OTHER recipe still
//     references a repo - souslet never sees the catalog at all (DeployCommand
//     carries a recipe's full YAML precisely "so souslet needs no catalog of
//     its own"). This does not round-trip to the node: everything it needs
//     is already in s.cat. Deliberately not built on internal/larder's own
//     classification, even though it was the closest precedent, since that
//     whole package is gone now (Task 14) - anything layered on top of it
//     here would have needed re-deriving anyway. This reads internal/catalog
//     directly instead.
//
// POLICY itself has two severity tiers, mirroring the original larder's own
// StateReferenced/StateProtected split exactly (see classifyProtection):
//
//   - an ACTIVE (non-archived) other recipe still referencing the repo is
//     the StateReferenced case - unconditional, force never overrides it,
//     because recipe.Archived's own doc comment is explicit that an active,
//     merely-not-currently-deployed recipe is the MORE likely one to be
//     redeployed soon, not the less likely one;
//   - an ARCHIVED other recipe still referencing the repo is the
//     StateProtected case - rollback insurance, force DOES override it.
//
// Both server-rendered browser requests (the confirm-button forms this
// package's templates render, Content-Type
// application/x-www-form-urlencoded - see wantsHTML) and API/fetch-style
// JSON callers are supported, matching every other confirm-button-backed
// route in this codebase (see deleteWeightsOnNode's own doc comment).
package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/grpcserver"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
)

// deleteWeightsFromNode sends a DeleteWeightsCommand to nodeID and waits for
// its correlated DeleteWeightsResult - mirrors undeployFromNode's shape
// exactly (deploy_grpc.go).
func deleteWeightsFromNode(gsrv *grpcserver.Server, nodeID, repo string, force bool) (*pb.DeleteWeightsResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	reply, err := gsrv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_DeleteWeights{
		DeleteWeights: &pb.DeleteWeightsCommand{Repo: repo, Force: force},
	}})
	if err != nil {
		return nil, fmt.Errorf("delete weights on %s: %w", nodeID, err)
	}
	res := reply.GetDeleteWeightsResult()
	if res == nil {
		return nil, fmt.Errorf("delete weights on %s: unexpected reply shape", nodeID)
	}
	return res, nil
}

// weightsProtectionView is the UI's own copy of classifyProtection's split,
// precomputed once per recipe card in pageModels (handlers.go) so
// models.html can decide, without a round trip of its own, whether to
// render a plain "Clear weights" button, a force-only "Force clear weights"
// button, or no button at all (matching larder.html's own precedent: "a
// button that exists to say no" for a referenced/protected entry). *Text
// fields are pre-joined (strings.Join(..., ", ")) since html/template has no
// join function of its own and this project has not added one.
type weightsProtectionView struct {
	ActiveBy       []string
	ArchivedBy     []string
	ActiveByText   string
	ArchivedByText string
}

// classifyProtection separates every OTHER recipe (not excludeID) naming
// model into activeBy (not archived) and archivedBy (archived) - the exact
// split internal/larder's own Scan made between StateReferenced ("an active
// recipe names it, or it is deployed right now") and StateProtected ("only
// an archived recipe names it"), re-derived here against a recipe list
// already in hand rather than reused from larder (see this file's package
// doc comment for why).
//
// excludeID is the recipe the delete request is FOR, not a recipe that can
// itself grant protection here: the question this answers is "does some
// OTHER recipe still need these weights", not "is the recipe whose card you
// clicked archived" - those are different questions, and the second one is
// answered by whether a button appears on the card at all.
//
// Pure and side-effect-free so both deleteWeightsOnNode's guard (fed from a
// fresh s.cat.List() via repoProtection) and pageModels' UI classification
// (fed from a recipe list it already read for other reasons) can share one
// implementation without re-deriving the split or reading the catalog twice
// from the same code path.
func classifyProtection(recipes []recipe.Recipe, excludeID, model string) (activeBy, archivedBy []string) {
	if model == "" {
		return nil, nil
	}
	for _, rec := range recipes {
		if rec.ID == excludeID || rec.Model != model {
			continue
		}
		if rec.Archived {
			archivedBy = append(archivedBy, rec.ID)
		} else {
			activeBy = append(activeBy, rec.ID)
		}
	}
	return activeBy, archivedBy
}

// repoProtection is classifyProtection fed from a fresh catalog read - the
// HTTP guard's own entry point (deleteWeightsOnNode).
func repoProtection(cat *catalog.Catalog, excludeID, model string) (activeBy, archivedBy []string, err error) {
	recipes, err := cat.List()
	if err != nil {
		return nil, nil, err
	}
	activeBy, archivedBy = classifyProtection(recipes, excludeID, model)
	return activeBy, archivedBy, nil
}

// weightsRefused answers a refusal (or any other failure) on whichever
// surface deleteWeightsOnNode was actually reached from - see that
// function's own doc comment for why this branch is load-bearing, not
// cosmetic: every other confirm-button-backed destructive route in this
// codebase (s.deleteWeights, s.deploy's capacity refusal, s.requireConfirm)
// branches on wantsHTML the same way, and a route driven by the SAME
// confirm-button partial that skips it sends a real browser's full-page
// navigation to a raw JSON body instead of back to /models with a banner.
func (s *Server) weightsRefused(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if wantsHTML(r) {
		s.redirect(w, r, "/models", msg, true)
		return
	}
	writeErr(w, code, msg)
}

// deleteWeightsOnNode is the HTTP handler wrapper: resolve recipeID's model
// repo from the catalog (souslet keeps no catalog of its own, so the repo
// has to be looked up here rather than sent as-is - see deployToNode's own
// comment on the same point), enforce the two-tier POLICY guard locally
// (see this file's package doc comment), then dispatch to nodeID over gsrv.
//
// force follows the same query-parameter convention the retired
// /api/larder/delete route used (r.URL.Query().Get("force") == "true") - it
// survives on a POSTed form because it lives in the URL (models.html's
// force-clear button bakes ?force=true into the confirm-button's Action),
// not the body.
//
// Both surfaces this route can be reached from are handled, exactly like
// every other confirm-button-backed destructive route in this codebase
// (s.deploy's capacity refusal path, s.requireConfirm itself - see
// internal/httpapi/handlers.go and confirm.go):
//   - a real browser submitting the confirm-button's form
//     (Content-Type: application/x-www-form-urlencoded, wantsHTML(r) true) -
//     every outcome redirects back to /models with a ?msg=...&err=1 banner
//     via weightsRefused/s.redirect, matching requireConfirm's own pattern,
//     rather than ever rendering raw JSON as if it were a page;
//   - a JSON/fetch-style API caller (any other Content-Type) - every outcome
//     is a JSON body with a real status code, as before.
//
// requireConfirm gates this the same way it gates every other
// confirm-button-driven route: the confirm-button partial always posts
// confirm=yes on the html path, so this is a no-op for the real button and a
// backstop against a stray non-browser POST that skipped the two-click
// drawer (a form post can arrive from anywhere - see confirm.go's own doc
// comment on confirmed()).
func (s *Server) deleteWeightsOnNode(w http.ResponseWriter, r *http.Request) {
	recipeID := r.PathValue("recipeID")
	nodeID := r.PathValue("nodeID")
	if !recipe.ValidID(recipeID) {
		s.weightsRefused(w, r, http.StatusBadRequest, "invalid recipe id")
		return
	}

	rec, err := s.cat.Get(recipeID)
	if err != nil {
		s.weightsRefused(w, r, http.StatusNotFound, err.Error())
		return
	}

	if wantsHTML(r) && !s.requireConfirm(w, r, rec.Model+" on "+nodeID, "/models") {
		return
	}

	force := r.URL.Query().Get("force") == "true"

	// POLICY guard, enforced here rather than on the node - see this file's
	// package doc comment for why souslet cannot make this judgment itself,
	// and for the two distinct severity tiers below.
	activeBy, archivedBy, err := repoProtection(s.cat, recipeID, rec.Model)
	if err != nil {
		s.weightsRefused(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	// StateReferenced-equivalent: an active recipe still names this repo.
	// UNCONDITIONAL - force must never override this tier, exactly like the
	// original (and like souslet's own live-deployment SAFETY guard).
	if len(activeBy) > 0 {
		s.weightsRefused(w, r, http.StatusConflict, fmt.Sprintf(
			"refusing to delete %s: active recipe(s) %s still reference it; this cannot be overridden with force",
			rec.Model, strings.Join(activeBy, ", ")))
		return
	}
	// StateProtected-equivalent: only an archived recipe names this repo -
	// rollback insurance. force DOES override this tier.
	if !force && len(archivedBy) > 0 {
		s.weightsRefused(w, r, http.StatusConflict, fmt.Sprintf(
			"refusing to delete %s: archived recipe(s) %s still reference it as rollback insurance; retry with force=true to override",
			rec.Model, strings.Join(archivedBy, ", ")))
		return
	}

	res, err := deleteWeightsFromNode(s.gsrv, nodeID, rec.Model, force)
	if err != nil {
		s.weightsRefused(w, r, http.StatusBadGateway, err.Error())
		return
	}
	if res.Error != "" {
		// A GuardError from the node itself (refused: currently deployed
		// there, or an unsafe/unknown repo) comes back over the wire as a
		// plain DeleteWeightsResult.Error string, not a typed error - 409,
		// same shape as the local POLICY refusals above and the legacy
		// /api/larder/delete route's own treatment of a *larder.GuardError.
		s.weightsRefused(w, r, http.StatusConflict, res.Error)
		return
	}
	if wantsHTML(r) {
		s.redirect(w, r, "/models", "cleared "+rec.Model+" from "+nodeID, false)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
