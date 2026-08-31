# Sous multi-node redesign

**Goal:** Split Sous from a single-node recipe manager into a control plane
(API+UI, one instance, runs on uae-homenode) and a per-node worker
("souslet", runs on every model-serving node — asus-gx10, aorus-ubuntu, and
any future node) connected over a single gRPC connection that souslet
initiates. The API's node catalog tracks per-node capacity and which
recipes' weights are physically present on which node, drives a
drag-and-drop "deploy this recipe to this node" UI, and proxies all model
traffic (control-plane commands and inference requests alike) over that same
gRPC connection. The per-node "larder" concept is retired — weight presence
becomes a property the node catalog observes per (recipe, node) pair, not a
thing a node manages in isolation; deploying a recipe whose weights aren't
present on the target node triggers a download as part of the deploy flow.

**Architecture:** Two binaries built from one Go module. `sous-api` holds
the recipe catalog, the node catalog, the UI, and the gRPC server souslets
dial into. `souslet` holds a Docker engine wrapper (today's
`internal/engine`, mostly unmodified) and a gRPC client that dials
`sous-api` on startup and keeps one bidirectional stream open for the life
of the process. Every deploy/undeploy/fetch/proxy operation the API needs a
specific node to perform is framed as a message on that stream; souslet
executes it locally against Docker and streams results back on the same
stream. Reconnection is level-triggered: on connect (first time or after a
drop), souslet reports its complete current state in one shot, and the API
replaces its last-known view of that node with it — no event log, no
buffering, no history of what happened while disconnected.

**Tech stack:** Go (matching the existing module), `google.golang.org/grpc`
+ `google.golang.org/protobuf` (this project's first network-RPC
dependency — confirmed zero existing gRPC/proto code during exploration),
`crypto/x509` + a minimal self-issued CA for per-node mTLS client certs (no
external CA infrastructure), existing `docker/docker` SDK unchanged inside
souslet, existing `yaml.v3`-based `internal/store` unchanged but now living
only in `sous-api` (souslet is stateless — it derives everything from local
Docker, matching the existing `internal/fetch` and `internal/deploy` "state
is the container, not a record" philosophy already in this codebase).

## Global Constraints

- Every RPC-carrying message must be defined in `.proto` files under
  `proto/` and compiled with `protoc`/`buf` — pin exact tool versions in
  `Makefile`/CI, don't rely on ambient `protoc` on a developer's machine.
- souslet dials out; sous-api never initiates a TCP connection to a node.
  No inbound port needs to be open on asus-gx10 or aorus-ubuntu for this to
  work.
- Auth is mTLS: each node gets a client certificate signed by a CA sous-api
  generates and owns. No bearer tokens for the souslet↔API channel (the
  existing `SOUS_API_TOKEN` for external HTTP API clients is untouched and
  orthogonal).
- Reconciliation on reconnect is level-triggered (full state resync + diff),
  never edge-triggered (no buffered event log, no replay).
- Existing single-node deployments are NOT migrated in place. Cutover is:
  stop old Sous, install souslet, redeploy each recipe fresh through the new
  flow. No code is spent reading the old on-disk store format from souslet.
- Drag-and-drop recipe-card-onto-node-capacity-indicator is in scope for
  this plan, not deferred — this is the first meaningful client-side JS in
  a codebase that has been plain `html/template` + CSS with zero JS
  framework until now; keep it vanilla JS (no framework dependency) to stay
  consistent with the project's existing minimalism.
- `internal/fetch`, `internal/deploy`'s `Runtime` interface, and
  `internal/engine` are reused inside souslet largely as-is — they already
  sit behind clean interfaces (confirmed during exploration) and don't need
  a rewrite, only relocation and a driving layer (the gRPC handler) in front
  of them instead of `internal/httpapi`.
- `internal/larder` is deleted, not deprecated. Its scan-and-delete logic is
  absorbed into a new node-catalog-facing capability (see Weight Lifecycle
  below), but there is no "larder page" or per-node-only weight view in the
  new design.

---

## Components

### 1. `proto/souslet/v1/souslet.proto` — the wire contract

One service, one bidi-streaming RPC, everything else is message shapes
inside it:

```proto
service Souslet {
  rpc Connect(stream Envelope) returns (stream Envelope);
}

message Envelope {
  string stream_id = 1;      // correlates a request with its response(s)
  oneof payload {
    // souslet -> API, sent once immediately after connecting
    NodeSnapshot snapshot = 10;
    // API -> souslet
    DeployCommand deploy = 20;
    UndeployCommand undeploy = 21;
    PlanCommand plan = 22;
    FetchCommand fetch = 23;
    HTTPRequestHead http_req_head = 24;
    HTTPRequestChunk http_req_chunk = 25;
    // souslet -> API
    DeployResult deploy_result = 30;
    UndeployResult undeploy_result = 31;
    PlanResult plan_result = 32;
    FetchProgress fetch_progress = 33;
    HTTPResponseHead http_resp_head = 34;
    HTTPResponseChunk http_resp_chunk = 35;
    // either direction
    Heartbeat heartbeat = 40;
    Error error = 41;
  }
}

message NodeSnapshot {
  string node_id = 1;
  double pool_gib = 2;
  double reserve_gib = 3;
  repeated DeploymentState deployments = 4;   // one per resident container
  repeated string cached_weight_repos = 5;    // recipes physically on disk here
}

message DeploymentState {
  string recipe_id = 1;
  int32 host_port = 2;
  string phase = 3;            // mirrors internal/deploy.Phase's string form
  double weights_gib = 4;      // from the last real Observation, 0 if unmeasured
  double kv_gib = 5;
}
```

`HTTPRequestHead`/`HTTPRequestChunk`/`HTTPResponseHead`/`HTTPResponseChunk`
carry method, path, headers, and body bytes respectively, framed so a
chunked/SSE response streams naturally — one `HTTPResponseChunk` per
`Flush()` on the vLLM side, forwarded immediately rather than buffered.
`stream_id` scopes a request/response pair; sous-api generates a fresh one
per inbound HTTP request or per control command, souslet echoes it back on
every reply so out-of-order completions (a long inference request finishing
after a short control call started later) resolve correctly.

### 2. `sous-api` — the control plane

Everything `internal/httpapi`, `internal/catalog`, `internal/overlay`,
`internal/store`, `internal/gateway`, `internal/reqlog` already do, largely
unchanged, **plus**:

- **`internal/nodecatalog`** (new package). Holds the live, in-memory view
  of every connected node: `map[NodeID]*NodeState{PoolGiB, ReserveGiB,
  Deployments []DeploymentState, CachedWeights map[RecipeID]bool,
  Connected bool, LastSeen time.Time}`. Updated only by
  `internal/grpcserver`'s handler for `NodeSnapshot` messages (full
  replace, not a merge — level-triggered) and by individual
  `DeployResult`/`FetchProgress` messages between snapshots for live
  progress display. `Connected` flips to `false` when a souslet's stream
  ends; the node's last-known deployments stay visible (greyed out in the
  UI) rather than disappearing, so "what was running here before it went
  quiet" stays answerable.
- **`internal/grpcserver`** (new package). Implements the `Souslet` service.
  On `Connect`, verifies the client cert's CN against the node catalog's
  known node IDs (a node must be registered — see Node Registration below
  — before its cert is accepted), registers the stream, blocks reading
  `NodeSnapshot`/results off it into `nodecatalog`, and exposes a
  `Send(nodeID, Envelope) error` method that `internal/httpapi`'s deploy
  handlers and `internal/gateway`'s proxy call into instead of talking to
  `internal/deploy.Manager`/`internal/engine` directly.
- **`internal/httpapi`** changes: deploy/undeploy/plan handlers become thin
  — build the appropriate `Envelope`, call `grpcserver.Send`, wait for the
  correlated result (or time out). `internal/deploy.Manager` as it exists
  today (the one that calls `engine.Docker` directly) is deleted from
  `sous-api` entirely — that logic moves into souslet (see below). Capacity
  planning (`internal/capacity`) stays in `sous-api`, now reading residency
  from `nodecatalog` instead of a local `store.List(KindDeployment)`.
- **`internal/gateway`** changes: `Proxy` resolves which node serves the
  requested model (from `nodecatalog`), then instead of an in-process
  `httputil.ReverseProxy` to a local port, it opens a new `stream_id` on
  that node's `grpcserver` connection, writes `HTTPRequestHead`/`Chunk`
  messages for the inbound request, and streams `HTTPResponseHead`/`Chunk`
  messages back to the original client as they arrive — a real proxy, just
  over gRPC instead of a local TCP dial.

### 3. `souslet` — the worker

A new, small binary. Owns, largely verbatim from today's codebase:
`internal/engine` (Docker wrapper), `internal/fetch` (weight download,
already stateless-by-design), and a **trimmed** `internal/deploy` (the
`Runtime`-calling half — capacity *decisions* move to the API, but the
actual `engine.BuildSpec`/`Runtime.Start`/`Runtime.Stop` calls stay local,
since Docker access has to be local).

New: **`internal/grpcclient`** — dials `sous-api`'s gRPC address (from a
`-api-addr` flag/env var, tailnet address) with the node's mTLS client
cert, opens `Connect`, sends one `NodeSnapshot` immediately (built by
listing local Docker state via `engine.States`/`fetch.List`, exactly the
same "ask Docker, don't trust a cache" pattern `internal/deploy`'s `Phase`
already uses today), then loops: read `Envelope`s, dispatch
`DeployCommand`/`FetchCommand`/`HTTPRequestHead` etc. to the local
`engine`/`fetch` calls, write results back. Reconnects with exponential
backoff (capped, matching this project's existing GH Actions runner
reconnect pattern) on any stream error; on every successful (re)connect,
re-sends a fresh full `NodeSnapshot` before anything else.

souslet has **no UI, no HTTP server, no persistent store of its own** — if
it and its whole host reboot, the only source of truth it needs is "what is
Docker actually running right now," which is exactly what it already
computes to build the snapshot.

### 4. Node registration

A node has to exist in the catalog, and have a signed cert, before its
souslet can connect. Flow: an operator runs `sous-api node add
<node-id>` (a small CLI subcommand or an admin-page action), which
generates a keypair, signs it with `sous-api`'s CA, and prints/writes the
cert+key pair to copy onto the node (same "copy this artifact onto the
node by hand or via the existing adhoc-script channel" pattern this fleet
already uses for onboarding — no new distribution mechanism invented).
`souslet` is started with `-cert`/`-key`/`-ca` flags pointing at that
material. Revoking a node (decommissioning) removes it from the node
catalog's known-CN set; a souslet with a revoked cert is refused at
`Connect` time.

### 5. Weight lifecycle (replacing the larder)

The recipe becomes the entity that *declares* what weights it needs (its
`Model` field, unchanged from today). Presence is now a per-(recipe, node)
fact, reported by each souslet's `NodeSnapshot.cached_weight_repos` (built
by the same disk-scan `internal/larder.Scan` does today, just run locally
inside souslet against that node's own `ModelDir`, and folded into the
snapshot instead of served from a `/api/larder` endpoint).

- **Deploying** a recipe to a node whose `cached_weight_repos` doesn't
  include that recipe's `Model` triggers a `FetchCommand` first
  (souslet's existing `fetch.Manager.Start`, unchanged), then the
  `DeployCommand` once the fetch reports `done`. This is a new
  orchestration step in `sous-api`'s deploy handler, not new logic in
  `fetch` itself.
- **Cleanup** moves onto the recipe card in the UI: a "clear weights from
  disk" action per (recipe, node) pair the catalog shows weights resident
  on, sent as a new `DeleteWeightsCommand` souslet executes with the same
  guards `internal/larder/delete.go` has today (never delete if
  `StateReferenced` — i.e. deployed right now on that node; require
  confirmation if `StateProtected` — referenced only by an archived
  recipe). The guard logic itself (`internal/larder/delete.go`'s
  `Delete` function) moves into souslet essentially unchanged; only its
  caller changes from an HTTP handler to a gRPC command handler.
- There is no cross-node weight sharing or single shared store — "recipe-
  scoped" means the recipe is the one entity that names what's needed;
  each node's disk is still independently either populated or not, exactly
  as physics requires.

### 6. UI changes

`internal/ui/templates/node.html`'s "One pool, N GiB" singular dashboard
becomes a grid of per-node cards (`PoolGiB`/`ReserveGiB`/`MarginGiB`/
connected-state per node, reusing the existing `poolbar.html` partial per
card instead of once globally). `models.html`'s recipe cards gain a
per-node "resident here: yes/no" chip row (from the same catalog data) and
become drag sources; node cards become drop targets. Drop handler posts to
a new `POST /api/deploy/{recipeID}/{nodeID}` (replacing today's
`POST /api/deploy/{id}`, which had no node dimension) — implemented as
plain `fetch()` + native HTML5 drag-and-drop events, no framework, matching
the constraint above. A recipe card without a valid drop target (no node
has enough margin) shows that in its chip row rather than only failing
silently on drop.

## Data Flow

**Deploy:** UI drop (or `POST /api/deploy/{recipe}/{node}`) → `sous-api`
checks `nodecatalog` for that node's margin (same `capacity.Planner` logic,
now fed by `nodecatalog` residency instead of local `store`) → if weights
aren't in that node's `cached_weight_repos`, send `FetchCommand`, wait for
`done` → send `DeployCommand` → souslet runs `engine.BuildSpec` +
`Runtime.Start` locally, streams back `DeployResult` → `nodecatalog`
updated, UI reflects new state on next poll/event.

**Inference request:** client → `sous-api`'s existing `/v1/chat/completions`
gateway path → resolve node from `nodecatalog` → open `stream_id` on that
node's connection → forward request headers/body as `HTTPRequestHead`/
`Chunk` → souslet forwards to the local model container over plain
`net/http` (unchanged) → streams response back chunk-by-chunk → `sous-api`
writes those chunks to the original client as they arrive, preserving
streaming/SSE behavior end to end.

**Reconnect:** souslet's stream drops (network blip, sous-api restart, node
reboot) → souslet retries with backoff → on success, sends a fresh full
`NodeSnapshot` → `nodecatalog` replaces its entry for that node wholesale →
any deployments that vanished (container gone, e.g. exactly the kind of
"container needed recreating, not just restarting" issue found operating
this fleet) show up as gone in the next snapshot, full stop — no attempt to
explain or replay what happened while disconnected.

## Error Handling

- A `DeployCommand` that fails locally on souslet (capacity mismatch
  discovered late, Docker error) returns a `DeployResult` with an error
  field; `sous-api` surfaces it exactly where today's `CapacityError`/plain
  error surfaces in the UI. No new error taxonomy beyond what
  `internal/deploy`'s existing `CapacityError` pattern already provides.
- If a node is disconnected when a deploy/proxy is attempted against it,
  `sous-api` fails fast with a clear "node not connected" error rather than
  queuing — consistent with the "no buffering" reconciliation decision.
- gRPC stream-level errors (cert rejected, network partition) are logged on
  both sides; souslet's retry loop is the only recovery path, no manual
  "reconnect" action needed in the UI.

## Testing

- `internal/nodecatalog`, `internal/grpcserver`, `internal/grpcclient`:
  unit tests using an in-memory `bufconn` gRPC connection (standard Go gRPC
  testing pattern) between a fake souslet and a real `grpcserver`, covering
  snapshot replace-on-reconnect, command/result correlation by
  `stream_id`, and disconnect handling.
- `internal/fetch`/`internal/deploy`'s `Runtime`-calling half/`internal/
  engine`: existing tests carry over largely unchanged (relocated, not
  rewritten) since their public behavior doesn't change, only what drives
  them.
- End-to-end: a docker-in-docker or local-only test standing up `sous-api`
  and one `souslet` against a fake/no-op container runtime, exercising
  deploy → snapshot → proxy → undeploy, without needing real GPUs — mirrors
  how this project already tests without needing real vLLM containers
  today (check existing test patterns in `internal/httpapi/*_test.go`
  during implementation and match them).
- UI drag-drop: no automated test infrastructure exists for this project's
  UI today (plain html/template, no JS test runner) — manual verification
  against the running dev stack is the bar, consistent with how UI changes
  have been verified throughout this project's history.

## Migration / Rollout

1. Land `sous-api`/`souslet` split behind the existing single-node code
   staying intact until cutover (new packages added, old ones not deleted
   until the new path is proven).
2. Stand up `sous-api` on uae-homenode (new deployment, new port, doesn't
   touch anything currently running there).
3. Register asus-gx10, install `souslet` there, confirm it connects and
   reports an accurate snapshot of what's currently running under the OLD
   single-node Sous (informational only at this point — nothing is
   double-managed).
4. Cut over: stop old single-node Sous on asus-gx10, redeploy
   `qwen38-dflash2` fresh through the new `sous-api`+`souslet` path (per
   the earlier migration decision — clean cutover, not a state migration).
5. Register aorus-ubuntu the same way once its `souslet` exists (this was
   already going to be a fresh node with nothing to migrate).
6. Delete `internal/larder`, `internal/httpapi`'s single-node deploy path,
   and the old single-binary `cmd/sous/main.go` once both nodes are
   confirmed running clean on the new path.

## Open Risks

- **gRPC is genuinely new infrastructure for this codebase** (confirmed
  zero existing RPC code) — proto tooling, codegen-in-CI, and the
  hand-rolled multiplexing protocol are the biggest net-new engineering
  surface in this whole plan, not the node-catalog/UI work.
- **mTLS CA is new, minimal, self-run infrastructure.** No rotation/renewal
  automation is in scope for this plan — certs are treated as long-lived,
  manually reissued on revocation/decommission. Worth a follow-up if this
  fleet grows past a handful of nodes.
- **Proxying all inference traffic through one gRPC connection per node**
  means `sous-api` is now in the data path for every token of every request
  to every node, not just a control plane — a `sous-api` outage now takes
  down inference fleet-wide, not just management. This was discussed and
  accepted explicitly as a tradeoff for keeping one external endpoint.
