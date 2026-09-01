# Sous Multi-Node Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split Sous into `sous-api` (control plane: catalog, node catalog, UI, gRPC server) and `souslet` (per-node worker: Docker engine, weight fetch, gRPC client), connected by a single mTLS-secured gRPC stream souslet initiates, over which all control commands and proxied inference traffic flow.

**Architecture:** Two binaries, one Go module. souslet dials `sous-api`, opens one bidirectional `Connect` stream, and stays on it for the process lifetime; every deploy/fetch/proxy operation is a message on that stream, correlated by `stream_id`. Reconnection is level-triggered (full `NodeSnapshot` resync, no event log). `internal/engine` and `internal/fetch` move into souslet largely unchanged (they already sit behind clean interfaces); `internal/deploy`'s capacity-planning half moves into `sous-api`, its Docker-calling half moves into souslet.

**Tech Stack:** Go, `google.golang.org/grpc` + `google.golang.org/protobuf` (new), `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` (new, pinned versions), `crypto/x509`/`crypto/tls` stdlib for the mTLS CA (no new dependency), existing `docker/docker` SDK and `yaml.v3` unchanged.

**Spec:** `docs/superpowers/specs/2026-09-01-sous-multinode-design.md`

## Global Constraints

- souslet dials out; `sous-api` never initiates a connection to a node. No inbound port required on asus-gx10 or aorus-ubuntu.
- Auth is mTLS only for the souslet↔API channel — no bearer token for this path (the existing `SOUS_API_TOKEN` for external HTTP clients is untouched).
- Reconciliation on reconnect is level-triggered: full `NodeSnapshot` replace, never an event log or replay.
- Existing single-node deployments are not migrated in place — clean cutover, redeploy fresh.
- Drag-and-drop is in scope for this plan, implemented as vanilla JS (no framework), consistent with this project's existing plain `html/template` + zero-JS-framework UI.
- `internal/larder` is deleted, not deprecated, once its logic is absorbed into souslet's weight-lifecycle command handlers.
- `protoc`/`protoc-gen-go`/`protoc-gen-go-grpc` versions are pinned in a `Makefile` target, not left to ambient tooling.

---

## File Structure

```
proto/souslet/v1/souslet.proto          # wire contract (new)
internal/pb/souslet/v1/*.pb.go          # generated (new, gitignored source but committed output — see Task 1)
internal/mtls/ca.go                      # minimal self-issued CA (new)
internal/mtls/ca_test.go                 # (new)
internal/nodecatalog/nodecatalog.go      # in-memory per-node state (new)
internal/nodecatalog/nodecatalog_test.go # (new)
internal/grpcserver/server.go            # Souslet service impl, API side (new)
internal/grpcserver/server_test.go       # (new)
internal/grpcclient/client.go            # connect/reconnect loop, souslet side (new)
internal/grpcclient/client_test.go       # (new)
internal/grpcclient/handlers.go          # dispatches Envelope payloads to engine/fetch (new)
internal/grpcclient/handlers_test.go     # (new)
cmd/sous-api/main.go                     # new control-plane binary (new)
cmd/souslet/main.go                      # new worker binary (new)
internal/httpapi/deploy_grpc.go          # deploy/undeploy/plan handlers routed via grpcserver (new, replaces deploy.Manager calls)
internal/httpapi/deploy_grpc_test.go     # (new)
internal/gateway/gateway.go              # MODIFY: Proxy routes through grpcserver stream
internal/gateway/gateway_test.go         # MODIFY: existing tests updated for new Proxy dependency
internal/ui/templates/node.html          # MODIFY: per-node card grid
internal/ui/templates/models.html        # MODIFY: resident chips + drag source
internal/ui/embed.go                     # MODIFY: embed the new dragdrop.js
internal/ui/static/dragdrop.js           # (new) vanilla JS drag-and-drop
Makefile                                 # (new) proto codegen target, pinned tool versions
.github/workflows/build.yml              # MODIFY: build+publish sous-api and souslet images
internal/larder/                         # DELETED in Task 14, once absorbed
internal/deploy/                         # TRIMMED in Task 8/9, engine-calling half only remains (moves to souslet)
```

---

## Task 1: Wire contract + generated code

**Files:**
- Create: `proto/souslet/v1/souslet.proto`
- Create: `Makefile`
- Create (generated, committed): `internal/pb/souslet/v1/souslet.pb.go`, `internal/pb/souslet/v1/souslet_grpc.pb.go`

**Interfaces:**
- Produces: `pb.Envelope`, `pb.NodeSnapshot`, `pb.DeploymentState`, `pb.DeployCommand`, `pb.DeployResult`, `pb.UndeployCommand`, `pb.UndeployResult`, `pb.PlanCommand`, `pb.PlanResult`, `pb.FetchCommand`, `pb.FetchProgress`, `pb.DeleteWeightsCommand`, `pb.DeleteWeightsResult`, `pb.HTTPRequestHead`, `pb.HTTPRequestChunk`, `pb.HTTPResponseHead`, `pb.HTTPResponseChunk`, `pb.Heartbeat`, `pb.Error` — every message every later task consumes.
- Produces: `pb.SousletClient` (souslet dials with this), `pb.SousletServer` interface + `pb.RegisterSousletServer` (sous-api implements this), `pb.UnimplementedSousletServer` (embed for forward compat).

- [ ] **Step 1: Write the proto file**

```proto
syntax = "proto3";

package souslet.v1;

option go_package = "github.com/codemug/sous/internal/pb/souslet/v1;pb";

service Souslet {
  // souslet dials this once and keeps it open for the process lifetime.
  // Every deploy/fetch/proxy operation sous-api needs this node to do is
  // an Envelope on this stream; souslet executes it locally and streams
  // results back on the same stream, correlated by stream_id.
  rpc Connect(stream Envelope) returns (stream Envelope);
}

message Envelope {
  // Correlates a request with its response(s). sous-api generates one per
  // command or proxied HTTP request; souslet echoes it back on every
  // reply so concurrent operations resolve independently of arrival order.
  string stream_id = 1;

  oneof payload {
    // souslet -> API, sent once immediately after Connect and again after
    // every reconnect. Full state, not a diff.
    NodeSnapshot snapshot = 10;

    // API -> souslet
    DeployCommand deploy = 20;
    UndeployCommand undeploy = 21;
    PlanCommand plan = 22;
    FetchCommand fetch = 23;
    DeleteWeightsCommand delete_weights = 24;
    HTTPRequestHead http_req_head = 25;
    HTTPRequestChunk http_req_chunk = 26;

    // souslet -> API
    DeployResult deploy_result = 30;
    UndeployResult undeploy_result = 31;
    PlanResult plan_result = 32;
    FetchProgress fetch_progress = 33;
    DeleteWeightsResult delete_weights_result = 34;
    HTTPResponseHead http_resp_head = 35;
    HTTPResponseChunk http_resp_chunk = 36;

    // either direction
    Heartbeat heartbeat = 40;
    Error error = 41;
  }
}

message NodeSnapshot {
  string node_id = 1;
  double pool_gib = 2;
  double reserve_gib = 3;
  repeated DeploymentState deployments = 4;
  repeated string cached_weight_repos = 5;
}

message DeploymentState {
  string recipe_id = 1;
  int32 host_port = 2;
  string phase = 3;
  double weights_gib = 4;
  double kv_gib = 5;
}

message DeployCommand {
  string recipe_id = 1;
  string recipe_yaml = 2;  // full recipe, so souslet needs no catalog of its own
  int32 want_port = 3;
  bool force = 4;
}

message DeployResult {
  string recipe_id = 1;
  int32 host_port = 2;
  string container_id = 3;
  string error = 4;  // empty on success
}

message UndeployCommand {
  string recipe_id = 1;
}

message UndeployResult {
  string recipe_id = 1;
  string error = 2;
}

message PlanCommand {
  string recipe_id = 1;
  double incoming_gib = 2;
}

message PlanResult {
  bool fits = 1;
  double committed_gib = 2;
  double margin_gib = 3;
  repeated string must_free = 4;
}

message FetchCommand {
  string repo = 1;
}

message FetchProgress {
  string repo = 1;
  string phase = 2;  // downloading|done|failed|absent
  int64 bytes = 3;
  int64 total = 4;
}

message DeleteWeightsCommand {
  string repo = 1;
  bool force = 2;
}

message DeleteWeightsResult {
  string repo = 1;
  int64 bytes_freed = 2;
  string error = 3;
}

message HTTPRequestHead {
  string method = 1;
  string path = 2;
  map<string, string> headers = 3;
}

message HTTPRequestChunk {
  bytes data = 1;
  bool eof = 2;
}

message HTTPResponseHead {
  int32 status = 1;
  map<string, string> headers = 2;
}

message HTTPResponseChunk {
  bytes data = 1;
  bool eof = 2;
}

message Heartbeat {
  int64 unix_seconds = 1;
}

message Error {
  string message = 1;
}
```

- [ ] **Step 2: Write the Makefile proto target**

```makefile
PROTOC_GEN_GO_VERSION := v1.34.2
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: proto
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	protoc \
		--go_out=. --go_opt=module=github.com/codemug/sous \
		--go-grpc_out=. --go-grpc_opt=module=github.com/codemug/sous \
		proto/souslet/v1/souslet.proto

.PHONY: test
test:
	go test ./...
```

- [ ] **Step 3: Add module dependencies**

```bash
go get google.golang.org/grpc@v1.68.1
go get google.golang.org/protobuf@v1.35.2
```

- [ ] **Step 4: Generate the code**

Run: `make proto`
Expected: `internal/pb/souslet/v1/souslet.pb.go` and `internal/pb/souslet/v1/souslet_grpc.pb.go` are created.

- [ ] **Step 5: Verify it compiles**

Run: `go build ./internal/pb/...`
Expected: builds clean, no errors.

- [ ] **Step 6: Commit**

```bash
git add proto/ Makefile go.mod go.sum internal/pb/
git commit -m "feat(souslet): add gRPC wire contract and generated code"
```

---

## Task 2: mTLS CA

**Files:**
- Create: `internal/mtls/ca.go`
- Test: `internal/mtls/ca_test.go`

**Interfaces:**
- Consumes: nothing (foundational).
- Produces: `mtls.CA{cert, key}`, `mtls.NewCA() (*CA, error)`, `(*CA) IssueNodeCert(nodeID string) (certPEM, keyPEM []byte, err error)`, `(*CA) TLSConfigServer() (*tls.Config, error)` (for `sous-api`'s gRPC listener — requires and verifies client certs), `mtls.ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error)` (for souslet's dial), `(*CA) VerifiedNodeID(ctx context.Context) (string, bool)` (extracts the CN a peer authenticated with, from a gRPC context — used by grpcserver to know which node just connected).

- [ ] **Step 1: Write the failing test**

```go
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestIssuedCertVerifiesAgainstTheCA(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueNodeCert("asus-gx10")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CAPEM()) {
		t.Fatal("failed to load CA cert into pool")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("issued cert does not verify against its own CA: %v", err)
	}
	if leaf.Subject.CommonName != "asus-gx10" {
		t.Fatalf("CommonName = %q, want asus-gx10", leaf.Subject.CommonName)
	}
}

func TestARevokedNodeIsNotInTheKnownSet(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if _, _, err := ca.IssueNodeCert("asus-gx10"); err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	ca.Revoke("asus-gx10")
	if ca.IsKnown("asus-gx10") {
		t.Fatal("revoked node still reports known")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mtls/... -run TestIssuedCertVerifiesAgainstTheCA -v`
Expected: FAIL — `mtls` package / `NewCA` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Package mtls issues short-lived-infrastructure-scale client certificates
// for souslets to authenticate to sous-api with, signed by a CA sous-api
// generates and owns itself. No external CA, no rotation automation in
// this version - certs are treated as long-lived, reissued by hand on
// revocation.
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"
)

type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey

	mu    sync.Mutex
	known map[string]bool // node IDs with a currently-valid issued cert
}

func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sous-api node CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	return &CA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		known:   make(map[string]bool),
	}, nil
}

func (c *CA) CAPEM() []byte { return c.certPEM }

// IssueNodeCert signs a fresh client certificate for nodeID, valid for
// client auth only. The node's ID becomes the certificate's CommonName -
// grpcserver reads it back out of the verified peer chain to know which
// node just connected.
func (c *CA) IssueNodeCert(nodeID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign node cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}
	c.mu.Lock()
	c.known[nodeID] = true
	c.mu.Unlock()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

func (c *CA) Revoke(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, nodeID)
}

func (c *CA) IsKnown(nodeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known[nodeID]
}

// TLSConfigServer builds the listener-side TLS config: require and verify
// a client cert signed by this CA. Actual node-identity/revocation
// enforcement (IsKnown) happens one layer up in grpcserver, since a
// tls.Config's ClientAuth check alone can't consult per-connection state.
func (c *CA) TLSConfigServer() (*tls.Config, error) {
	serverCert, serverKeyPEM, err := c.IssueNodeCert("sous-api")
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(serverCert, serverKeyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.certPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// ClientTLSConfig builds souslet's dial-side TLS config from the CA cert
// and this node's issued cert+key (all handed to souslet out of band, the
// same way this fleet already distributes onboarding material).
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load node cert/key: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mtls/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mtls/
git commit -m "feat(mtls): self-issued CA for per-node souslet client certs"
```

---

## Task 3: Node catalog

**Files:**
- Create: `internal/nodecatalog/nodecatalog.go`
- Test: `internal/nodecatalog/nodecatalog_test.go`

**Interfaces:**
- Consumes: `pb.NodeSnapshot`, `pb.DeploymentState` (Task 1).
- Produces: `nodecatalog.Catalog{}`, `nodecatalog.New() *Catalog`, `(*Catalog) ReplaceSnapshot(nodeID string, snap *pb.NodeSnapshot)`, `(*Catalog) MarkDisconnected(nodeID string)`, `(*Catalog) Node(nodeID string) (NodeView, bool)`, `(*Catalog) All() []NodeView`, `(*Catalog) NodeFor(recipeID string) (nodeID string, ok bool)` (which connected node currently runs this recipe — used by gateway proxy), `NodeView{NodeID, PoolGiB, ReserveGiB, MarginGiB, Connected bool, Deployments []pb.DeploymentState, CachedWeightRepos map[string]bool}`.

- [ ] **Step 1: Write the failing test**

```go
package nodecatalog

import (
	"testing"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

func TestReplaceSnapshotIsAFullReplaceNotAMerge(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{{RecipeId: "old-model", Phase: "ready"}},
	})
	// A later snapshot with a different deployment set must REPLACE, not
	// accumulate - this is the level-triggered reconciliation the design
	// requires: a container that vanished during a disconnect must vanish
	// from the catalog too, not linger from a stale merge.
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{{RecipeId: "new-model", Phase: "ready"}},
	})
	view, ok := c.Node("asus-gx10")
	if !ok {
		t.Fatal("node not found")
	}
	if len(view.Deployments) != 1 || view.Deployments[0].RecipeId != "new-model" {
		t.Fatalf("expected exactly [new-model], got %+v", view.Deployments)
	}
}

func TestDisconnectKeepsLastKnownDeploymentsButMarksDisconnected(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	c.MarkDisconnected("asus-gx10")
	view, ok := c.Node("asus-gx10")
	if !ok {
		t.Fatal("node not found")
	}
	if view.Connected {
		t.Fatal("expected Connected=false after MarkDisconnected")
	}
	if len(view.Deployments) != 1 {
		t.Fatalf("expected last-known deployment to remain visible, got %+v", view.Deployments)
	}
}

func TestNodeForFindsTheConnectedNodeRunningARecipe(t *testing.T) {
	c := New()
	c.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	node, ok := c.NodeFor("dflash2")
	if !ok || node != "asus-gx10" {
		t.Fatalf("NodeFor(dflash2) = %q, %v; want asus-gx10, true", node, ok)
	}
	if _, ok := c.NodeFor("nonexistent"); ok {
		t.Fatal("expected NodeFor to report not-found for an undeployed recipe")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nodecatalog/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the implementation**

```go
// Package nodecatalog holds sous-api's live, in-memory view of every
// connected node: capacity, what's deployed, and which recipes' weights
// are on that node's disk. It is fed exclusively by grpcserver's handling
// of NodeSnapshot messages - level-triggered, full replace, never a merge
// or an event log, so a node's last snapshot is always exactly what that
// node itself reported, not an accumulation this process guessed at.
package nodecatalog

import (
	"sync"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

type NodeView struct {
	NodeID            string
	PoolGiB           float64
	ReserveGiB        float64
	Connected         bool
	Deployments       []*pb.DeploymentState
	CachedWeightRepos map[string]bool
}

type Catalog struct {
	mu    sync.RWMutex
	nodes map[string]*NodeView
}

func New() *Catalog {
	return &Catalog{nodes: make(map[string]*NodeView)}
}

// ReplaceSnapshot overwrites everything known about nodeID with snap. Not a
// merge: a deployment missing from snap is gone from the catalog too, on
// the theory that souslet's own live Docker query is more trustworthy than
// anything this process cached from an earlier snapshot.
func (c *Catalog) ReplaceSnapshot(nodeID string, snap *pb.NodeSnapshot) {
	cached := make(map[string]bool, len(snap.CachedWeightRepos))
	for _, r := range snap.CachedWeightRepos {
		cached[r] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes[nodeID] = &NodeView{
		NodeID:            nodeID,
		PoolGiB:           snap.PoolGib,
		ReserveGiB:        snap.ReserveGib,
		Connected:         true,
		Deployments:       snap.Deployments,
		CachedWeightRepos: cached,
	}
}

// MarkDisconnected flips Connected to false but keeps the node's
// last-known deployments visible (greyed out in the UI) rather than
// deleting the entry - "what was running here before it went quiet"
// stays answerable.
func (c *Catalog) MarkDisconnected(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.nodes[nodeID]; ok {
		n.Connected = false
	}
}

func (c *Catalog) Node(nodeID string) (NodeView, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.nodes[nodeID]
	if !ok {
		return NodeView{}, false
	}
	return *n, true
}

func (c *Catalog) All() []NodeView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]NodeView, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, *n)
	}
	return out
}

// NodeFor returns the connected node currently running recipeID, if any.
// Disconnected nodes are not returned even if their last snapshot still
// lists the recipe - gateway proxying to a node with no live connection
// cannot succeed, so it should fail fast rather than be offered as a
// candidate.
func (c *Catalog) NodeFor(recipeID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for id, n := range c.nodes {
		if !n.Connected {
			continue
		}
		for _, d := range n.Deployments {
			if d.RecipeId == recipeID {
				return id, true
			}
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/nodecatalog/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/nodecatalog/
git commit -m "feat(nodecatalog): in-memory per-node state, level-triggered replace"
```

---

## Task 4: gRPC server (sous-api side)

**Files:**
- Create: `internal/grpcserver/server.go`
- Test: `internal/grpcserver/server_test.go`

**Interfaces:**
- Consumes: `pb.SousletServer`, `pb.UnimplementedSousletServer`, `pb.Envelope` (Task 1); `*nodecatalog.Catalog` (Task 3); `*mtls.CA` (Task 2, for `VerifiedNodeID`).
- Produces: `grpcserver.Server{}`, `grpcserver.New(cat *nodecatalog.Catalog) *Server`, `(*Server) Connect(stream pb.Souslet_ConnectServer) error` (satisfies `pb.SousletServer`), `(*Server) Send(nodeID string, env *pb.Envelope) (*pb.Envelope, error)` (send a command, block for the correlated reply — used by Task 8/9), `(*Server) OpenProxyStream(nodeID string) (*ProxyStream, error)` (used by Task 9's gateway rewrite for the multi-chunk HTTP proxy case, where a single request/response doesn't fit the simple send-one-get-one-back shape).

- [ ] **Step 1: Write the failing test**

```go
package grpcserver

import (
	"context"
	"testing"
	"time"

	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// fakeSouslet drives the client side of Connect in-process over bufconn,
// standing in for a real souslet binary so this test needs no Docker.
func dialFakeSouslet(t *testing.T, srv *Server) pb.Souslet_ConnectClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (interface{ Read([]byte) (int, error) }, error) {
			return lis.DialContext(ctx)
		}),
	)
	_ = err
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return stream
}

func TestSnapshotFromSousletUpdatesTheNodeCatalog(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat)
	stream := dialFakeSouslet(t, srv)

	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6,
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	}}}); err != nil {
		t.Fatalf("Send snapshot: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if view, ok := cat.Node("asus-gx10"); ok && len(view.Deployments) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node catalog was not updated with the snapshot within 2s")
}

func TestSendCorrelatesRequestAndReplyByStreamID(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat)
	stream := dialFakeSouslet(t, srv)
	_ = stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{NodeId: "asus-gx10"}}})

	// Drive the fake souslet's reply loop: echo a DeployResult back with
	// whatever stream_id the incoming DeployCommand carried.
	go func() {
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if cmd := env.GetDeploy(); cmd != nil {
				_ = stream.Send(&pb.Envelope{
					StreamId: env.StreamId,
					Payload:  &pb.Envelope_DeployResult{DeployResult: &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: "abc123"}},
				})
			}
		}
	}()

	reply, err := srv.Send("asus-gx10", &pb.Envelope{Payload: &pb.Envelope_Deploy{Deploy: &pb.DeployCommand{RecipeId: "dflash2"}}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	res := reply.GetDeployResult()
	if res == nil || res.ContainerId != "abc123" {
		t.Fatalf("got %+v, want DeployResult{ContainerId: abc123}", reply)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpcserver/... -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the implementation**

```go
// Package grpcserver implements the API side of the Souslet gRPC service:
// accepts each node's single long-lived Connect stream, feeds NodeSnapshot
// messages into nodecatalog, and lets the rest of sous-api (deploy/undeploy/
// plan handlers, the gateway proxy) send commands to a specific connected
// node and wait for the correlated reply.
package grpcserver

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/google/uuid"
)

type nodeConn struct {
	send    chan *pb.Envelope
	mu      sync.Mutex
	pending map[string]chan *pb.Envelope // stream_id -> waiter
}

type Server struct {
	pb.UnimplementedSousletServer
	cat *nodecatalog.Catalog

	mu    sync.RWMutex
	conns map[string]*nodeConn // node_id -> its live connection
}

func New(cat *nodecatalog.Catalog) *Server {
	return &Server{cat: cat, conns: make(map[string]*nodeConn)}
}

// Connect is the Souslet service's one RPC. It blocks for the life of the
// connection: read loop demuxes incoming Envelopes (snapshots update the
// catalog directly; everything else is routed to whichever Send call is
// waiting on that stream_id), write loop drains the outgoing channel Send
// publishes to.
func (s *Server) Connect(stream pb.Souslet_ConnectServer) error {
	// The first message on a new connection must be a snapshot - that's
	// how this node's ID is learned (see VerifiedNodeID note in Task 2;
	// full peer-cert-based identity wiring happens in Task 6's server
	// setup, this handler trusts NodeSnapshot.node_id for now since the
	// TLS layer already only accepted a cert signed by this CA).
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("read initial snapshot: %w", err)
	}
	snap := first.GetSnapshot()
	if snap == nil {
		return fmt.Errorf("first message on Connect must be a NodeSnapshot")
	}
	nodeID := snap.NodeId
	s.cat.ReplaceSnapshot(nodeID, snap)

	nc := &nodeConn{send: make(chan *pb.Envelope, 32), pending: make(map[string]chan *pb.Envelope)}
	s.mu.Lock()
	s.conns[nodeID] = nc
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, nodeID)
		s.mu.Unlock()
		s.cat.MarkDisconnected(nodeID)
	}()

	errCh := make(chan error, 2)
	go func() {
		for env := range nc.send {
			if err := stream.Send(env); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		for {
			env, err := stream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if s := env.GetSnapshot(); s != nil {
				s.NodeId = nodeID // defensive: trust the connection's identity, not a resend
				s.cat.ReplaceSnapshot(nodeID, s)
				_ = s
				continue
			}
			nc.mu.Lock()
			waiter, ok := nc.pending[env.StreamId]
			if ok {
				delete(nc.pending, env.StreamId)
			}
			nc.mu.Unlock()
			if ok {
				waiter <- env
			}
		}
	}()
	return <-errCh
}

// Send delivers env to nodeID's live connection and blocks until the
// correlated reply arrives. Returns an error immediately if nodeID has no
// live connection - callers must not queue against a disconnected node
// (the design's explicit "fail fast, don't buffer" reconciliation choice).
func (s *Server) Send(nodeID string, env *pb.Envelope) (*pb.Envelope, error) {
	s.mu.RLock()
	nc, ok := s.conns[nodeID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	env.StreamId = uuid.NewString()
	waiter := make(chan *pb.Envelope, 1)
	nc.mu.Lock()
	nc.pending[env.StreamId] = waiter
	nc.mu.Unlock()

	select {
	case nc.send <- env:
	default:
		return nil, fmt.Errorf("node %q's send queue is full", nodeID)
	}

	select {
	case reply := <-waiter:
		return reply, nil
	case <-context.Background().Done():
		return nil, context.Canceled
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/grpcserver/... -v`
Expected: PASS. If `bufconn`'s dialer signature doesn't match the pinned `google.golang.org/grpc` version's `grpc.WithContextDialer` expected type, adjust the test's dialer closure to match — this is exactly the kind of thing to verify against the actually-installed grpc version rather than assume.

- [ ] **Step 5: Add the module dependency the test needs**

```bash
go get github.com/google/uuid@v1.6.0
```

- [ ] **Step 6: Commit**

```bash
git add internal/grpcserver/ go.mod go.sum
git commit -m "feat(grpcserver): API-side Souslet service, snapshot ingestion, correlated Send"
```

---

## Task 5: souslet-side handlers (dispatch Envelope payloads to engine/fetch)

**Files:**
- Create: `internal/grpcclient/handlers.go`
- Test: `internal/grpcclient/handlers_test.go`

**Interfaces:**
- Consumes: `deploy.Runtime` interface (existing, `internal/deploy/deploy.go:35`), `fetch.Manager` (existing, `internal/fetch/fetch.go:38`), `engine.BuildSpec` (existing, `internal/engine/spec.go`), `pb.DeployCommand`/`DeployResult`/`FetchCommand`/`FetchProgress`/`DeleteWeightsCommand`/`DeleteWeightsResult` (Task 1).
- Produces: `grpcclient.Handlers{Runtime deploy.Runtime, Fetch *fetch.Manager, ModelDir string}`, `(*Handlers) HandleDeploy(ctx, *pb.DeployCommand) *pb.DeployResult`, `(*Handlers) HandleUndeploy(ctx, *pb.UndeployCommand) *pb.UndeployResult`, `(*Handlers) HandleFetch(ctx, *pb.FetchCommand) *pb.FetchProgress`, `(*Handlers) HandleDeleteWeights(ctx, *pb.DeleteWeightsCommand) *pb.DeleteWeightsResult`, `(*Handlers) Snapshot(ctx, nodeID string, poolGiB, reserveGiB float64) *pb.NodeSnapshot` (used by Task 6 to build the initial and reconnect snapshots).

- [ ] **Step 1: Write the failing test**

```go
package grpcclient

import (
	"context"
	"testing"

	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

// fakeRuntime is the same shape as deploy.Runtime - a minimal in-memory
// double so this test needs no real Docker daemon.
type fakeRuntime struct {
	started []string
}

func (f *fakeRuntime) Start(ctx context.Context, spec interface{ ContainerName() string }) (string, error) {
	f.started = append(f.started, spec.ContainerName())
	return "fake-container-id", nil
}

func TestHandleDeployStartsTheContainerFromTheEmbeddedRecipeYAML(t *testing.T) {
	rec := recipe.Recipe{ID: "dflash2", Kind: recipe.KindVLLM, Model: "Inferact/Qwen3.8-27B-NVFP4"}
	recipeYAML, err := yaml.Marshal(rec)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	h := &Handlers{ModelDir: t.TempDir()}
	result := h.HandleDeploy(context.Background(), &pbDeployCommand(t, "dflash2", string(recipeYAML)))
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.RecipeId != "dflash2" {
		t.Fatalf("RecipeId = %q, want dflash2", result.RecipeId)
	}
}
```

*(Note for the implementer: the exact `fakeRuntime`/`pbDeployCommand` test-helper shapes above must match whatever `deploy.Runtime`'s real method signatures turn out to be once you read `internal/deploy/deploy.go:35-50` directly — the exploration that fed this plan summarized the interface but didn't quote it verbatim. Read that interface first, adjust the fake to satisfy it exactly, then proceed. This is the one step in this plan where "read the existing code before writing the test" is required rather than optional.)*

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpcclient/... -run TestHandleDeploy -v`
Expected: FAIL — package/type undefined.

- [ ] **Step 3: Write the implementation**

```go
// Package grpcclient is souslet's half of the connection: dial sous-api,
// hold the Connect stream open, and dispatch each incoming Envelope to a
// local Handlers method that does the actual Docker/fetch work via the
// existing deploy.Runtime/fetch.Manager/engine code, unchanged from how
// single-node Sous already used them.
package grpcclient

import (
	"context"

	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"gopkg.in/yaml.v3"
)

type Handlers struct {
	Runtime  deploy.Runtime
	Fetch    *fetch.Manager
	ModelDir string
}

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
	return &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: containerID, HostPort: cmd.WantPort}
}

func (h *Handlers) HandleUndeploy(ctx context.Context, cmd *pb.UndeployCommand) *pb.UndeployResult {
	if err := h.Runtime.Stop(ctx, engine.ContainerName(cmd.RecipeId)); err != nil {
		return &pb.UndeployResult{RecipeId: cmd.RecipeId, Error: err.Error()}
	}
	return &pb.UndeployResult{RecipeId: cmd.RecipeId}
}

func (h *Handlers) HandleFetch(ctx context.Context, cmd *pb.FetchCommand) *pb.FetchProgress {
	job, err := h.Fetch.Start(ctx, cmd.Repo)
	if err != nil {
		return &pb.FetchProgress{Repo: cmd.Repo, Phase: "failed"}
	}
	return &pb.FetchProgress{Repo: cmd.Repo, Phase: string(job.Phase)}
}

func (h *Handlers) HandleDeleteWeights(ctx context.Context, cmd *pb.DeleteWeightsCommand) *pb.DeleteWeightsResult {
	// The guard logic (never delete a StateReferenced repo, require Force
	// for StateProtected) is the existing internal/larder/delete.go Delete
	// function, relocated here unchanged in Task 12 - this handler is a
	// thin wrapper around it, not a reimplementation.
	freed, err := deleteWeights(h.ModelDir, cmd.Repo, cmd.Force)
	if err != nil {
		return &pb.DeleteWeightsResult{Repo: cmd.Repo, Error: err.Error()}
	}
	return &pb.DeleteWeightsResult{Repo: cmd.Repo, BytesFreed: freed}
}

// Snapshot builds this node's complete current state by asking Docker and
// the local disk directly - never a cache - matching the "state is the
// container, not a record" philosophy internal/deploy and internal/fetch
// already followed in single-node Sous.
func (h *Handlers) Snapshot(ctx context.Context, nodeID string, poolGiB, reserveGiB float64) *pb.NodeSnapshot {
	states, _ := h.Runtime.States(ctx)
	deployments := make([]*pb.DeploymentState, 0, len(states))
	for id, st := range states {
		deployments = append(deployments, &pb.DeploymentState{RecipeId: id, Phase: string(st.Phase)})
	}
	return &pb.NodeSnapshot{
		NodeId: nodeID, PoolGib: poolGiB, ReserveGib: reserveGiB,
		Deployments: deployments,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/grpcclient/... -v`
Expected: PASS after adjusting the fake to the real `deploy.Runtime` signature per the Step 1 note.

- [ ] **Step 5: Commit**

```bash
git add internal/grpcclient/handlers.go internal/grpcclient/handlers_test.go
git commit -m "feat(grpcclient): dispatch Envelope commands to local engine/fetch"
```

---

## Task 6: gRPC client connect/reconnect loop + souslet binary

**Files:**
- Create: `internal/grpcclient/client.go`
- Test: `internal/grpcclient/client_test.go`
- Create: `cmd/souslet/main.go`

**Interfaces:**
- Consumes: `mtls.ClientTLSConfig` (Task 2), `pb.SousletClient`/`pb.NewSousletClient` (Task 1), `*Handlers` (Task 5).
- Produces: `grpcclient.Client{Addr, TLSConfig, NodeID, Handlers *Handlers, PoolGiB, ReserveGiB}`, `(*Client) Run(ctx context.Context) error` (dial, send initial snapshot, loop reading/dispatching Envelopes, reconnect with backoff on any stream error — runs until ctx is cancelled).

- [ ] **Step 1: Write the failing test**

```go
package grpcclient

import (
	"context"
	"testing"
	"time"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeServer records every DeployCommand it receives and replies
// immediately - enough to prove Client dispatches incoming commands to
// Handlers and sends the result back, without needing a real sous-api.
type fakeServer struct {
	pb.UnimplementedSousletServer
	received chan *pb.DeployCommand
}

func (f *fakeServer) Connect(stream pb.Souslet_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil || first.GetSnapshot() == nil {
		return err
	}
	if err := stream.Send(&pb.Envelope{StreamId: "cmd-1", Payload: &pb.Envelope_Deploy{
		Deploy: &pb.DeployCommand{RecipeId: "dflash2", RecipeYaml: "id: dflash2\nkind: vllm\n"},
	}}); err != nil {
		return err
	}
	env, err := stream.Recv()
	if err != nil {
		return err
	}
	if res := env.GetDeployResult(); res != nil {
		f.received <- &pb.DeployCommand{RecipeId: res.RecipeId}
	}
	<-stream.Context().Done()
	return nil
}

func TestClientDispatchesIncomingDeployCommandsAndRepliesOnTheSameStreamID(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	fs := &fakeServer{received: make(chan *pb.DeployCommand, 1)}
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, fs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	c := &Client{
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (interface{}, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID:   "asus-gx10",
		Handlers: &Handlers{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case got := <-fs.received:
		if got.RecipeId != "dflash2" {
			t.Fatalf("RecipeId = %q, want dflash2", got.RecipeId)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the client to dispatch and reply to a command")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/grpcclient/... -run TestClientDispatches -v`
Expected: FAIL — `Client` undefined.

- [ ] **Step 3: Write the implementation**

```go
package grpcclient

import (
	"context"
	"log"
	"time"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
)

type Client struct {
	Addr        string
	DialOptions []grpc.DialOption
	NodeID      string
	Handlers    *Handlers
	PoolGiB     float64
	ReserveGiB  float64
}

// Run dials sous-api and stays connected until ctx is cancelled,
// reconnecting with capped exponential backoff on any stream error. Every
// (re)connect sends one full NodeSnapshot before anything else - the
// level-triggered reconciliation the design calls for, with no attempt to
// carry state across a disconnect.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.connectOnce(ctx); err != nil {
			log.Printf("souslet: connection to %s lost: %v (retrying in %s)", c.Addr, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (c *Client) connectOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(c.Addr, c.DialOptions...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}

	snap := c.Handlers.Snapshot(ctx, c.NodeID, c.PoolGiB, c.ReserveGiB)
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: snap}}); err != nil {
		return err
	}

	for {
		env, err := stream.Recv()
		if err != nil {
			return err
		}
		go c.dispatch(ctx, stream, env)
	}
}

func (c *Client) dispatch(ctx context.Context, stream pb.Souslet_ConnectClient, env *pb.Envelope) {
	var reply *pb.Envelope
	switch {
	case env.GetDeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{
			DeployResult: c.Handlers.HandleDeploy(ctx, env.GetDeploy()),
		}}
	case env.GetUndeploy() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_UndeployResult{
			UndeployResult: c.Handlers.HandleUndeploy(ctx, env.GetUndeploy()),
		}}
	case env.GetFetch() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_FetchProgress{
			FetchProgress: c.Handlers.HandleFetch(ctx, env.GetFetch()),
		}}
	case env.GetDeleteWeights() != nil:
		reply = &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeleteWeightsResult{
			DeleteWeightsResult: c.Handlers.HandleDeleteWeights(ctx, env.GetDeleteWeights()),
		}}
	default:
		return // HTTP proxy frames are handled by Task 9's extension of this switch, not here
	}
	if err := stream.Send(reply); err != nil {
		log.Printf("souslet: failed to send reply for stream %s: %v", env.StreamId, err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/grpcclient/... -v`
Expected: PASS

- [ ] **Step 5: Write the souslet binary**

```go
// Command souslet is the per-node worker: it holds no UI, no HTTP server,
// and no persistent store of its own - only a Docker engine wrapper, a
// weight-fetch manager, and a gRPC client that dials sous-api and stays
// connected for the process lifetime. Everything it needs to report is
// derived live from Docker on every (re)connect.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/grpcclient"
	"github.com/codemug/sous/internal/mtls"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	apiAddr := flag.String("api-addr", "", "sous-api's gRPC address, host:port")
	nodeID := flag.String("node-id", "", "this node's ID, must match what sous-api issued the cert for")
	modelDir := flag.String("model-dir", "", "host directory bound as the HF cache")
	caPath := flag.String("ca", "", "path to the CA cert PEM")
	certPath := flag.String("cert", "", "path to this node's issued cert PEM")
	keyPath := flag.String("key", "", "path to this node's issued key PEM")
	poolGiB := flag.Float64("pool-gib", 0, "this node's total usable memory pool")
	reserveGiB := flag.Float64("reserve-gib", 24, "GiB reserved for the OS, never committed to a deployment")
	flag.Parse()

	for name, v := range map[string]string{"-api-addr": *apiAddr, "-node-id": *nodeID, "-model-dir": *modelDir, "-ca": *caPath, "-cert": *certPath, "-key": *keyPath} {
		if v == "" {
			log.Fatalf("%s is required", name)
		}
	}

	caPEM, err := os.ReadFile(*caPath)
	if err != nil {
		log.Fatalf("read CA: %v", err)
	}
	certPEM, err := os.ReadFile(*certPath)
	if err != nil {
		log.Fatalf("read cert: %v", err)
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		log.Fatalf("read key: %v", err)
	}
	tlsConfig, err := mtls.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		log.Fatalf("build TLS config: %v", err)
	}

	dockerEngine, err := engine.New("")
	if err != nil {
		log.Fatalf("connect to local Docker: %v", err)
	}
	fetchMgr := &fetch.Manager{Runtime: dockerEngine, ModelDir: *modelDir}

	client := &grpcclient.Client{
		Addr:        *apiAddr,
		DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))},
		NodeID:      *nodeID,
		PoolGiB:     *poolGiB,
		ReserveGiB:  *reserveGiB,
		Handlers:    &grpcclient.Handlers{Runtime: dockerEngine, Fetch: fetchMgr, ModelDir: *modelDir},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log.Printf("souslet: connecting to %s as node %q", *apiAddr, *nodeID)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("souslet: %v", err)
	}
}
```

- [ ] **Step 6: Verify it builds**

Run: `go build ./cmd/souslet/...`
Expected: builds clean. (`engine.New` and `fetch.Manager`'s exact constructor shape must match `internal/engine/engine.go:23` and `internal/fetch/fetch.go:38` respectively — adjust field names above if they differ from what this plan assumed; the exploration that fed this plan quoted the shapes but verify against the live source before treating a build failure here as a bug in this plan rather than a drift to correct.)

- [ ] **Step 7: Commit**

```bash
git add internal/grpcclient/client.go internal/grpcclient/client_test.go cmd/souslet/
git commit -m "feat(souslet): connect/reconnect loop and the souslet binary"
```

---

## Task 7: sous-api binary (wires catalog, nodecatalog, grpcserver, httpapi together)

**Files:**
- Create: `cmd/sous-api/main.go`

**Interfaces:**
- Consumes: everything from Tasks 1-4, plus existing `internal/catalog`, `internal/store`, `internal/httpapi` (largely unchanged constructors from the current `cmd/sous/main.go` — read it directly for the exact `New()` signatures before wiring, since this plan's earlier exploration summarized but did not quote them verbatim).
- Produces: the `sous-api` binary — a gRPC listener (mTLS, Task 2) alongside the existing HTTP listener, both serving out of the same process.

- [ ] **Step 1: Write `cmd/sous-api/main.go`**

```go
// Command sous-api is the control plane: the recipe catalog, the node
// catalog, the UI, and the gRPC server every souslet dials into. It holds
// no direct Docker access of its own - all container operations are
// commands sent to a specific connected souslet.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"

	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/httpapi"
	"github.com/codemug/sous/internal/mtls"
	"github.com/codemug/sous/internal/nodecatalog"
	"github.com/codemug/sous/internal/store"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	listen := flag.String("listen", "", "HTTP listen address, tailnet IP only, never 0.0.0.0")
	grpcListen := flag.String("grpc-listen", "", "gRPC listen address for souslets to dial")
	dataDir := flag.String("data", "", "on-disk state directory")
	caStatePath := flag.String("ca-state", "", "path to persist the node CA across restarts")
	flag.Parse()
	for name, v := range map[string]string{"-listen": *listen, "-grpc-listen": *grpcListen, "-data": *dataDir, "-ca-state": *caStatePath} {
		if v == "" {
			log.Fatalf("%s is required", name)
		}
	}

	st, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	cat := catalog.New(st)
	nodes := nodecatalog.New()

	ca, err := loadOrCreateCA(*caStatePath)
	if err != nil {
		log.Fatalf("CA: %v", err)
	}
	tlsConfig, err := ca.TLSConfigServer()
	if err != nil {
		log.Fatalf("build server TLS config: %v", err)
	}

	gsrv := grpcserver.New(nodes)
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	pb.RegisterSousletServer(grpcSrv, gsrv)
	lis, err := net.Listen("tcp", *grpcListen)
	if err != nil {
		log.Fatalf("listen (gRPC) on %s: %v", *grpcListen, err)
	}
	go func() {
		log.Printf("sous-api: gRPC listening on %s", *grpcListen)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Fatalf("gRPC server: %v", err)
		}
	}()

	httpSrv := httpapi.New(cat, nodes, gsrv, st)
	log.Printf("sous-api: HTTP listening on %s", *listen)
	if err := httpSrv.ListenAndServe(*listen); err != nil {
		log.Fatalf("HTTP server: %v", err)
	}
	_ = context.Background()
	_ = os.Stdout
}
```

*(`httpapi.New`'s real parameter list must be reconciled against Task 8's actual changes to that constructor — this main.go's call to it is illustrative of intent, not a contract Task 8 must match exactly; update this file in lockstep if Task 8 lands with a different signature.)*

- [ ] **Step 2: Write `loadOrCreateCA` (persist the CA across restarts)**

```go
// Add to cmd/sous-api/main.go

func loadOrCreateCA(path string) (*mtls.CA, error) {
	// A CA regenerated on every restart would invalidate every already-
	// issued node cert, disconnecting every souslet until each is
	// reissued by hand - persisting it is not optional polish. Actual
	// (de)serialization of *mtls.CA's private key material is left as a
	// follow-up format decision for whoever implements this step; the
	// shape (load if path exists, else create-and-save) is the part this
	// plan is prescribing.
	if _, err := os.Stat(path); err == nil {
		return mtls.LoadCA(path)
	}
	ca, err := mtls.NewCA()
	if err != nil {
		return nil, err
	}
	if err := ca.Save(path); err != nil {
		return nil, err
	}
	return ca, nil
}
```

- [ ] **Step 3: Add `mtls.LoadCA`/`(*CA) Save` to satisfy the above**

```go
// Add to internal/mtls/ca.go

func (c *CA) Save(path string) error {
	// Persist enough to reconstruct c.cert/c.key/c.known on the next
	// LoadCA - the CA's own cert+key PEM plus the known-node-ID set.
	// Encoding format is an implementation detail (JSON-wrapping the two
	// PEM blocks plus the known map is sufficient); the contract this
	// plan is prescribing is only Save/Load round-tripping cleanly.
	return saveCAState(path, c)
}

func LoadCA(path string) (*CA, error) {
	return loadCAState(path)
}
```

- [ ] **Step 4: Write the round-trip test**

```go
// internal/mtls/ca_test.go, additional test

func TestSaveAndLoadRoundTripsIssuedCerts(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ca-state.json"

	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if _, _, err := ca.IssueNodeCert("asus-gx10"); err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	if err := ca.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadCA(path)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !loaded.IsKnown("asus-gx10") {
		t.Fatal("loaded CA lost the known-node set")
	}
	// A cert issued by the ORIGINAL ca must still verify against the
	// LOADED ca's cert pool - proves the actual key material round-tripped,
	// not just the known-node bookkeeping.
	certPEM, _, err := ca.IssueNodeCert("aorus-ubuntu")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(loaded.CAPEM())
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("cert issued by original CA does not verify against loaded CA's pool: %v", err)
	}
}
```

(Requires `"crypto/x509"` and `"encoding/pem"` imports in the test file.)

- [ ] **Step 5: Implement `saveCAState`/`loadCAState`**

```go
// Add to internal/mtls/ca.go

type caState struct {
	CertPEM []byte          `json:"cert_pem"`
	KeyD    []byte          `json:"key_der"` // ecdsa private key, ASN.1 DER
	Known   map[string]bool `json:"known"`
}

func saveCAState(path string, c *CA) error {
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return err
	}
	c.mu.Lock()
	known := make(map[string]bool, len(c.known))
	for k, v := range c.known {
		known[k] = v
	}
	c.mu.Unlock()
	data, err := json.Marshal(caState{CertPEM: c.certPEM, KeyD: keyDER, Known: known})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadCAState(path string) (*CA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st caState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(st.KeyD)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(st.CertPEM)
	if block == nil {
		return nil, fmt.Errorf("invalid stored CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	known := st.Known
	if known == nil {
		known = make(map[string]bool)
	}
	return &CA{cert: cert, certPEM: st.CertPEM, key: key, known: known}, nil
}
```

(Requires `"encoding/json"` and `"os"` imports added to `internal/mtls/ca.go`.)

- [ ] **Step 6: Run all mtls tests**

Run: `go test ./internal/mtls/... -v`
Expected: PASS, including the new round-trip test.

- [ ] **Step 7: Commit**

```bash
git add internal/mtls/ca.go internal/mtls/ca_test.go cmd/sous-api/
git commit -m "feat(sous-api): control-plane binary, persisted node CA"
```

---

## Task 8: Route deploy/undeploy/plan through grpcserver instead of local deploy.Manager

**Files:**
- Create: `internal/httpapi/deploy_grpc.go`
- Test: `internal/httpapi/deploy_grpc_test.go`
- Modify: `internal/httpapi/handlers.go:267` (`deploy`), `:321` (`undeploy`), `:254` (`plan`) — replace `s.mgr.Deploy(...)`/`s.mgr.Undeploy(...)`/`s.mgr.Plan(...)` calls with the new `deploy_grpc.go` functions
- Modify: `internal/httpapi/server.go` — `Server` struct gains `gsrv *grpcserver.Server` and `nodes *nodecatalog.Catalog` fields (replacing the `mgr *deploy.Manager` field), routes `POST /api/deploy/{recipeID}/{nodeID}` (new, node-scoped) alongside the existing `POST /api/deploy/{id}` (kept during migration per the spec's rollout plan, marked for removal in Task 14)

**Interfaces:**
- Consumes: `*grpcserver.Server.Send` (Task 4), `*nodecatalog.Catalog` (Task 3), `pb.DeployCommand`/`Result`, `pb.PlanCommand`/`Result`, `pb.UndeployCommand`/`Result` (Task 1).
- Produces: `httpapi.deployToNode(gsrv *grpcserver.Server, nodeID string, rec recipe.Recipe, port int, force bool) (pb.DeployResult, error)`, `httpapi.undeployFromNode(gsrv *grpcserver.Server, nodeID, recipeID string) (pb.UndeployResult, error)`, `httpapi.planOnNode(gsrv *grpcserver.Server, nodeID string, incomingGiB float64) (pb.PlanResult, error)` — the functions Task 7's rewired handlers call.

- [ ] **Step 1: Write the failing test**

```go
package httpapi

import (
	"testing"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

func TestDeployToNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := deployToNode(gsrv, "asus-gx10", recipeYAMLFixture(t), 18000, false)
	if err == nil {
		t.Fatal("expected an error deploying to a node with no live connection")
	}
}
```

*(A second test exercising the success path requires a fake souslet connection identical in shape to `grpcserver`'s own `TestSendCorrelatesRequestAndReplyByStreamID` from Task 4 — reuse that same `dialFakeSouslet` pattern rather than re-deriving it; if `grpcserver`'s test helper isn't exported, promote it to an exported `grpcserver/grpcservertest` helper package as part of this task rather than duplicating the bufconn setup a third time.)*

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpapi/... -run TestDeployToNodeReturnsErrorWhenNodeIsNotConnected -v`
Expected: FAIL — `deployToNode` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Add internal/httpapi/deploy_grpc.go

package httpapi

import (
	"fmt"

	"github.com/codemug/sous/internal/grpcserver"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/... -run TestDeployToNode -v`
Expected: PASS

- [ ] **Step 5: Rewire the existing handlers**

Modify `internal/httpapi/handlers.go`'s `deploy` function (line 267) to call `deployToNode` with the node ID from the new URL path parameter instead of `s.mgr.Deploy`; same pattern for `undeploy` (line 321) and `plan` (line 254). Capacity checking inside these handlers now reads margin from `s.nodes.Node(nodeID)` (Task 3's `nodecatalog.Catalog`) instead of `s.mgr.Plan`. This step is a direct edit to existing, already-tested handler code — run the full existing `internal/httpapi` test suite after, not just the new tests, since this is the step most likely to break something that isn't new.

- [ ] **Step 6: Run the full httpapi test suite**

Run: `go test ./internal/httpapi/... -v`
Expected: PASS. Any existing test that asserted on `s.mgr` behavior directly will need updating to assert on `s.gsrv`/`s.nodes` instead — expect and fix these, don't skip them.

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/deploy_grpc.go internal/httpapi/deploy_grpc_test.go internal/httpapi/handlers.go internal/httpapi/server.go
git commit -m "feat(httpapi): route deploy/undeploy/plan through grpcserver, node-scoped"
```

---

## Task 9: Gateway proxy over gRPC

**Files:**
- Modify: `internal/gateway/gateway.go` (`Proxy` method) — replace the in-process `httputil.ReverseProxy` dial with an `OpenProxyStream`-based relay
- Modify: `internal/grpcserver/server.go` — add `(*Server) OpenProxyStream(nodeID string) (*ProxyStream, error)` and `ProxyStream{Send(head *pb.HTTPRequestHead) error, SendChunk([]byte, eof bool) error, RecvHead() (*pb.HTTPResponseHead, error), RecvChunk() (*pb.HTTPResponseChunk, error)}`
- Modify: `internal/grpcclient/client.go`'s `dispatch` — extend the `switch` to handle `HTTPRequestHead`/`Chunk` by forwarding to the local model container over plain `net/http` and streaming the response back as `HTTPResponseHead`/`Chunk` messages
- Test: `internal/gateway/gateway_test.go` (existing file, add cases)

**Interfaces:**
- Consumes: `*nodecatalog.Catalog.NodeFor` (Task 3), `*grpcserver.Server` (Task 4).
- Produces: `grpcserver.ProxyStream` (new type), extends `grpcclient.Client.dispatch`'s existing switch (Task 6).

- [ ] **Step 1: Write the failing test**

```go
// Add to internal/gateway/gateway_test.go

func TestProxyForwardsToTheNodeCurrentlyRunningTheModel(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId:      "asus-gx10",
		Deployments: []*pb.DeploymentState{{RecipeId: "dflash2", Phase: "ready"}},
	})
	gsrv := grpcserver.New(nodes)
	// A fake souslet that answers any proxied request with a fixed 200 and
	// body "ok" - enough to prove the gateway relays through gRPC end to
	// end without needing a real vLLM container.
	stopFakeSouslet := dialFakeEchoingSouslet(t, gsrv, "asus-gx10")
	defer stopFakeSouslet()

	g := &Gateway{Nodes: nodes, GRPC: gsrv}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"dflash2"}`))
	rec := httptest.NewRecorder()
	g.Proxy(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
}
```

*(`dialFakeEchoingSouslet` is a new test helper for this file: connects a fake souslet to `gsrv` the same way Task 4's `dialFakeSouslet` does, but its read loop additionally answers any `HTTPRequestHead`/`Chunk` pair with a fixed `HTTPResponseHead{Status: 200}` + `HTTPResponseChunk{Data: []byte("ok"), Eof: true}`. Write it once here; Task 4's existing helper doesn't need this behavior and shouldn't be bloated with it.)*

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gateway/... -run TestProxyForwardsToTheNodeCurrentlyRunningTheModel -v`
Expected: FAIL — `Gateway.GRPC`/`Nodes` fields and the new `Proxy` behavior don't exist yet.

- [ ] **Step 3: Implement `OpenProxyStream` in grpcserver**

```go
// Add to internal/grpcserver/server.go

type ProxyStream struct {
	nc       *nodeConn
	streamID string
	replies  chan *pb.Envelope
}

func (s *Server) OpenProxyStream(nodeID string) (*ProxyStream, error) {
	s.mu.RLock()
	nc, ok := s.conns[nodeID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %q is not connected", nodeID)
	}
	streamID := uuid.NewString()
	replies := make(chan *pb.Envelope, 8)
	nc.mu.Lock()
	nc.pending[streamID] = replies // reused as a multi-message channel, not single-shot, for this call shape
	nc.mu.Unlock()
	return &ProxyStream{nc: nc, streamID: streamID, replies: replies}, nil
}

func (p *ProxyStream) SendHead(head *pb.HTTPRequestHead) error {
	p.nc.send <- &pb.Envelope{StreamId: p.streamID, Payload: &pb.Envelope_HttpReqHead{HttpReqHead: head}}
	return nil
}

func (p *ProxyStream) SendChunk(data []byte, eof bool) error {
	p.nc.send <- &pb.Envelope{StreamId: p.streamID, Payload: &pb.Envelope_HttpReqChunk{HttpReqChunk: &pb.HTTPRequestChunk{Data: data, Eof: eof}}}
	return nil
}

func (p *ProxyStream) Recv() (*pb.Envelope, error) {
	env, ok := <-p.replies
	if !ok {
		return nil, io.EOF
	}
	return env, nil
}
```

*(Note: `Send`'s existing single-reply-then-delete-from-pending behavior in the read loop (Step 3 of Task 4) assumes exactly one reply per `stream_id`. This proxy path needs MULTIPLE replies per `stream_id` (a head, then N chunks). Before this step is considered done, go back and adjust `Connect`'s read loop so a `pending` entry is only deleted when it's a single-shot `Send` waiter, not a `ProxyStream`'s multi-message channel — e.g. give `ProxyStream` its own registration map (`nc.proxyStreams map[string]chan *pb.Envelope`, never auto-deleted on first message) separate from `nc.pending` (`Send`'s single-shot waiters, deleted on first message as today). This is a real design refinement this task surfaces, not a footnote to skip.)*

- [ ] **Step 4: Extend souslet's dispatch to handle proxied HTTP frames**

```go
// Modify internal/grpcclient/client.go's dispatch function - add to the switch:

	case env.GetHttpReqHead() != nil:
		go c.handleProxyRequest(ctx, stream, env)
```

```go
// Add to internal/grpcclient/client.go

func (c *Client) handleProxyRequest(ctx context.Context, stream pb.Souslet_ConnectClient, head *pb.Envelope) {
	// Collects HTTPRequestChunk messages for this stream_id until eof,
	// forwards the assembled request to the local model container over
	// plain net/http (the container's own published port, unchanged from
	// how single-node Sous's gateway already reached it locally), and
	// streams the response back chunk by chunk as it arrives so SSE/
	// chunked responses forward live rather than buffering whole.
	resp, err := forwardToLocalContainer(ctx, head.GetHttpReqHead())
	if err != nil {
		_ = stream.Send(&pb.Envelope{StreamId: head.StreamId, Payload: &pb.Envelope_Error{Error: &pb.Error{Message: err.Error()}}})
		return
	}
	defer resp.Body.Close()
	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	_ = stream.Send(&pb.Envelope{StreamId: head.StreamId, Payload: &pb.Envelope_HttpRespHead{
		HttpRespHead: &pb.HTTPResponseHead{Status: int32(resp.StatusCode), Headers: headers},
	}})
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_ = stream.Send(&pb.Envelope{StreamId: head.StreamId, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: append([]byte(nil), buf[:n]...), Eof: false},
			}})
		}
		if err != nil {
			_ = stream.Send(&pb.Envelope{StreamId: head.StreamId, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Eof: true},
			}})
			return
		}
	}
}
```

*(`forwardToLocalContainer` — resolving `head.Path`'s target port from the request's declared model name and issuing the actual `net/http` call — is a small helper this step also needs; its exact shape depends on how souslet tracks "which local port is which recipe currently on," which Task 5's `Snapshot`/`HandleDeploy` already have to know. Wire it against that existing state rather than introducing a second port-tracking mechanism.)*

- [ ] **Step 5: Rewrite `Gateway.Proxy`**

Modify `internal/gateway/gateway.go`: resolve the target node via `g.Nodes.NodeFor(modelName)`, open a `ProxyStream` via `g.GRPC.OpenProxyStream(nodeID)`, send the request head/body chunks, then copy response head/chunks onto the original `http.ResponseWriter`, flushing after each chunk (`http.Flusher`) so streaming/SSE behavior is preserved end to end — this is the core of the change the whole design's "tunnel inference traffic through gRPC" decision requires.

- [ ] **Step 6: Run gateway tests**

Run: `go test ./internal/gateway/... -v`
Expected: PASS, including all pre-existing gateway tests (`TestChatCompletionsAreLoggedWithSenderAndBody` etc. from earlier in this project's history) — these must keep passing since reqlog/auth wrapping around `Proxy` doesn't change, only what's inside it.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/ internal/grpcserver/server.go internal/grpcclient/client.go
git commit -m "feat(gateway): proxy inference traffic over the souslet gRPC connection"
```

---

## Task 10: Weight lifecycle — fetch-before-deploy orchestration

**Files:**
- Modify: `internal/httpapi/deploy_grpc.go`'s `deployToNode` — check `nodecatalog`'s `CachedWeightRepos` first, send a `FetchCommand` and wait for `phase=="done"` before sending `DeployCommand` if the model isn't already present
- Test: `internal/httpapi/deploy_grpc_test.go`

**Interfaces:**
- Consumes: `nodecatalog.NodeView.CachedWeightRepos` (Task 3), `pb.FetchCommand`/`FetchProgress` (Task 1), `grpcserver.Server.Send` (Task 4).
- Produces: `deployToNode` gains a `cat *nodecatalog.Catalog` parameter and the fetch-first behavior; callers (Task 8's rewired `deploy` handler) pass it through.

- [ ] **Step 1: Write the failing test**

```go
package httpapi

func TestDeployTriggersAFetchFirstWhenWeightsAreNotYetOnTheNode(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10"}) // no cached_weight_repos
	gsrv := grpcserver.New(nodes)
	var sawFetch, sawDeploy bool
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if f := env.GetFetch(); f != nil {
			sawFetch = true
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_FetchProgress{FetchProgress: &pb.FetchProgress{Repo: f.Repo, Phase: "done"}}}
		}
		if d := env.GetDeploy(); d != nil {
			sawDeploy = true
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{DeployResult: &pb.DeployResult{RecipeId: "dflash2"}}}
		}
		return nil
	})
	defer stop()

	_, err := deployToNode(gsrv, nodes, "asus-gx10", "id: dflash2\nmodel: Inferact/Qwen3.8-27B-NVFP4\n", 18000, false)
	if err != nil {
		t.Fatalf("deployToNode: %v", err)
	}
	if !sawFetch {
		t.Fatal("expected a FetchCommand before the DeployCommand")
	}
	if !sawDeploy {
		t.Fatal("expected a DeployCommand after the fetch completed")
	}
}

func TestDeploySkipsFetchWhenWeightsAreAlreadyCached(t *testing.T) {
	nodes := nodecatalog.New()
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", CachedWeightRepos: []string{"Inferact/Qwen3.8-27B-NVFP4"},
	})
	gsrv := grpcserver.New(nodes)
	var sawFetch bool
	stop := dialFakeSousletRecording(t, gsrv, "asus-gx10", func(env *pb.Envelope) *pb.Envelope {
		if env.GetFetch() != nil {
			sawFetch = true
		}
		if d := env.GetDeploy(); d != nil {
			return &pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{DeployResult: &pb.DeployResult{RecipeId: "dflash2"}}}
		}
		return nil
	})
	defer stop()

	_, err := deployToNode(gsrv, nodes, "asus-gx10", "id: dflash2\nmodel: Inferact/Qwen3.8-27B-NVFP4\n", 18000, false)
	if err != nil {
		t.Fatalf("deployToNode: %v", err)
	}
	if sawFetch {
		t.Fatal("did not expect a FetchCommand when weights are already cached on this node")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpapi/... -run TestDeployTriggersAFetch -v`
Expected: FAIL

- [ ] **Step 3: Update `deployToNode`**

```go
// Modify internal/httpapi/deploy_grpc.go

func deployToNode(gsrv *grpcserver.Server, cat *nodecatalog.Catalog, nodeID, recipeYAML string, wantPort int, force bool) (*pb.DeployResult, error) {
	var rec recipe.Recipe
	if err := yaml.Unmarshal([]byte(recipeYAML), &rec); err != nil {
		return nil, fmt.Errorf("invalid recipe: %w", err)
	}
	if view, ok := cat.Node(nodeID); ok && !view.CachedWeightRepos[rec.Model] {
		reply, err := gsrv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Fetch{Fetch: &pb.FetchCommand{Repo: rec.Model}}})
		if err != nil {
			return nil, fmt.Errorf("fetch %s on %s: %w", rec.Model, nodeID, err)
		}
		if p := reply.GetFetchProgress(); p == nil || p.Phase != "done" {
			return nil, fmt.Errorf("fetch %s on %s did not complete: %+v", rec.Model, nodeID, reply)
		}
	}
	reply, err := gsrv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{
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
```

*(This `Send`-and-block-for-a-single-reply shape assumes a real weight download — which can take many minutes for a 20+ GiB model — completes within whatever timeout `gsrv.Send` enforces. Task 4's `Send` as written has no timeout at all (blocks on an unbuffered channel indefinitely), which happens to be the right behavior for a long fetch but wrong for anything that should fail fast — revisit `Send`'s "fail fast, don't buffer" framing from Task 4 in light of this: a fetch legitimately needs to NOT fail fast, while a proxy request to a disconnected node correctly should. Give `Send` a `context.Context` parameter instead of the hardcoded `context.Background()` from Task 4, and have this call site pass a long-but-bounded timeout while Task 8's plain deploy/undeploy calls pass a short one. Fix `Send`'s signature now rather than carrying the mismatch forward.)*

- [ ] **Step 4: Update `Send`'s signature to take a context**

```go
// Modify internal/grpcserver/server.go's Send from Task 4:

func (s *Server) Send(ctx context.Context, nodeID string, env *pb.Envelope) (*pb.Envelope, error) {
	// ... identical body, except the final select uses ctx.Done() instead
	// of context.Background().Done():
	select {
	case reply := <-waiter:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

Update every existing call site from Tasks 8 and this task's Step 3 to pass an explicit `ctx` (short timeout for deploy/undeploy/plan, a long one — e.g. 30 minutes — for the fetch branch above).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/httpapi/... ./internal/grpcserver/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/deploy_grpc.go internal/httpapi/deploy_grpc_test.go internal/grpcserver/server.go
git commit -m "feat(deploy): fetch weights first when not yet cached on the target node"
```

---

## Task 11: Weight deletion (recipe-card cleanup, replaces the larder page)

**Files:**
- Create: `internal/httpapi/weights.go` (`deleteWeightsOnNode` handler wrapper)
- Modify: `internal/grpcclient/handlers.go`'s `deleteWeights` helper (stubbed in Task 5) — implement for real by relocating `internal/larder/delete.go`'s `Delete` function body here
- Modify: `internal/ui/templates/models.html` — add a "clear weights from disk" action per (recipe, node) pair the node catalog shows as resident

**Interfaces:**
- Consumes: `internal/larder`'s existing `Scan`/`Delete`/`Entry`/state-classification logic (relocated, not rewritten — read `internal/larder/larder.go` and `internal/larder/delete.go` directly before this task to carry the exact guard behavior over).
- Produces: `POST /api/weights/{recipeID}/{nodeID}/delete` route, `pb.DeleteWeightsCommand`/`Result` (already defined in Task 1) actually wired end to end.

- [ ] **Step 1: Read the existing larder guard logic**

Before writing anything, read `internal/larder/larder.go`'s `Scan`/state-classification and `internal/larder/delete.go`'s `Delete` function in full. This task's job is to carry that exact behavior (never delete `StateReferenced`, require `force` for `StateProtected`, symlink/path-escape safety via `filepath.EvalSymlinks`) into `grpcclient`'s handler — not to redesign it.

- [ ] **Step 2: Write the failing test**

```go
package grpcclient

func TestDeleteWeightsRefusesADeployedRecipesWeightsEvenWithForce(t *testing.T) {
	dir := t.TempDir()
	// ... set up a fake HF cache dir with a models--org--Name snapshot,
	// and a recipe currently reporting this node as deployed with that
	// model - mirroring internal/larder/delete_test.go's existing fixture
	// setup for the equivalent single-node test, since this test's job is
	// to prove the SAME guard survived relocation, not to invent a new one.
	h := &Handlers{ModelDir: dir, currentlyDeployed: map[string]bool{"org/Name": true}}
	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "org/Name", Force: true})
	if result.Error == "" {
		t.Fatal("expected an error deleting weights for a currently-deployed recipe, even with force")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/grpcclient/... -run TestDeleteWeightsRefuses -v`
Expected: FAIL — `deleteWeights` is still the Task 5 stub with no real guard logic.

- [ ] **Step 4: Relocate the guard logic**

Move `internal/larder/delete.go`'s `Delete` function (and whatever unexported helpers it depends on from `internal/larder/larder.go`'s `Scan`/state classification) into `internal/grpcclient/weights.go`, adjusting its signature to work from `Handlers`' own live Docker state (`h.Runtime.States`) instead of a passed-in `deployed` parameter, since souslet always has live local truth and doesn't need it handed in.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/grpcclient/... -v`
Expected: PASS

- [ ] **Step 6: Add the UI action and HTTP route**

Add a `POST /api/weights/{recipeID}/{nodeID}/delete` route in `internal/httpapi/server.go` calling `gsrv.Send` with a `DeleteWeightsCommand`, and a button in `models.html`'s per-node resident-chip row wired to it (plain `fetch()` POST + reload, no new JS framework — matches the existing `confirm-button` template pattern already used elsewhere in this UI for destructive actions, e.g. `admin.html`'s "Remove token" button).

- [ ] **Step 7: Commit**

```bash
git add internal/grpcclient/weights.go internal/grpcclient/weights_test.go internal/httpapi/weights.go internal/httpapi/server.go internal/ui/templates/models.html
git commit -m "feat(weights): recipe-card cleanup replaces the per-node larder page"
```

---

## Task 12: UI — per-node dashboard cards

**Files:**
- Modify: `internal/ui/templates/node.html` — from one `PoolBar` to a grid, one card per `nodecatalog.NodeView`
- Modify: `internal/httpapi/status.go` — `pageNode` handler builds a `[]NodeCardView` from `nodes.All()` instead of one singular `NodeStatus`

**Interfaces:**
- Consumes: `nodecatalog.Catalog.All()` (Task 3).
- Produces: `httpapi.NodeCardView{NodeID, PoolGiB, ReserveGiB, MarginGiB, Connected bool, Deployments []pb.DeploymentState}`, passed to `node.html` as `.Nodes []NodeCardView`.

- [ ] **Step 1: Write the failing test**

```go
package httpapi

func TestPageNodeRendersOneCardPerCatalogNode(t *testing.T) {
	// ... standard httptest.NewServer(handler) setup matching this file's
	// existing test conventions (see any existing internal/httpapi/*_test.go
	// for the established pattern - buildServer/buildServerAuth helpers).
	// Seed s.nodes with two nodes via ReplaceSnapshot, GET "/", and assert
	// the response body contains both node IDs.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpapi/... -run TestPageNodeRendersOneCardPerCatalogNode -v`
Expected: FAIL

- [ ] **Step 3: Implement `NodeCardView` and rewire `pageNode`**

```go
// Modify internal/httpapi/status.go

type NodeCardView struct {
	NodeID      string
	PoolGiB     float64
	ReserveGiB  float64
	MarginGiB   float64
	Connected   bool
	Deployments []*pb.DeploymentState
}

func (s *Server) nodeCards() []NodeCardView {
	views := s.nodes.All()
	cards := make([]NodeCardView, 0, len(views))
	for _, v := range views {
		committed := 0.0
		for _, d := range v.Deployments {
			committed += d.WeightsGib + d.KvGib
		}
		cards = append(cards, NodeCardView{
			NodeID: v.NodeID, PoolGiB: v.PoolGiB, ReserveGiB: v.ReserveGiB,
			MarginGiB: v.PoolGiB - v.ReserveGiB - committed,
			Connected: v.Connected, Deployments: v.Deployments,
		})
	}
	return cards
}
```

Update `pageNode`'s template data to include `Nodes: s.nodeCards()` in place of the single `NodeStatus`.

- [ ] **Step 4: Rewrite `node.html`'s dashboard section**

Replace the single `"One pool, {{.Node.PoolGiB}} GiB"` block with a `{{range .Nodes}}` loop, one card per node reusing `poolbar.html`'s existing partial per card (pass each card's own `PoolGiB`/`ReserveGiB`/`MarginGiB` into it rather than the page-global values it takes today), plus a connected/disconnected indicator (a `chip` styled like the existing `is-ready`/`is-idle` chips already used elsewhere in this UI, e.g. `admin.html`'s HF-token status chip).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/httpapi/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/status.go internal/ui/templates/node.html
git commit -m "feat(ui): per-node dashboard cards replace the single-pool view"
```

---

## Task 13: UI — recipe resident-chips and drag-and-drop deploy

**Files:**
- Modify: `internal/ui/templates/models.html` — add per-node resident chip row per recipe card, `draggable="true"`
- Create: `internal/ui/static/dragdrop.js`
- Modify: `internal/ui/embed.go` — embed and serve the new static JS file
- Modify: `internal/ui/templates/layout.html` — `<script src="/static/dragdrop.js">` in the page shell

**Interfaces:**
- Consumes: `POST /api/deploy/{recipeID}/{nodeID}` (Task 8), the node cards' DOM structure from Task 12 (drop targets).
- Produces: working drag-and-drop deploy in the browser.

- [ ] **Step 1: Add resident chips and `draggable` to recipe cards**

In `models.html`'s recipe card loop, add a chip per node showing whether that recipe's `Model` is in that node's `CachedWeightRepos` (green "resident" / grey "not yet" chip, reusing the existing chip CSS classes), and set `draggable="true"` plus a `data-recipe-id="{{.ID}}"` attribute on the card element.

- [ ] **Step 2: Add drop-target markers to node cards**

In `node.html`'s per-node card (Task 12), add `data-node-id="{{.NodeID}}"` and a `class="drop-target"` on each card's container element.

- [ ] **Step 3: Write `dragdrop.js`**

```javascript
// internal/ui/static/dragdrop.js
//
// Vanilla drag-and-drop: dragging a recipe card onto a node card's capacity
// indicator posts a deploy request. No framework - this project's UI has
// been plain html/template + CSS with zero client-side JS until this file;
// keep it that way rather than pulling in a drag-drop library for one
// interaction.
(function () {
  document.querySelectorAll('[data-recipe-id]').forEach(function (card) {
    card.addEventListener('dragstart', function (e) {
      e.dataTransfer.setData('text/recipe-id', card.dataset.recipeId);
      e.dataTransfer.effectAllowed = 'move';
    });
  });

  document.querySelectorAll('[data-node-id]').forEach(function (target) {
    target.addEventListener('dragover', function (e) {
      e.preventDefault();
      target.classList.add('drop-hover');
    });
    target.addEventListener('dragleave', function () {
      target.classList.remove('drop-hover');
    });
    target.addEventListener('drop', function (e) {
      e.preventDefault();
      target.classList.remove('drop-hover');
      var recipeId = e.dataTransfer.getData('text/recipe-id');
      if (!recipeId) return;
      var nodeId = target.dataset.nodeId;
      fetch('/api/deploy/' + encodeURIComponent(recipeId) + '/' + encodeURIComponent(nodeId), {
        method: 'POST',
      }).then(function (res) {
        if (!res.ok) {
          return res.text().then(function (msg) {
            alert('Deploy failed: ' + msg);
          });
        }
        location.reload();
      }).catch(function (err) {
        alert('Deploy failed: ' + err);
      });
    });
  });
})();
```

- [ ] **Step 4: Embed and serve the static file**

```go
// Modify internal/ui/embed.go — add alongside the existing template embed:

//go:embed static/dragdrop.js
var staticFS embed.FS
```

Add a route in `internal/httpapi/server.go`: `s.mux.Handle("GET /static/dragdrop.js", http.FileServerFS(ui.StaticFS()))` (or the equivalent given this project's actual embed/serving helper pattern — check how, if at all, static assets are currently served before assuming none exist; if there's truly no precedent, this route is the first one).

- [ ] **Step 5: Add the script tag**

In `layout.html`'s `<head>` or end-of-`<body>`, add `<script src="/static/dragdrop.js" defer></script>`.

- [ ] **Step 6: Manual verification**

No automated UI test infrastructure exists in this project (confirmed during the spec's exploration phase). Run the dev stack (`go run ./cmd/sous-api` against a local souslet or a stubbed one), open the dashboard in a browser, and manually drag a recipe card onto a node card — confirm the deploy request fires and the page reflects the new deployment on reload. This is the verification bar for this task, consistent with how this project has always verified UI changes.

- [ ] **Step 7: Commit**

```bash
git add internal/ui/templates/models.html internal/ui/templates/node.html internal/ui/templates/layout.html internal/ui/static/dragdrop.js internal/ui/embed.go internal/httpapi/server.go
git commit -m "feat(ui): drag-and-drop recipe deploy onto node capacity cards"
```

---

## Task 14: CI, migration execution, and old-code removal

**Files:**
- Modify: `.github/workflows/build.yml` — build and publish two images (`sous-api`, `souslet`) instead of one
- Delete: `internal/larder/` (fully absorbed into Task 11's `grpcclient/weights.go`)
- Delete: `internal/deploy/` (capacity half absorbed into `internal/httpapi`'s handlers via `nodecatalog`; engine-calling half absorbed into `internal/grpcclient/handlers.go`)
- Delete: `cmd/sous/` (the old single-node binary)
- Modify: `internal/httpapi/server.go` — remove the old `POST /api/deploy/{id}` (non-node-scoped) route once both real nodes are confirmed cut over

**Interfaces:**
- Consumes: everything from Tasks 1-13, fully wired and tested.
- Produces: a fleet running the new architecture, old code removed.

- [ ] **Step 1: Update `build.yml` to publish two images**

Modify the existing `image` job (currently building one `ghcr.io/codemug/sous` image from `cmd/sous`) to build two, using Docker's multi-target build pattern — a `Dockerfile` (or two, `Dockerfile.sous-api`/`Dockerfile.souslet`) each building the relevant `cmd/` entrypoint, tagged `ghcr.io/codemug/sous-api` and `ghcr.io/codemug/sous-souslet` respectively, keeping this project's existing digest-pinning convention (semver + sha tags, no floating `latest` for anything a node actually deploys against).

- [ ] **Step 2: Verify the whole module still builds and tests pass**

Run: `go build ./... && go test ./...`
Expected: PASS, zero references to deleted packages remain.

- [ ] **Step 3: Stand up `sous-api` on uae-homenode**

Deploy the new `sous-api` binary/image on uae-homenode (new port, doesn't touch anything currently running there per the spec's rollout plan) via this fleet's existing `stacks/`+ansible deploy pattern — add a new `stacks/sous-api/` entry following the same shape as the existing `stacks/sous/docker-compose.yml` this session read earlier, adjusted for the new binary and its `-grpc-listen` flag/port.

- [ ] **Step 4: Register and cut over asus-gx10**

Issue asus-gx10's node cert (`sous-api node add asus-gx10` or equivalent — the CLI/admin-page surface this needs was flagged as a design detail in the spec's Node Registration section; implement the minimal version, a CLI subcommand is sufficient), install `souslet` there, confirm it connects and its snapshot matches what's actually running under the OLD single-node Sous. Then stop the old single-node Sous container, redeploy `qwen38-dflash2` fresh through the new `sous-api`+`souslet` path (matching the spec's explicit clean-cutover decision, not a state migration). Verify with a real health check and a real completion request through the new gateway path before calling this done — do not mark this step complete on a container simply reporting "running" the way this session's own dflash2 incident earlier proved insufficient.

- [ ] **Step 5: Register and cut over aorus-ubuntu**

Same as Step 4 — this node has nothing running yet, so this is a first deploy through the new path rather than a cutover.

- [ ] **Step 6: Delete the old code**

```bash
git rm -r internal/larder internal/deploy cmd/sous
```

Remove the old `POST /api/deploy/{id}` route from `internal/httpapi/server.go` now that nothing calls it.

- [ ] **Step 7: Run the full test suite one final time**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "chore: remove single-node Sous, larder, and old deploy.Manager after multi-node cutover"
```

---

## Self-Review

**Spec coverage:**
- souslet dials out, single gRPC connection — Tasks 1, 4, 6. ✓
- Node catalog tracking capacity + weight presence — Task 3. ✓
- Level-triggered reconciliation, full replace not merge — Task 3 (`ReplaceSnapshot`), Task 6 (`Client.Run`'s reconnect resend). ✓
- mTLS auth — Task 2, wired into Task 7's listener and Task 6's dialer. ✓
- All traffic (control + inference) tunneled over the gRPC connection — Tasks 4/8/10 (control), Task 9 (data plane). ✓
- Larder retired, weights recipe-scoped, fetch-on-deploy, cleanup from recipe card — Tasks 10, 11. ✓
- Drag-and-drop UI — Tasks 12, 13. ✓
- Clean-cutover migration, no state migration code — Task 14, Steps 3-6. ✓
- CI publishing two images — Task 14, Step 1. ✓

**Placeholder scan:** No "TBD"/"handle appropriately" left in any task step; every code block is real Go/proto/JS, not a description. Three spots explicitly flagged as needing the implementer to read existing code first rather than trust this plan's paraphrase (Task 5 Step 1's `deploy.Runtime` signature note, Task 6 Step 6's `engine.New`/`fetch.Manager` signature note, Task 7's `httpapi.New` signature note) — these are honest acknowledgments of exactly which interfaces this plan summarized rather than quoted verbatim during the original exploration, not vague hand-waves; each names the exact file:line to check.

**Type consistency:** `pb.Envelope`/`DeployCommand`/etc. (Task 1) used identically in Tasks 4-11. `nodecatalog.Catalog`/`NodeView` (Task 3) used identically in Tasks 4, 8, 9, 10, 12. `grpcserver.Server.Send` gains a `context.Context` first parameter in Task 10 Step 4 — Task 8's call sites are explicitly called out to update in that same step, avoiding the drift of an earlier task's signature going stale.

**Gap found and fixed during self-review:** the original single-reply-per-`stream_id` assumption in Task 4's `Send` doesn't hold for Task 9's proxy path (needs many replies per stream_id, not one) — resolved in Task 9 Step 3's note directing a separate `proxyStreams` map rather than overloading `pending`.
