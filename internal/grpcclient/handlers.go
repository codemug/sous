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
	"github.com/codemug/sous/internal/ports"
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

	// Ports allocates the host port a deployed model listens on when the
	// DeployCommand does not name one (WantPort 0 - which is every
	// drag-and-drop deploy, since the UI sends no port at all).
	//
	// ALLOCATED HERE, ON THE NODE, not on sous-api. The legacy single-node
	// path used the same ports.Allocator from inside deploy.Manager, and
	// that allocator decides availability by ACTUALLY BINDING the port
	// (see the ports package doc: a foreign process holding a port is
	// invisible to a records-based check, which is how k3s Traefik silently
	// owning 443 went undetected on this fleet). Binding is only meaningful
	// on the machine the container will run on, so sous-api cannot answer
	// this question for a remote node - it would be testing its own
	// listening sockets and handing the node a port some other process
	// there already holds.
	//
	// A zero-value Allocator falls back to defaultPortLow/defaultPortHigh,
	// so a Handlers built without one still allocates real ports rather
	// than silently handing Docker port 0.
	Ports ports.Allocator

	// BindHost is the host the allocator probes and the container publishes
	// on. Empty means 127.0.0.1, matching ports.Allocator's own usage in
	// deploy.Manager.
	BindHost string

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

	// currentlyDeployed remembers each currently-deployed recipe's model
	// repo (recipe.Recipe.Model, HuggingFace's "org/Name" form), keyed by
	// recipe ID. This is the one piece of catalog-shaped knowledge the
	// weight-delete guard (weights.go) needs and souslet can answer
	// honestly without a catalog of its own: DeployCommand always carries a
	// recipe's full YAML - "so souslet needs no catalog of its own", per
	// that message's own proto comment - so HandleDeploy already has
	// rec.Model in hand the moment a deploy happens; remembering it here
	// costs nothing extra and needs no round trip. Same lifecycle, same
	// lock as footprints and ports: populated by a successful HandleDeploy,
	// cleared by HandleUndeploy.
	currentlyDeployed map[string]string

	// ports remembers each currently-deployed recipe's local host port,
	// keyed by recipe ID - the "which local port is which recipe currently
	// on" state Task 9's proxied-HTTP path (handleProxyRequest, client.go)
	// needs to forward a request to the right container. Reuses
	// footprintsMu rather than a second lock: both maps are written
	// together by HandleDeploy and cleared together by HandleUndeploy, so
	// there is never a reason to hold one without the other.
	//
	// In the gRPC proxy path the "model name" a forwarded request declares
	// is the recipe ID directly - sous-api's gateway only rewrites a
	// request to a recipe's served-model alias in its LOCAL (Res/Cat)
	// forwarding path (internal/gateway/gateway.go's rewriteModel), which
	// the node-routed path does not use - so keying this map by recipe ID
	// is exactly what a proxied request's declared model matches against.
	ports map[string]int
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

// rememberModel records a successfully deployed recipe's model repo under
// its recipe ID, mirroring rememberFootprint exactly - see currentlyDeployed's
// doc comment for why the weight-delete guard needs this.
func (h *Handlers) rememberModel(recipeID, model string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	if h.currentlyDeployed == nil {
		h.currentlyDeployed = make(map[string]string)
	}
	h.currentlyDeployed[recipeID] = model
}

// forgetModel drops a recipe's remembered model once it is no longer
// deployed - mirrors forgetFootprint exactly, same lifecycle, same reason.
func (h *Handlers) forgetModel(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.currentlyDeployed, recipeID)
}

// rememberPort records a successfully deployed recipe's local host port
// under its recipe ID, so a later proxied HTTP request (Task 9's
// handleProxyRequest) can find the right container.
func (h *Handlers) rememberPort(recipeID string, port int) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	if h.ports == nil {
		h.ports = make(map[string]int)
	}
	h.ports[recipeID] = port
}

// forgetPort drops a recipe's cached port once it is no longer deployed -
// mirrors forgetFootprint exactly, same lifecycle, same reason.
func (h *Handlers) forgetPort(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.ports, recipeID)
}

// portFor returns the local host port a recipe is currently deployed on, if
// this process deployed it (through HandleDeploy, in its current run) and
// has not since undeployed it.
func (h *Handlers) portFor(recipeID string) (int, bool) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	p, ok := h.ports[recipeID]
	return p, ok
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

// Default port range, matching cmd/sous-api's own -port-low/-port-high
// defaults so a node's deployments land where this fleet already expects
// them even if souslet was started without the flags.
const (
	defaultPortLow  = 18000
	defaultPortHigh = 18100
)

func (h *Handlers) bindHost() string {
	if h.BindHost == "" {
		return "127.0.0.1"
	}
	return h.BindHost
}

// resolvePort turns a DeployCommand's want_port into the port the container
// will actually publish on, mirroring deploy.Manager.Deploy's own rule
// exactly: 0 means "pick a free one", and an explicitly requested port must
// actually be free (that is what makes ADOPTION of an already-running
// service's port safe - see the deploy handler's own comment on the -port
// query parameter).
//
// Before this, want_port went straight into engine.BuildSpec, so a
// drag-and-drop deploy (which sends no port at all) handed Docker HostPort 0
// - meaning "pick an ephemeral port" - and nothing anywhere recorded what
// Docker actually picked. The model ran but had no discoverable address:
// DeployResult.HostPort stayed 0, the snapshot's DeploymentState.HostPort
// stayed 0, and portFor returned 0, so the proxy path built
// http://127.0.0.1:0/... and failed.
func (h *Handlers) resolvePort(want int) (int, error) {
	alloc := h.Ports
	if alloc.Low == 0 && alloc.High == 0 {
		alloc = ports.Allocator{Low: defaultPortLow, High: defaultPortHigh}
	}
	if want == 0 {
		return alloc.Free(h.bindHost())
	}
	if !alloc.IsFree(h.bindHost(), want) {
		return 0, fmt.Errorf("port %d is already in use on this node", want)
	}
	return want, nil
}

// HandleDeploy starts a container from a recipe sent whole on the wire, so
// souslet never needs its own copy of the catalog. The recipe is untrusted
// input, not a local file, so engine.BuildSpec's validation is exactly what
// stands between a malformed recipe and a call into Docker.
//
// The host port is resolved HERE rather than by sous-api - see resolvePort
// and the Ports field's doc comment - and the resolved port, never the
// requested one, is what gets remembered, reported back in DeployResult, and
// carried in every subsequent NodeSnapshot.
func (h *Handlers) HandleDeploy(ctx context.Context, cmd *pb.DeployCommand) *pb.DeployResult {
	var rec recipe.Recipe
	if err := yaml.Unmarshal([]byte(cmd.RecipeYaml), &rec); err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: "invalid recipe: " + err.Error()}
	}
	port, err := h.resolvePort(int(cmd.WantPort))
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	spec, err := engine.BuildSpec(rec, port, h.ModelDir)
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	containerID, err := h.Runtime.Start(ctx, spec)
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	h.rememberFootprint(cmd.RecipeId, rec.Declared)
	h.rememberPort(cmd.RecipeId, port)
	h.rememberModel(cmd.RecipeId, rec.Model)
	return &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: containerID, HostPort: int32(port)}
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
	h.forgetPort(cmd.RecipeId)
	h.forgetModel(cmd.RecipeId)
	return &pb.UndeployResult{RecipeId: cmd.RecipeId}
}

// HandleFetch reports a repo's fetch status, starting a download only when
// none has ever been attempted (or its container is gone).
//
// This checks fetch.Manager.Status FIRST, and only falls through to
// fetch.Manager.Start when Status reports PhaseAbsent - deliberately NOT the
// other way around. fetch.Manager.Start is idempotent against a fetch
// already IN FLIGHT (a still-running job of the same name is left alone,
// its "downloading" phase reported back as-is), but it is NOT idempotent
// against a fetch that has already FINISHED: Start's own logic treats any
// non-running job of the same name - success or failure alike - as stale
// leftover blocking a new container, and removes and restarts it
// unconditionally (see Start's doc comment: "clear it so a retry is
// possible without a manual docker rm"). It has no way to tell "done" from
// "abandoned," because that was never a distinction its one caller before
// this task needed - the single-node dashboard's own POST /api/fetch calls
// Start exactly once, then polls Status (never Start) to watch it finish.
//
// FetchCommand's whole design (see deployToNode's fetchWeights in
// internal/httpapi/deploy_grpc.go) is a REPEATED poll, unlike that one-shot
// dashboard call - so calling Start on every poll, as this handler
// originally did, meant a poll landing just after a download actually
// finished would silently wipe it out and restart the whole thing from
// scratch, never once reporting "done" to a caller that keeps asking.
// Status is a pure read (see its own doc comment) with no such side effect,
// so checking it first - and answering "done"/"failed"/"downloading"
// straight from it - is what makes repeated FetchCommand polling actually
// safe to observe completion with. Start is reached only for a genuinely
// absent job: never attempted, or one whose container is gone (e.g.
// Forgotten via the dashboard's forgetFetch).
func (h *Handlers) HandleFetch(ctx context.Context, cmd *pb.FetchCommand) *pb.FetchProgress {
	if status := h.Fetch.Status(ctx, cmd.Repo); status.Phase != fetch.PhaseAbsent {
		return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(status.Phase)}
	}
	job, err := h.Fetch.Start(ctx, cmd.Repo)
	if err != nil {
		return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(fetch.PhaseFailed)}
	}
	return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(job.Phase)}
}

// deleteWeights and HandleDeleteWeights (the real, guarded implementation)
// live in weights.go - relocated there from internal/larder/delete.go, see
// that file's package doc comment for the guard behavior carried over and
// the one piece that could not be (StateProtected, which needs a recipe
// catalog souslet does not keep).

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
//
// CachedWeightRepos comes from scanning ModelDir/hub directly (see
// weights.go's scanWeightRepos, relocated from internal/larder/larder.go's
// Scan) - the same "the disk is the source of truth" philosophy the old
// single-node larder page was built on, now reported centrally so sous-api's
// nodecatalog can answer "is repo already on this node" (deployToNode's
// fetch-before-deploy check) and the recipe-card UI can show what is safe to
// clear. A scan failure is swallowed to an empty list rather than failing
// the whole snapshot, matching this function's existing tolerance of a
// States() error above - a disk read glitch should not take a node's entire
// heartbeat down.
func (h *Handlers) Snapshot(ctx context.Context, nodeID string, poolGiB, reserveGiB float64) *pb.NodeSnapshot {
	states, _ := h.Runtime.States(ctx)
	deployments := make([]*pb.DeploymentState, 0, len(states))
	for name, st := range states {
		recipeID := strings.TrimPrefix(name, containerNamePrefix)
		footprint := h.footprintFor(recipeID)
		// HostPort comes from the same ports cache HandleDeploy fills in and
		// the proxy path reads (portFor): the port this souslet actually
		// resolved and started the container on. A recipe this process did
		// not deploy in its current run reports 0, which is an honest
		// "unknown" - the same convention WeightsGib/KvGib already use here.
		port, _ := h.portFor(recipeID)
		deployments = append(deployments, &pb.DeploymentState{
			RecipeId:   recipeID,
			HostPort:   int32(port),
			Phase:      st.Status,
			WeightsGib: footprint.WeightsGiB,
			KvGib:      footprint.KVGiB,
		})
	}
	cached, _ := h.scanWeightRepos()
	return &pb.NodeSnapshot{
		NodeId: nodeID, PoolGib: poolGiB, ReserveGib: reserveGiB,
		Deployments:       deployments,
		CachedWeightRepos: cached,
	}
}
