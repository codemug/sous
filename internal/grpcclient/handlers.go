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

	// DropCaches mirrors deploy.Manager.DropCaches (internal/deploy/deploy.go)
	// exactly - injectable so tests do not need root, optional so a
	// Handlers built without one still deploys (nil is a no-op, not an
	// error). Legacy single-node Sous calls this before every container
	// start because vLLM sizes its KV cache against memory the kernel is
	// holding as page cache, and this fleet has OOM'd a model on asus-gx10
	// for exactly that reason (see stacks/sous/docker-compose.yml's own
	// account of it). That risk is identical on a souslet-managed node -
	// nothing about who dispatches the deploy changes what vLLM reads from
	// /proc - so this needs the same call, not a lesser one.
	DropCaches func() error

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
	// cleared by HandleUndeploy - and, for a container this process did not
	// deploy, adopted from its engine.ModelLabel (see adopt), so a restarted
	// souslet goes on guarding a running model's weights.
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
	//
	// A CACHE OF DOCKER, NOT THE RECORD. This map used to be the only place a
	// port lived, so every souslet restart - a node reboot included, which
	// Docker's restart policy brings every model back from on its old port -
	// left a process that reported host_port 0 for every deployment and
	// failed every proxied request with "no local deployment" until each
	// model was undeployed and redeployed (production, 2026-09-11 and
	// 2026-10-05). A container this process did not deploy is now adopted
	// from Docker's own answer (see adopt): on every snapshot - the first of
	// which goes out on connect, before any request can arrive - and on a
	// portFor miss in the proxy path (see localPort).
	ports map[string]int

	// changes counts every remember/forget above. A reconciliation pass reads
	// Docker WITHOUT holding footprintsMu - holding it would stall every
	// proxied request behind a Docker call - so by the time it has an answer,
	// a deploy or undeploy may have landed and made that answer stale. adopt
	// compares this against the count taken before the read and drops the
	// whole pass if anything moved, rather than writing a container an
	// undeploy has just removed back into the maps. Commands are minutes
	// apart and snapshots seconds apart, so a dropped pass costs one tick.
	changes uint64
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
	h.changes++
}

// forgetFootprint drops a recipe's cached declared footprint once it is no
// longer deployed, so a stopped model does not keep contributing to
// Snapshot's capacity figures after it is gone.
func (h *Handlers) forgetFootprint(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.footprints, recipeID)
	h.changes++
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
	h.changes++
}

// forgetModel drops a recipe's remembered model once it is no longer
// deployed - mirrors forgetFootprint exactly, same lifecycle, same reason.
func (h *Handlers) forgetModel(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.currentlyDeployed, recipeID)
	h.changes++
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
	h.changes++
}

// forgetPort drops a recipe's cached port once it is no longer deployed -
// mirrors forgetFootprint exactly, same lifecycle, same reason.
func (h *Handlers) forgetPort(recipeID string) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	delete(h.ports, recipeID)
	h.changes++
}

// portFor returns the local host port a recipe is currently deployed on, if
// this process deployed it (through HandleDeploy, in its current run) or has
// adopted it from Docker since, and has not since undeployed it. A pure read
// of the cache: localPort is what asks Docker on a miss.
func (h *Handlers) portFor(recipeID string) (int, bool) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	p, ok := h.ports[recipeID]
	return p, ok
}

// changeCount reads changes, for a reconciliation pass to hand back to adopt.
func (h *Handlers) changeCount() uint64 {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	return h.changes
}

// adopt fills in what this process does not know about a container from what
// Docker says about it - the recovery a restarted souslet needs, since its
// maps start empty while every model it ran before is still up. before is
// changeCount() as read BEFORE states was fetched; if any deploy or undeploy
// has landed since, states may be stale and the pass is dropped whole (see
// changes). Only ever fills gaps: an entry HandleDeploy made is the same fact
// Docker reports, and overwriting it would only open a window for a stale
// answer to win.
//
// THE PORT IS ADOPTED ONLY FROM A RUNNING CONTAINER, because adopting it is
// what makes the proxy path route there. A stopped or restarting container is
// not listening, and its port may not even be its own any more: the allocator
// before this fix tested ports only by binding, which a stopped container
// does not do, so it could hand a stopped model's port to a later deploy -
// routing to it would then deliver one model's requests to another. Such a
// container is still reported by Snapshot (Phase says what it is) and its port
// is still held back from new deploys (heldPorts); the first pass that finds
// it running adopts it. Footprints are not adopted at all: Snapshot reads them
// straight from the labels, which no pass can make stale.
func (h *Handlers) adopt(states map[string]engine.ContainerState, before uint64) {
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	if h.changes != before {
		return
	}
	// FORGET A PORT WHOSE CONTAINER IS NOT RUNNING. The table used to keep an
	// entry for as long as the process lived, so a model that exited went on
	// being routed to - and if another container had since come up on that
	// port (two created for the same one, which the allocator before this fix
	// allowed), to the wrong model. Dropping it sends the next request for it
	// to localPort's Docker lookup, which says what state it is really in; a
	// later pass adopts it again once it is running.
	//
	// Only for a container Docker LISTS as not running. One it does not list
	// at all is left alone: the entry may be from a deploy whose container
	// this answer predates by less than the change counter can see.
	for id := range h.ports {
		if st, ok := states[engine.ContainerName(id)]; ok && !st.Running() {
			delete(h.ports, id)
		}
	}
	for name, st := range states {
		recipeID := strings.TrimPrefix(name, containerNamePrefix)
		if _, known := h.ports[recipeID]; !known && st.Running() && st.HostPort > 0 {
			if h.ports == nil {
				h.ports = make(map[string]int)
			}
			h.ports[recipeID] = st.HostPort
		}
		// The model is adopted whatever the status: a stopped model is still
		// deployed (Snapshot still reports it, sous-api still places it), and
		// the restart policy will want its weights back.
		if _, known := h.currentlyDeployed[recipeID]; !known {
			if model := st.Labels[engine.ModelLabel]; model != "" {
				if h.currentlyDeployed == nil {
					h.currentlyDeployed = make(map[string]string)
				}
				h.currentlyDeployed[recipeID] = model
			}
		}
	}
}

// localPort is the port a proxied request for recipeID is forwarded to. A
// cache hit is answered from memory, as before. A miss - after a restart,
// a model the last snapshot could not adopt because Docker was out of reach
// or the container was not running yet - asks Docker once, adopts what it
// says, and otherwise says precisely why there is nowhere to send the
// request rather than dialling somewhere. A cache hit is never re-checked
// against Docker: that would put a Docker call in front of every request.
func (h *Handlers) localPort(ctx context.Context, recipeID string) (int, error) {
	// A FEW ATTEMPTS, because the change counter is one for the whole node: a
	// lookup for this model is invalidated by any other model's deploy or
	// undeploy landing while Docker is being read. That says nothing about
	// this model, and the request has done nothing wrong, so Docker is read
	// again rather than the request failed. The counter only moves in the
	// instant a command finishes, so a second read all but always stands.
	var err error
	for attempt := 0; attempt < localPortAttempts; attempt++ {
		var p int
		var stale bool
		if p, stale, err = h.lookupPort(ctx, recipeID); !stale {
			return p, err
		}
	}
	return 0, err
}

const localPortAttempts = 3

// lookupPort is one attempt of localPort. stale reports that Docker's answer
// was overtaken by a deploy or undeploy while it was being read, which is the
// one outcome worth trying again.
func (h *Handlers) lookupPort(ctx context.Context, recipeID string) (port int, stale bool, err error) {
	if p, ok := h.portFor(recipeID); ok {
		return p, false, nil
	}
	before := h.changeCount()
	states, err := h.Runtime.States(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("no local deployment for model %q known to this souslet, and Docker could not be asked: %w", recipeID, err)
	}
	h.adopt(states, before)
	if p, ok := h.portFor(recipeID); ok {
		return p, false, nil
	}
	// An exact name lookup in Docker's full list, never Docker's own name
	// filter: that is a substring match, under which "sous-qwen" also finds
	// "sous-qwen-big".
	st, ok := states[engine.ContainerName(recipeID)]
	switch {
	case !ok:
		return 0, false, fmt.Errorf("no local deployment for model %q", recipeID)
	case !st.Running():
		return 0, false, fmt.Errorf("model %q is %s on this node, not running", recipeID, st.Status)
	case st.HostPort == 0:
		return 0, false, fmt.Errorf("model %q publishes no host port on this node", recipeID)
	}
	// Running and published, yet not adopted: a deploy or undeploy landed
	// while Docker was being read, so adopt dropped the pass - and this answer
	// is the same stale one. It is not forwarded on: if what landed was this
	// model's undeploy, the port it names is being torn down or is already
	// another model's. Reported as stale so localPort reads Docker again.
	return 0, true, fmt.Errorf("model %q changed on this node while it was being looked up; retry", recipeID)
}

// footprintFor returns the zero recipe.Footprint for a recipe ID this
// process has no record of - an honest "unknown" (e.g. a container that
// predates this souslet process's current run, so it was never deployed
// through HandleDeploy), never a fabricated figure. Snapshot falls back to
// the container's own labels for those.
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
//
// Binding alone is not enough to call a port free here, though: see
// heldPorts for the ports that are spoken for with nothing bound to them.
func (h *Handlers) resolvePort(ctx context.Context, want int) (int, error) {
	alloc := h.Ports
	if alloc.Low == 0 && alloc.High == 0 {
		alloc = ports.Allocator{Low: defaultPortLow, High: defaultPortHigh}
	}
	held, err := h.heldPorts(ctx)
	if err != nil {
		return 0, err
	}
	if want == 0 {
		return alloc.FreeExcept(h.bindHost(), held)
	}
	if held[want] {
		return 0, fmt.Errorf("port %d belongs to an existing model container on this node, running or not", want)
	}
	if !alloc.IsFree(h.bindHost(), want) {
		return 0, fmt.Errorf("port %d is already in use on this node", want)
	}
	return want, nil
}

// heldPorts is every host port a model container on this node publishes or
// was created to publish, WHATEVER ITS STATE, plus every port this process
// has remembered. Asked of Docker at the moment of allocation, so it does not
// depend on any earlier recovery pass having succeeded.
//
// The bind probe already skips a port a RUNNING container holds. It cannot see
// one whose container is stopped or between restarts: nothing is bound, the
// probe succeeds, and the port used to be handed straight to the next deploy -
// leaving two containers wanting one port the moment Docker restarted the
// first.
//
// A Docker error fails the deploy rather than falling back to binding alone:
// it is exactly when Docker is out of reach that a restarting model's port
// cannot be seen, and the deploy needs Docker to start its container anyway.
func (h *Handlers) heldPorts(ctx context.Context) (map[int]bool, error) {
	states, err := h.Runtime.States(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing this node's model containers, to keep clear of their ports: %w", err)
	}
	held := make(map[int]bool, len(states))
	for _, st := range states {
		if st.HostPort > 0 {
			held[st.HostPort] = true
		}
	}
	h.footprintsMu.Lock()
	defer h.footprintsMu.Unlock()
	for _, p := range h.ports {
		held[p] = true
	}
	return held, nil
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
//
// The container also carries the recipe's declared footprint and model repo
// as labels (engine.BuildSpec), so a souslet restarted after this one can
// read back what this one remembers below.
func (h *Handlers) HandleDeploy(ctx context.Context, cmd *pb.DeployCommand) *pb.DeployResult {
	var rec recipe.Recipe
	if err := yaml.Unmarshal([]byte(cmd.RecipeYaml), &rec); err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: "invalid recipe: " + err.Error()}
	}
	port, err := h.resolvePort(ctx, int(cmd.WantPort))
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	spec, err := engine.BuildSpec(rec, port, h.ModelDir)
	if err != nil {
		return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	// A THIRD-PARTY IMAGE LISTENS WHERE ITS AUTHOR CHOSE. BuildSpec says 8000
	// because that is where Sous tells vLLM to listen; for KindContainer
	// nothing here controls the port - kokoro serves on 8880 - and publishing
	// the host port to the wrong one makes a container that starts, reports
	// running, and resets every connection. deploy.Manager has always
	// corrected this for the single-node path; this path never did, so a
	// container recipe has not been deployable to a node since nodes existed.
	//
	// Same order as there: the recipe's container_port when it says anything
	// (the only thing that can correct an image whose EXPOSE is wrong), then
	// the image's own EXPOSE, then BuildSpec's 8000 - an image that exposes
	// nothing still has to be deployable, and is visibly broken either way.
	if rec.Kind == recipe.KindContainer {
		if rec.ContainerPort > 0 {
			spec.ContainerPort = rec.ContainerPort
		} else if cp, err := h.Runtime.ImageExposedPort(ctx, rec.Image); err == nil && cp > 0 {
			spec.ContainerPort = cp
		}
	}
	if h.DropCaches != nil {
		if err := h.DropCaches(); err != nil {
			return &pb.DeployResult{RecipeId: cmd.RecipeId, Error: "dropping page cache: " + err.Error()}
		}
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
//
// The forgets below cover an entry adopted from Docker exactly as they cover
// one HandleDeploy made - the maps do not tell them apart - and bump changes,
// so a recovery pass that read Docker before the Stop cannot write the
// container back.
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
// left over from before souslet last restarted) reports the same declared
// figures from the container's own labels (engine.BuildSpec writes them), or
// 0 for a container created before those labels existed, which is an honest
// "unknown", not a claim that the deployment has no footprint.
//
// HostPort is Docker's own answer (engine.ContainerState.HostPort) whatever
// the Phase, since it says where the deployment is published, and Phase says
// whether anything is listening there. The ports cache is only the fallback
// for a container Docker gives no port for.
//
// The same Docker answer is then handed to adopt, which is how a restarted
// souslet recovers its routing table: the snapshot sent on connect - before
// any command or proxied request can arrive on that connection - is the
// startup recovery, and every later tick retries it, so Docker being out of
// reach at startup costs one snapshot interval, not a crash loop or a
// redeploy.
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
	before := h.changeCount()
	states, err := h.Runtime.States(ctx)
	if err == nil {
		h.adopt(states, before)
	}
	deployments := make([]*pb.DeploymentState, 0, len(states))
	for name, st := range states {
		recipeID := strings.TrimPrefix(name, containerNamePrefix)
		footprint := h.footprintFor(recipeID)
		if footprint == (recipe.Footprint{}) {
			footprint, _ = st.DeclaredFootprint() // zero when unlabelled: unknown, as before
		}
		port := st.HostPort
		if port == 0 {
			port, _ = h.portFor(recipeID)
		}
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
