// Package grpcclient is souslet's half of the connection: dial sous-api,
// hold the Connect stream open, and dispatch each incoming Envelope to a
// local Handlers method that does the actual Docker/fetch work via the
// existing deploy.Runtime/fetch.Manager/engine code, unchanged from how
// single-node Sous already used them.
package grpcclient

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

// Handlers turns an incoming Envelope command into a call against the
// machinery this node already had before multi-node existed: deploy.Runtime
// drives Docker directly, fetch.Manager downloads weights. Deliberately no
// deploy.Manager and no store.Store here - those own ordering (serialised
// loads, stop-before-start), capacity planning and on-disk records, none of
// which souslet is meant to decide for itself. sous-api holds that
// authority centrally; souslet only executes what it is told and reports
// what Docker and the local disk actually show.
type Handlers struct {
	Runtime  deploy.Runtime
	Fetch    *fetch.Manager
	ModelDir string

	// footprintsMu guards footprints, which HandleDeploy writes and
	// Snapshot reads - both reachable concurrently from the dispatch loop.
	footprintsMu sync.Mutex
	// footprints remembers each currently-deployed recipe's DECLARED
	// footprint (recipe.Footprint, i.e. WeightsGiB/KVGiB from the recipe's
	// own Declared field), keyed by recipe ID. This is the cheapest thing
	// Snapshot can report without a store: single-node Sous refines a
	// declared footprint against a measured observe.Observation once a
	// model has actually loaded, but that refinement needs
	// store.KindObservation, which souslet has no equivalent of. Declared
	// figures are an honest, if less precise, substitute - not a
	// regression this task is expected to fix.
	footprints map[string]recipe.Footprint
}

// rememberFootprint records a successfully deployed recipe's declared
// footprint under its recipe ID (pb.DeployCommand.RecipeId - the same
// identifier HandleUndeploy uses to derive the container to stop via
// engine.ContainerName, and therefore the same identifier Snapshot derives
// back out of the live container name) so Snapshot can find it later.
func (h *Handlers) rememberFootprint(recipeID string, f recipe.Footprint) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	if h.footprints == nil {
		h.footprints = make(map[string]recipe.Footprint)
	}
	h.footprints[recipeID] = f
}

// forgetFootprint drops a recipe's cached declared footprint once it is no
// longer deployed, so a stopped model does not keep contributing to
// Snapshot's capacity figures after it is gone.
func (h *Handlers) forgetFootprint(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.footprints, recipeID)
}

// footprintFor returns the zero recipe.Footprint for a recipe ID this
// process has no record of - an honest "unknown" (e.g. a container that
// predates this souslet process's current run, so it was never deployed
// through HandleDeploy), never a fabricated figure.
func (h *Handlers) footprintFor(recipeID string) recipe.Footprint {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	return h.footprints[recipeID]
}

// HandleDeploy starts a container from a recipe sent whole on the wire, so
// souslet never needs its own copy of the catalog. The recipe is untrusted
// input, not a local file, so engine.BuildSpec's validation is exactly what
// stands between a malformed recipe and a call into Docker.
func (h *Handlers) HandleDeploy(ctx context.Context, cmd *pb.DeployCommand) *pb.DeployResult {
	var rec recipe.Recipe
	if err := yaml.Unmarshal([]byte(cmd.RecipeYaml), &rec); err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: "invalid recipe: " + err.Error()}
	}
	spec, err := engine.BuildSpec(rec, int(cmd.WantPort), h.ModelDir)
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	containerID, err := h.Runtime.Start(ctx, spec)
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	h.rememberFootprint(cmd.RecipeId, rec.Declared)
	return &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: containerID, HostPort: cmd.WantPort}
}

// HandleUndeploy stops and removes the container. deploy.Runtime.Stop
// already treats "no such container" as success (see engine.Docker.Stop),
// so a redundant undeploy of something already gone reports success here
// too, matching the "missing record is success" philosophy that made
// single-node Sous's Undeploy idempotent.
func (h *Handlers) HandleUndeploy(ctx context.Context, cmd *pb.UndeployCommand) *pb.UndeployResult {
	if err := h.Runtime.Stop(ctx, engine.ContainerName(cmd.RecipeId)); err != nil {
		return &pb.UndeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	h.forgetFootprint(cmd.RecipeId)
	return &pb.UndeployResult{RecipeId: cmd.RecipeId}
}

// HandleFetch starts a weights download and returns immediately with its
// initial phase; fetch.Manager.Start is itself idempotent against a fetch
// already in flight, so a retried FetchCommand joins the existing job
// rather than starting a second one.
func (h *Handlers) HandleFetch(ctx context.Context, cmd *pb.FetchCommand) *pb.FetchProgress {
	job, err := h.Fetch.Start(ctx, cmd.Repo)
	if err != nil {
		return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(fetch.PhaseFailed)}
	}
	return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(job.Phase)}
}

// deleteWeights is a PLACEHOLDER, not the real implementation.
//
// The real guard logic (never delete a StateReferenced repo, require Force
// for StateProtected) lives in internal/larder/delete.go's Delete function
// today. Task 11 of the multi-node plan relocates that logic to this
// package and replaces this stub with the real call - do not build out the
// guard rules here, and do not extend this stub; replace it wholesale.
func deleteWeights(modelDir, repo string, force bool) (int64, error) {
	return 0, fmt.Errorf("not yet implemented")
}

// HandleDeleteWeights is a thin wrapper around deleteWeights (see its
// placeholder comment above) - this handler is dispatch only, never a
// reimplementation of the delete guard rules.
func (h *Handlers) HandleDeleteWeights(ctx context.Context, cmd *pb.DeleteWeightsCommand) *pb.DeleteWeightsResult {
	freed, err := deleteWeights(h.ModelDir, cmd.Repo, cmd.Force)
	if err != nil {
		return &pb.DeleteWeightsResult{Repo: cmd.Repo, Error: err.Error()}
	}
	return &pb.DeleteWeightsResult{Repo: cmd.Repo, BytesFreed: freed}
}

// containerNamePrefix mirrors engine's own unexported namePrefix
// ("sous-"), which engine.ContainerName applies and does not offer an
// exported inverse for. Safe to strip literally here: deploy.Runtime.States
// (engine.Docker.States) already excludes job containers
// (engine.JobPrefix, "sous-job-"), so every name reaching Snapshot carries
// exactly this one prefix.
const containerNamePrefix = "sous-"

// Snapshot builds this node's complete current state by asking Docker
// directly - never a cache - matching the "state is the container, not a
// record" philosophy internal/deploy and internal/fetch already followed in
// single-node Sous.
//
// Phase here is Docker's own raw status word (running, exited, restarting,
// ...), not the richer starting/ready/failed/stopping/gone vocabulary
// deploy.Manager.Phase computes - that computation needs a store.Record and
// a readiness probe, neither of which souslet's dispatch layer holds. This
// is the most complete answer available from deploy.Runtime alone.
//
// WeightsGib/KvGib come from the footprints cache HandleDeploy fills in -
// DECLARED figures, not a measured observe.Observation (single-node Sous's
// refinement of declared-vs-measured has no equivalent here, since souslet
// keeps no persistent store to refine against - an accepted simplification,
// not a regression). A recipe ID with no cache entry (never deployed
// through this handler in this process's current run - e.g. a container
// left over from before souslet last restarted) reports 0, which is an
// honest "unknown", not a claim that the deployment has no footprint.
//
// HostPort is left at its zero value: that data lives in store.Record,
// which this handler has no access to.
func (h *Handlers) Snapshot(ctx context.Context, nodeID string, poolGiB, reserveGiB float64) *pb.NodeSnapshot {
	states, _ := h.Runtime.States(ctx)
	deployments := make([]*pb.DeploymentState, 0, len(states))
	for name, st := range states {
		recipeID := strings.TrimPrefix(name, containerNamePrefix)
		footprint := h.footprintFor(recipeID)
		deployments = append(deployments, &pb.DeploymentState{
			RecipeId:   recipeID,
			Phase:      st.Status,
			WeightsGib: footprint.WeightsGiB,
			KvGib:      footprint.KVGiB,
		})
	}
	return &pb.NodeSnapshot{
		NodeId: nodeID, PoolGib: poolGiB, ReserveGib: reserveGiB,
		Deployments: deployments,
	}
}
