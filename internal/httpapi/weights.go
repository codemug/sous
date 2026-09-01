// weights.go is the node-scoped half of weight deletion: sending a
// DeleteWeightsCommand to a specific connected souslet over grpcserver, the
// same shape deploy_grpc.go's deployToNode/undeployFromNode already
// established for the other node-scoped commands. This is the "recipe-card
// cleanup" the multi-node plan's Task 11 describes, replacing the per-node
// browsing the old single-node larder page did (internal/larder, still
// reachable at /larder during the migration - see that package's own
// removal note in the multi-node plan's Task 14).
//
// The guard is split across two layers, each enforcing the half it actually
// has the information for:
//
//   - SAFETY (never delete a repo currently backing a live deployment on the
//     target node) lives in grpcclient/weights.go, souslet-side, because only
//     the node has live truth about what is running on it right now. force
//     never overrides this, at either layer.
//   - POLICY (an archived recipe's weights are rollback insurance - deleting
//     them turns a redeploy into a re-download) lives HERE, sous-api-side,
//     because only sous-api holds the recipe catalog needed to know whether
//     an archived recipe still references a repo. This does not round-trip
//     to the node: everything it needs is already in s.cat. Deliberately not
//     built on internal/larder's own classification, even though it is the
//     closest precedent, since that whole package is deleted once the
//     multi-node plan's Task 14 lands - anything layered on top of it here
//     would need re-deriving anyway. This reads internal/catalog directly
//     instead.
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

// archivedRecipesProtecting returns the IDs of every OTHER recipe (not
// excludeID) in cat that is Archived and still names model - the exact
// judgment internal/larder's own Scan used to make for its StateProtected
// classification (an archived recipe's weights are rollback insurance, not
// stale: deleting them turns a redeploy into a re-download during an
// outage), re-derived here against internal/catalog directly rather than
// reused from larder (see this file's package doc comment for why).
//
// excludeID is the recipe the delete request is FOR, not a recipe that can
// itself grant protection here: the question this answers is "does some
// OTHER recipe still need these weights as a rollback", not "is the recipe
// whose card you clicked archived" - those are different questions, and the
// second one is answered by whether the button appears on the card at all
// (models.html only offers a card's clear-weights action for a resident
// (recipe, node) pair in the first place).
func archivedRecipesProtecting(cat *catalog.Catalog, excludeID, model string) ([]string, error) {
	if model == "" {
		return nil, nil
	}
	recipes, err := cat.List()
	if err != nil {
		return nil, err
	}
	var protecting []string
	for _, rec := range recipes {
		if rec.ID == excludeID || rec.Model != model || !rec.Archived {
			continue
		}
		protecting = append(protecting, rec.ID)
	}
	return protecting, nil
}

// deleteWeightsOnNode is the HTTP handler wrapper: resolve recipeID's model
// repo from the catalog (souslet keeps no catalog of its own, so the repo
// has to be looked up here rather than sent as-is - see deployToNode's own
// comment on the same point), enforce the POLICY guard locally (see this
// file's package doc comment), then dispatch to nodeID over gsrv.
//
// force follows the same query-parameter convention the legacy
// /api/larder/delete route already used (s.deleteWeights in handlers.go:
// r.URL.Query().Get("force") == "true").
//
// JSON-only, like deployNode/undeployFromNode's node-scoped handlers: no
// existing form-posting UI predates the node-scoped routes, and the
// confirm-button in models.html below posts via fetch().
//
// A GuardError from the node (refused: currently deployed there, or an
// unsafe/unknown repo) comes back over the wire as a plain
// DeleteWeightsResult.Error string, not a typed error - so this reports it
// as 409 Conflict, matching the legacy /api/larder/delete route's own
// treatment of a *larder.GuardError, rather than trying to reconstruct a
// type across the wire. The local POLICY refusal below is reported the same
// way, for the same reason: one shape for "refused", regardless of which
// layer refused it.
func (s *Server) deleteWeightsOnNode(w http.ResponseWriter, r *http.Request) {
	recipeID := r.PathValue("recipeID")
	nodeID := r.PathValue("nodeID")
	if !recipe.ValidID(recipeID) {
		writeErr(w, http.StatusBadRequest, "invalid recipe id")
		return
	}

	rec, err := s.cat.Get(recipeID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	force := r.URL.Query().Get("force") == "true"

	// POLICY guard, enforced here rather than on the node - see this file's
	// package doc comment for why souslet cannot make this judgment itself.
	// No round trip: everything needed is already in s.cat.
	if !force {
		protectedBy, err := archivedRecipesProtecting(s.cat, recipeID, rec.Model)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(protectedBy) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf(
					"refusing to delete %s: archived recipe(s) %s still reference it as rollback insurance; retry with force=true to override",
					rec.Model, strings.Join(protectedBy, ", ")),
				"repo": rec.Model,
			})
			return
		}
	}

	res, err := deleteWeightsFromNode(s.gsrv, nodeID, rec.Model, force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if res.Error != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": res.Error, "repo": res.Repo})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
