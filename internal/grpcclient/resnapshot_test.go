package grpcclient

import (
	"context"
	"log"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// mutableRuntime is fakeRuntime with a States map a test can change while the
// client is running - the point being that the node's real state changes
// (something was deployed, something exited) without the connection dropping,
// which is exactly when a connect-time-only snapshot goes stale.
type mutableRuntime struct {
	fakeRuntime
	mu     sync.Mutex
	states map[string]engine.ContainerState
}

func (r *mutableRuntime) States(context.Context) (map[string]engine.ContainerState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]engine.ContainerState, len(r.states))
	for k, v := range r.states {
		out[k] = v
	}
	return out, nil
}

func (r *mutableRuntime) setStates(states map[string]engine.ContainerState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = states
}

// catalogResidents reads the recipe IDs sous-api currently believes are on a
// node, sorted so a comparison is stable.
func catalogResidents(cat *nodecatalog.Catalog, nodeID string) []string {
	view, ok := cat.Node(nodeID)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(view.Deployments))
	for _, d := range view.Deployments {
		out = append(out, d.RecipeId)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConnectedNodeKeepsItsCatalogEntryFresh is the regression test for a
// control plane whose view of a node was accurate only at the instant that
// node connected. souslet sent exactly ONE NodeSnapshot, at connect time, and
// never again - no ticker, no post-deploy push - so every downstream consumer
// (the capacity gate in planOnNode, the fleet cards, MarginGiB, the "weights
// cached" chips) went stale the moment anything changed on the node and
// stayed stale indefinitely.
//
// It runs the REAL client against the REAL grpcserver and asserts on what
// nodecatalog.Catalog.Node returns, changing the node's Docker state
// mid-connection with no reconnect anywhere: against the old code the
// catalog keeps reporting the connect-time residents forever.
func TestConnectedNodeKeepsItsCatalogEntryFresh(t *testing.T) {
	cat := nodecatalog.New()
	srv := grpcserver.New(cat, nil)

	lis := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	pb.RegisterSousletServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	// Capture the client's own logging so this test can prove the update
	// arrived on the SAME connection rather than via a reconnect (which
	// would resend a snapshot for unrelated reasons and make this test pass
	// for the wrong reason).
	logW := &syncWriter{}
	prev := log.Writer()
	log.SetOutput(logW)
	t.Cleanup(func() { log.SetOutput(prev) })

	rt := &mutableRuntime{states: map[string]engine.ContainerState{
		"sous-dflash2": {Name: "sous-dflash2", Status: "running"},
	}}
	c := &Client{
		Addr: "passthrough:///bufnet-resnapshot",
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		NodeID:     "asus-gx10",
		PoolGiB:    121.6,
		ReserveGiB: 24,
		// Production's default is defaultSnapshotInterval; a test should not
		// sleep that long to prove the mechanism exists.
		SnapshotInterval: 25 * time.Millisecond,
		Handlers:         &Handlers{Runtime: rt},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool {
		return equalStrings(catalogResidents(cat, "asus-gx10"), []string{"dflash2"})
	}, "the connect-time snapshot never reached the catalog")

	// A second model comes up on the node - the drag-and-drop deploy case,
	// and the exact change the capacity gate for the NEXT deploy has to see.
	rt.setStates(map[string]engine.ContainerState{
		"sous-dflash2": {Name: "sous-dflash2", Status: "running"},
		"sous-kokoro":  {Name: "sous-kokoro", Status: "running"},
	})
	waitFor(t, 5*time.Second, func() bool {
		return equalStrings(catalogResidents(cat, "asus-gx10"), []string{"dflash2", "kokoro"})
	}, "the catalog never picked up a change made while the node stayed connected")

	// And back down again: a full replace, not an accumulating merge.
	rt.setStates(map[string]engine.ContainerState{
		"sous-kokoro": {Name: "sous-kokoro", Status: "running"},
	})
	waitFor(t, 5*time.Second, func() bool {
		return equalStrings(catalogResidents(cat, "asus-gx10"), []string{"kokoro"})
	}, "the catalog never dropped a deployment that is no longer on the node")

	if logW.Contains("connection to passthrough:///bufnet-resnapshot lost") {
		t.Fatal("the connection dropped during this test - the refresh has to be proven on a CONTINUOUSLY connected node, not via a reconnect's handshake snapshot")
	}
	if view, ok := cat.Node("asus-gx10"); !ok || !view.Connected {
		t.Fatal("node should still be connected at the end of this test")
	}
}

// waitFor polls cond until it holds or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(failMsg)
}
