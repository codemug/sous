// deploy_grpc.go is the node-scoped half of deploy/undeploy/plan: sending a
// command to a specific connected node over grpcserver instead of running it
// against this process's own local deploy.Manager. This is additive - the
// legacy, non-node-scoped routes keep working through deploy.Manager exactly
// as before, per the multi-node rollout plan's migration period (removed in
// Task 14, once every deploy path is node-scoped).
package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

const (
	// sendTimeout bounds every ordinary deploy/undeploy/plan round trip to a
	// connected node. These are simple dispatch-and-reply exchanges on
	// souslet's side (start/stop a container - plan never leaves this
	// process at all, see planOnNode), so a node that hasn't answered within
	// a few seconds is not "about to" - it's unresponsive, and the caller
	// deserves a fast, honest error rather than a long hang.
	sendTimeout = 5 * time.Second
)

var (
	// fetchTimeout bounds the WHOLE fetch-and-poll loop below, not any single
	// FetchCommand round trip - a real weight download can take many minutes
	// for a 20+ GiB model, tens of GiB for the larger recipes in this fleet.
	// 30 minutes matches the brief's own suggested bound: long enough for a
	// real fetch to complete, short enough that a fetch that is genuinely
	// stuck (not just slow) still eventually fails instead of hanging this
	// call forever.
	//
	// A package-level var, not a const, purely so tests can shorten it - see
	// deploy_grpc_test.go's withFetchPollInterval-style helpers.
	fetchTimeout = 30 * time.Minute

	// fetchPollInterval is how long fetchWeights sleeps between FetchCommand
	// retries while souslet reports phase "downloading". souslet's own
	// fetch.Manager.Start (see HandleFetch's doc comment in
	// internal/grpcclient/handlers.go) is idempotent against a fetch already
	// in flight, so re-sending FetchCommand while one is running safely joins
	// the existing job rather than starting a second one - that is what
	// makes "keep sending FetchCommand" a correct way to poll rather than a
	// hack. A few seconds keeps the chatter low against a download that
	// realistically takes minutes, without risking a caller waiting long
	// past the point the download actually finished.
	fetchPollInterval = 3 * time.Second
)

// deployToNode sends a DeployCommand to nodeID and waits for its correlated
// DeployResult. recipeYAML travels whole rather than by ID because souslet
// keeps no catalog of its own - the recipe has to arrive with the command.
//
// Before deploying, it checks cat's last-known snapshot of nodeID: if the
// recipe's model is not among that node's CachedWeightRepos, it fetches the
// weights first (see fetchWeights) and waits for that to actually complete
// before ever sending the DeployCommand. A node deploy would otherwise fail
// (or, worse, silently trigger souslet's own on-demand fetch mid-deploy) for
// a model that has never been downloaded there - fetching first makes that
// step explicit, visible, and something this call actually waits on and can
// report an error for.
//
// If cat has no snapshot for nodeID at all (an unknown node), the fetch
// check is skipped and the DeployCommand is sent directly - the same "let
// the live gRPC call fail with its own error" behavior this function had
// before the cache check existed, rather than inventing a different error
// for a case gsrv.Send below already handles.
func deployToNode(gsrv *grpcserver.Server, cat *nodecatalog.Catalog, nodeID string, recipeYAML string, wantPort int, force bool) (*pb.DeployResult, error) {
	var rec recipe.Recipe
	if err := yaml.Unmarshal([]byte(recipeYAML), &rec); err != nil {
		return nil, fmt.Errorf("invalid recipe: %w", err)
	}

	if view, ok := cat.Node(nodeID); ok && !view.CachedWeightRepos[rec.Model] {
		if err := fetchWeights(gsrv, nodeID, rec.Model); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	reply, err := gsrv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{
		Deploy: &pb.DeployCommand{RecipeId: rec.ID, RecipeYaml: recipeYAML, WantPort: int32(wantPort), Force: force},
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

// fetchWeights makes sure repo's weights are on nodeID's disk before
// returning, blocking until that is true or fetchTimeout has passed.
//
// A single FetchCommand is NOT enough: souslet's HandleFetch dispatches
// straight to fetch.Manager.Start, and for a genuine cache miss that starts
// the download and returns immediately with phase "downloading" - it does
// not itself wait for the download to finish (see fetch.Manager.Start's own
// doc comment: "begins a download and returns immediately"). So this polls,
// resending FetchCommand every fetchPollInterval and inspecting the phase
// each reply reports:
//
//   - "done": the weights are on disk - return.
//   - "failed" or "absent": a terminal failure - return an error rather
//     than polling forever against a download that already gave up.
//   - "downloading": still in progress - sleep fetchPollInterval and ask
//     again. Re-sending FetchCommand while a job is running safely joins the
//     existing one instead of starting a second (see HandleFetch's doc
//     comment in internal/grpcclient/handlers.go) - that idempotency is what
//     makes polling via repeated FetchCommand correct here, not a hack.
//
// The whole loop, not any single round trip, is bounded by fetchTimeout:
// deployToNode must not hang forever on a fetch that never resolves either
// way.
func fetchWeights(gsrv *grpcserver.Server, nodeID, repo string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	for {
		reply, err := gsrv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_Fetch{
			Fetch: &pb.FetchCommand{Repo: repo},
		}})
		if err != nil {
			return fmt.Errorf("fetch %s on %s: %w", repo, nodeID, err)
		}
		p := reply.GetFetchProgress()
		if p == nil {
			return fmt.Errorf("fetch %s on %s: unexpected reply shape: %+v", repo, nodeID, reply)
		}
		switch fetch.Phase(p.Phase) {
		case fetch.PhaseDone:
			return nil
		case fetch.PhaseFailed, fetch.PhaseAbsent:
			return fmt.Errorf("fetch %s on %s did not complete: %+v", repo, nodeID, reply)
		case fetch.PhaseDownloading:
			// Fall through to the poll sleep below.
		default:
			return fmt.Errorf("fetch %s on %s: unrecognized phase %q", repo, nodeID, p.Phase)
		}
		select {
		case <-time.After(fetchPollInterval):
		case <-ctx.Done():
			return fmt.Errorf("fetch %s on %s did not complete within %s: %w", repo, nodeID, fetchTimeout, ctx.Err())
		}
	}
}

// undeployFromNode sends an UndeployCommand to nodeID and waits for its
// correlated UndeployResult.
func undeployFromNode(gsrv *grpcserver.Server, nodeID, recipeID string) (*pb.UndeployResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	reply, err := gsrv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_Undeploy{
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
	// WarnFreeGiB: 12 matches the constant cmd/sous/main.go hardcodes for the
	// legacy path's own capacity.Planner (not configurable via the recipe or
	// the wire protocol - a real per-fleet-observed swap-risk threshold, not
	// a placeholder). Omitting it here would silently drop the swap-risk
	// warning for every node-scoped plan/deploy; a fully per-node-
	// configurable value would need a wire-protocol change and is out of
	// scope for this task, so this hardcodes the same number the single-node
	// path already ships with.
	planner := capacity.Planner{PoolGiB: view.PoolGiB, ReserveGiB: view.ReserveGiB, WarnFreeGiB: 12}
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
