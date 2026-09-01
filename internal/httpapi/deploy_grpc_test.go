package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/recipe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// dialFakeSousletRecording drives the client side of grpcserver.Server's
// Connect RPC over bufconn, standing in for a real souslet binary so these
// tests need no Docker. Every envelope the server sends to this fake node is
// handed to respond; whatever respond returns (nil for "don't answer this
// one") is sent straight back, correlated by the same stream_id - letting a
// test script exactly the Fetch/Deploy exchange deployToNode is expected to
// drive.
//
// The handshake NodeSnapshot Connect requires as the very first message on a
// new connection re-sends whatever the catalog already knows about nodeID
// (CachedWeightRepos included) rather than a bare NodeSnapshot{NodeId:
// nodeID}: ReplaceSnapshot is a full replace, not a merge (see its own doc
// comment in nodecatalog.go), so a bare handshake would silently wipe out
// CachedWeightRepos a test configured on the catalog before dialing.
func dialFakeSousletRecording(t *testing.T, gsrv *grpcserver.Server, nodeID string, respond func(*pb.Envelope) *pb.Envelope) func() {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	pb.RegisterSousletServer(s, gsrv)
	go func() { _ = s.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	client := pb.NewSousletClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	handshake := &pb.NodeSnapshot{NodeId: nodeID}
	if view, ok := gsrv.Catalog().Node(nodeID); ok {
		handshake.PoolGib = view.PoolGiB
		handshake.ReserveGib = view.ReserveGiB
		handshake.Deployments = view.Deployments
		for repo := range view.CachedWeightRepos {
			handshake.CachedWeightRepos = append(handshake.CachedWeightRepos, repo)
		}
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: handshake}}); err != nil {
		t.Fatalf("send initial snapshot: %v", err)
	}

	go func() {
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if reply := respond(env); reply != nil {
				reply.StreamId = env.StreamId
				_ = stream.Send(reply)
			}
		}
	}()

	// gsrv.Send fails fast if nodeID isn't registered in its connection map
	// yet - the client-side Send above only hands the handshake to the
	// local transport, it does not wait for the server's Connect goroutine
	// to finish registering the connection. Poll gsrv.Connected, not the
	// catalog's own Connected flag: the catalog updates a few instructions
	// before the connection's entry is added to gsrv's internal map (see
	// Connect's body / Connected's doc comment), so polling the catalog
	// here would leave a narrow window where a caller's very next gsrv.Send
	// races that registration and spuriously fails with "not connected" -
	// exactly what an earlier version of this helper hit intermittently
	// when both fetch-orchestration tests ran in the same process.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if gsrv.Connected(nodeID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %q never showed as connected", nodeID)
		}
		time.Sleep(5 * time.Millisecond)
	}

	return func() {
		_ = stream.CloseSend()
		_ = conn.Close()
		s.Stop()
	}
}

// recipeYAMLFixture is a minimal, valid recipe rendered to YAML the way
// deployToNode ships it to a node - the whole recipe, not just its ID.
func recipeYAMLFixture(t *testing.T) string {
	t.Helper()
	rec := recipe.Recipe{ID: "dflash2", Kind: recipe.KindVLLM, Model: "Inferact/Qwen3.8-27B-NVFP4"}
	out, err := recipeToYAML(rec)
	if err != nil {
		t.Fatalf("recipeToYAML: %v", err)
	}
	return out
}

func TestDeployToNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := deployToNode(gsrv, nodecatalog.New(), "asus-gx10", recipeYAMLFixture(t), 18000, false)
	if err == nil {
		t.Fatal("expected an error deploying to a node with no live connection")
	}
}

func TestUndeployFromNodeReturnsErrorWhenNodeIsNotConnected(t *testing.T) {
	gsrv := grpcserver.New(nodecatalog.New())
	_, err := undeployFromNode(gsrv, "asus-gx10", "dflash2")
	if err == nil {
		t.Fatal("expected an error undeploying from a node with no live connection")
	}
}

// TestDeployTriggersAFetchFirstWhenWeightsAreNotYetOnTheNode is the fetch-
// triggers-on-cache-miss case: a node whose last-known snapshot carries no
// CachedWeightRepos for this recipe's model must see a FetchCommand, and its
// FetchProgress must report phase "done", before deployToNode ever sends the
// DeployCommand.
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

// TestDeploySkipsFetchWhenWeightsAreAlreadyCached is the fetch-skipped-on-
// cache-hit case: a node whose last-known snapshot already lists this
// recipe's model in CachedWeightRepos must go straight to the DeployCommand,
// with no FetchCommand sent at all.
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

// TestPlanOnNodeUsesTheCatalogSnapshotNotALiveCall proves planOnNode never
// touches gRPC at all: a node the catalog has never heard of - so there is no
// live connection to even attempt - still gets a normal "not known" error
// rather than hanging or requiring a connection.
func TestPlanOnNodeReturnsErrorWhenNodeIsUnknown(t *testing.T) {
	cat := nodecatalog.New()
	_, err := planOnNode(cat, "dflash2", "asus-gx10", 24.5)
	if err == nil {
		t.Fatal("expected an error planning against a node the catalog has never seen")
	}
}

// TestPlanOnNodeComputesMarginFromTheCatalogSnapshot is the success path:
// once a node has reported a snapshot, planOnNode must answer from it
// synchronously, with the resident recipe itself excluded from its own
// footprint accounting.
func TestPlanOnNodeComputesMarginFromTheCatalogSnapshot(t *testing.T) {
	cat := nodecatalog.New()
	cat.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{
			{RecipeId: "already-resident", WeightsGib: 20, KvGib: 5},
		},
	})
	res, err := planOnNode(cat, "incoming-model", "asus-gx10", 60)
	if err != nil {
		t.Fatalf("planOnNode: %v", err)
	}
	// committed = 60 (incoming) + 25 (resident) = 85; usable = 121.6-24 = 97.6
	if !res.Fits {
		t.Fatalf("expected the plan to fit, got %+v", res)
	}
	wantMargin := (121.6 - 24) - 85
	if diff := res.MarginGiB - wantMargin; diff > 0.01 || diff < -0.01 {
		t.Fatalf("MarginGiB = %v, want ~%v (got %+v)", res.MarginGiB, wantMargin, res)
	}

	// Re-planning the ALREADY-resident recipe itself must not double-count
	// its own footprint against itself.
	res, err = planOnNode(cat, "already-resident", "asus-gx10", 25)
	if err != nil {
		t.Fatalf("planOnNode: %v", err)
	}
	if !res.Fits || res.CommittedGiB != 25 {
		t.Fatalf("re-planning a resident recipe must exclude its own prior entry, got %+v", res)
	}
}

// TestPlanOnNodeWarnsWhenMarginIsThin guards a review finding on Task 8:
// planOnNode's capacity.Planner previously left WarnFreeGiB at its zero
// value, silently dropping the swap-risk warning capacity.Planner.Plan sets
// when a plan fits but only barely - a real safety signal (see
// cmd/sous/main.go's own WarnFreeGiB: 12 and capacity/plan.go's package doc
// for the fleet measurements behind that number), not a cosmetic one. This
// margin (10 GiB, i.e. under 12) fits but must still carry a warning.
func TestPlanOnNodeWarnsWhenMarginIsThin(t *testing.T) {
	cat := nodecatalog.New()
	cat.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24})
	// usable = 121.6-24 = 97.6; committed = 87.6 -> margin = 10 (< the 12
	// GiB WarnFreeGiB threshold, but still >= 0, so it should fit AND warn).
	res, err := planOnNode(cat, "incoming-model", "asus-gx10", 87.6)
	if err != nil {
		t.Fatalf("planOnNode: %v", err)
	}
	if !res.Fits {
		t.Fatalf("expected the plan to still fit at a 10 GiB margin, got %+v", res)
	}
	if res.Warning == "" {
		t.Fatalf("expected a swap-risk warning at a 10 GiB margin (below the 12 GiB WarnFreeGiB threshold), got none: %+v", res)
	}
}

// ---------- node-scoped routes, end to end ----------
//
// These exercise the actual HTTP wiring (route registration, the deploy/
// undeploy/plan handlers' nodeID branch, JSON response shapes) rather than
// deploy_grpc.go's package-level functions directly, on a server whose gsrv
// has no live souslet connection - proving the new routes fail the way a
// real disconnected/unknown node would, and that the pre-existing
// non-node-scoped routes keep working unchanged on the very same server.

func TestDeployNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
	})
	// kokoro is a 3 GiB recipe (well under this node's margin), so this
	// clears the capacity check and fails only because nothing ever
	// connected to gsrv as "asus-gx10" - the catalog knowing about a node
	// from a past snapshot is not the same as gsrv having a live stream for
	// it, exactly like a node that reported once and then dropped.
	rr := post(t, h, "/api/deploy/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (capacity fits, but no live connection): %s", rr.Code, rr.Body)
	}
}

func TestDeployNodeRouteReturnsConflictWhenCapacityDoesNotFit(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	// A tiny pool: qwen38 alone (24.87+45.67 GiB declared) cannot fit in 10.
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 10, ReserveGib: 0,
	})
	rr := post(t, h, "/api/deploy/qwen38/asus-gx10", "", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["margin_gib"]; !ok {
		t.Fatalf("refusal must report a margin: %v", got)
	}
}

func TestDeployNodeRouteReturns404ForUnknownNode(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/deploy/kokoro/never-seen-node", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body)
	}
}

func TestUndeployNodeRouteReturnsBadGatewayWhenNodeHasNoLiveConnection(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/undeploy/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rr.Code, rr.Body)
	}
}

func TestPlanNodeRouteReportsMargin(t *testing.T) {
	h, nodes := newTestServerWithNodes(t)
	nodes.ReplaceSnapshot("asus-gx10", &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6, ReserveGib: 24,
	})
	rr := send(t, h, http.MethodGet, "/api/plan/kokoro/asus-gx10", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["margin_gib"]; !ok {
		t.Fatalf("plan must report a margin, got %v", got)
	}
}

// TestLegacyDeployRouteStillWorksAlongsideNodeScoped proves the two routes
// genuinely coexist rather than one accidentally shadowing the other: the
// pre-existing, non-node-scoped route still deploys through the local
// deploy.Manager exactly as before, on a server that also has a real
// gsrv/nodes pair wired in for the new route.
func TestLegacyDeployRouteStillWorksAlongsideNodeScoped(t *testing.T) {
	h, _ := newTestServerWithNodes(t)
	rr := post(t, h, "/api/deploy/kokoro", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy deploy: %d %s", rr.Code, rr.Body)
	}
}

// TestNodeScopedRoutesReturnCleanErrorsWhenGRPCIsNotConfigured guards a
// review finding on Task 8: cmd/sous - the single-node binary actually
// deployed on gx10 today - calls httpapi.New with nil gsrv/nodes. Before the
// fix, hitting any node-scoped route on that exact configuration reached
// grpcserver.Server.Send or nodecatalog.Catalog.Node on a nil receiver -
// both start by locking an embedded sync.RWMutex field, which nil-panics.
// The fix registers the three node-scoped routes only when gsrv and nodes
// are both non-nil, so on a nil-configured server they simply don't exist
// and no handler that could reach a nil gsrv/nodes ever runs.
//
// The two expected status codes below differ, and that asymmetry is real
// Go net/http.ServeMux behavior, not a loose end: this package registers
// "GET /" as the node-dashboard catch-all (s.pageNode), which itself
// answers 404 for any path but the literal root (TestNodePageDoesNot
// SwallowUnknownPaths covers that separately) - so an unmatched GET lands
// there and gets 404. POST has no catch-all registered at "/" at all (only
// GET is), so the mux sees the path matches SOME registered pattern - the
// GET catch-all - just not for POST, and answers 405 with an Allow header
// instead. Either way, nothing dispatches to deployNode/undeployFromNode/
// planOnNode and nothing nil-panics, which is what this test actually
// guards; asserting the exact codes (rather than a loose "any 4xx") keeps
// this test honest about what Go's mux really does here, and would catch a
// regression to the wrong KIND of error (a 500, say) just as well as to a
// panic.
func TestNodeScopedRoutesReturnCleanErrorsWhenGRPCIsNotConfigured(t *testing.T) {
	h := newTestServerNilGRPC(t)

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/plan/kokoro/asus-gx10", http.StatusNotFound},
		{http.MethodPost, "/api/deploy/kokoro/asus-gx10", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/undeploy/kokoro/asus-gx10", http.StatusMethodNotAllowed},
	} {
		rr := send(t, h, tc.method, tc.path, "", "")
		if rr.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d; body: %s", tc.method, tc.path, rr.Code, tc.want, rr.Body)
		}
	}

	// The legacy, non-node-scoped route must still work normally on this
	// exact configuration - this is the shape cmd/sous runs in production.
	rr := post(t, h, "/api/deploy/kokoro", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy deploy on a nil-gsrv/nodes server: %d %s", rr.Code, rr.Body)
	}
}
