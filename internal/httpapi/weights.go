// weights.go is the node-scoped half of weight deletion: sending a
// DeleteWeightsCommand to a specific connected souslet over grpcserver, the
// same shape deploy_grpc.go's deployToNode/undeployFromNode already
// established for the other node-scoped commands. This is the "recipe-card
// cleanup" the multi-node plan's Task 11 describes, replacing the per-node
// browsing the old single-node larder page did (internal/larder, still
// reachable at /larder during the migration - see that package's own
// removal note in the multi-node plan's Task 14).
package httpapi

import (
	"context"
	"fmt"
	"net/http"

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

// deleteWeightsOnNode is the HTTP handler wrapper: resolve recipeID's model
// repo from the catalog (souslet keeps no catalog of its own, so the repo
// has to be looked up here rather than sent as-is - see deployToNode's own
// comment on the same point), then dispatch to nodeID over gsrv.
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
// type across the wire.
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
