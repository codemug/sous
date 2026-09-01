// deploy_grpc.go is the node-scoped half of deploy/undeploy/plan: sending a
// command to a specific connected node over grpcserver instead of running it
// against this process's own local deploy.Manager. This is additive - the
// legacy, non-node-scoped routes keep working through deploy.Manager exactly
// as before, per the multi-node rollout plan's migration period (removed in
// Task 14, once every deploy path is node-scoped).
package httpapi

import (
	"fmt"

	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

// deployToNode sends a DeployCommand to nodeID and waits for its correlated
// DeployResult. recipeYAML travels whole rather than by ID because souslet
// keeps no catalog of its own - the recipe has to arrive with the command.
func deployToNode(gsrv *grpcserver.Server, nodeID string, recipeYAML string, wantPort int, force bool) (*pb.DeployResult, error) {
	reply, err := gsrv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{
		Deploy: &pb.DeployCommand{RecipeYaml: recipeYAML, WantPort: int32(wantPort), Force: force},
	}})
	if err != nil {
		return nil, fmt.Errorf("deploy to %s: %w", nodeID, err)
	}
	res := reply.GetDeployResult()
	if res == nil {
		return nil, fmt.Errorf("deploy to %s: unexpected reply shape", nodeID)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("deploy to %s: %s", nodeID, res.Error)
	}
	return res, nil
}

// undeployFromNode sends an UndeployCommand to nodeID and waits for its
// correlated UndeployResult.
func undeployFromNode(gsrv *grpcserver.Server, nodeID, recipeID string) (*pb.UndeployResult, error) {
	reply, err := gsrv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Undeploy{
		Undeploy: &pb.UndeployCommand{RecipeId: recipeID},
	}})
	if err != nil {
		return nil, fmt.Errorf("undeploy from %s: %w", nodeID, err)
	}
	res := reply.GetUndeployResult()
	if res == nil {
		return nil, fmt.Errorf("undeploy from %s: unexpected reply shape", nodeID)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("undeploy from %s: %s", nodeID, res.Error)
	}
	return res, nil
}

// planOnNode answers "would incomingGiB more fit on nodeID" from this
// process's own nodecatalog snapshot, NOT a live round-trip to the node.
//
// This deliberately does not go over gRPC, unlike deployToNode/
// undeployFromNode. The wire protocol defines PlanCommand/PlanResult (Task
// 1), but no souslet-side dispatcher anywhere in this design ever handles an
// incoming Envelope_Plan - grpcclient.Handlers (Task 5) only implements
// HandleDeploy/HandleUndeploy/HandleFetch/HandleDeleteWeights. Sending a
// PlanCommand today would just sit unanswered until the node's connection
// drops. nodecatalog's last-known snapshot already carries everything a plan
// needs - PoolGiB, ReserveGiB and each resident's declared footprint - so
// this reuses the exact same capacity.Planner algorithm the single-node path
// used, fed from the catalog instead of a live query. (Precedent: Task 2's
// review made the identical call on VerifiedNodeID - build what has a real
// caller, not what the interface line oversold; revisit if a later task
// wires a real HandlePlan and gives this a reason to become an RPC.)
func planOnNode(nodes *nodecatalog.Catalog, recipeID, nodeID string, incomingGiB float64) (capacity.Result, error) {
	view, ok := nodes.Node(nodeID)
	if !ok {
		return capacity.Result{}, fmt.Errorf("node %q is not known", nodeID)
	}
	resident := make([]capacity.Entry, 0, len(view.Deployments))
	for _, d := range view.Deployments {
		// Exclude the recipe being planned itself: re-planning a model
		// already resident on this node must not double-count its own
		// footprint against itself.
		if d.RecipeId == recipeID {
			continue
		}
		resident = append(resident, capacity.Entry{ID: d.RecipeId, GiB: d.WeightsGib + d.KvGib})
	}
	planner := capacity.Planner{PoolGiB: view.PoolGiB, ReserveGiB: view.ReserveGiB}
	return planner.Plan(resident, capacity.Entry{ID: recipeID, GiB: incomingGiB}), nil
}

// recipeToYAML renders rec the way it travels to a node: the whole recipe,
// not just its ID, since souslet has no catalog of its own to look one up in.
func recipeToYAML(rec recipe.Recipe) (string, error) {
	b, err := yaml.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("marshal recipe %s: %w", rec.ID, err)
	}
	return string(b), nil
}
