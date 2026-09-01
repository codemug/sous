package grpcserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// newSousletDialer stands up one grpc.Server/ClientConn pair over bufconn and
// returns a function that opens ANOTHER Connect stream on it - so a test can
// have two connection generations for the same node ID alive at once, which
// is exactly the situation a reconnect creates.
func newSousletDialer(t *testing.T, srv *Server) func() pb.Souslet_ConnectClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	pb.RegisterSousletServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewSousletClient(conn)
	return func() pb.Souslet_ConnectClient {
		stream, err := client.Connect(context.Background())
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		return stream
	}
}

// residentRecipes reads back the recipe IDs the catalog currently believes are
// on a node - the cheapest way for a test to tell WHICH connection generation
// last wrote a snapshot, since both generations share one node ID.
func residentRecipes(cat *nodecatalog.Catalog, nodeID string) []string {
	view, ok := cat.Node(nodeID)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(view.Deployments))
	for _, d := range view.Deployments {
		out = append(out, d.RecipeId)
	}
	return out
}

// TestStaleConnectionTeardownDoesNotUnregisterAReconnectedNode reproduces the
// exact reconnect race: a node reboots (or a partition leaves the old
// server-side stream.Recv blocked - no gRPC keepalive is configured on either
// side, so this can last a long time), the node reconnects and successfully
// registers a NEW connection under the same node ID, and only THEN does the
// old stream finally error out and run its cleanup.
//
// With an unconditional `delete(s.conns, nodeID)` + MarkDisconnected in that
// cleanup, the dead connection's teardown tears down the LIVE one: the node is
// shown disconnected and is unreachable via Send/OpenProxyStream until a
// souslet or sous-api restart, with nothing actually wrong with it.
func TestStaleConnectionTeardownDoesNotUnregisterAReconnectedNode(t *testing.T) {
	const nodeID = "asus-gx10"
	cat := nodecatalog.New()
	srv := New(cat, nil)
	dial := newSousletDialer(t, srv)

	// Generation 1: the connection that is about to go stale.
	stale := dial()
	if err := stale.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: nodeID, Deployments: []*pb.DeploymentState{{RecipeId: "first-generation"}},
	}}}); err != nil {
		t.Fatalf("send snapshot (generation 1): %v", err)
	}
	waitUntilTrue(t, 2*time.Second, func() bool {
		r := residentRecipes(cat, nodeID)
		return len(r) == 1 && r[0] == "first-generation"
	}, "the first connection never registered")

	// Generation 2: the node reconnects while generation 1 is still open on
	// the server side. Its distinct snapshot is how this test knows the
	// server has finished registering it (Connect writes s.conns before it
	// ever touches the catalog).
	fresh := dial()
	if err := fresh.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: nodeID, Deployments: []*pb.DeploymentState{{RecipeId: "second-generation"}},
	}}}); err != nil {
		t.Fatalf("send snapshot (generation 2): %v", err)
	}
	waitUntilTrue(t, 2*time.Second, func() bool {
		r := residentRecipes(cat, nodeID)
		return len(r) == 1 && r[0] == "second-generation"
	}, "the reconnected connection never took over the registration")

	// The reconnected node answers commands, so a successful Send below is
	// proof the live connection is genuinely usable, not just present.
	go func() {
		for {
			env, err := fresh.Recv()
			if err != nil {
				return
			}
			if cmd := env.GetDeploy(); cmd != nil {
				_ = fresh.Send(&pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{
					DeployResult: &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: "live"},
				}})
			}
		}
	}()

	// NOW the stale connection finally dies and runs its cleanup.
	if err := stale.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	for { // drain until the server ends the stale RPC, i.e. its cleanup has run
		if _, err := stale.Recv(); err != nil {
			break
		}
	}
	// The cleanup defer runs a few instructions after the RPC ends; give it
	// room to do the damage it used to do rather than racing it.
	time.Sleep(200 * time.Millisecond)

	if !srv.Connected(nodeID) {
		t.Fatal("the stale connection's teardown unregistered the live, reconnected connection")
	}
	if view, ok := cat.Node(nodeID); !ok || !view.Connected {
		t.Fatal("the stale connection's teardown marked the live, reconnected node disconnected")
	}
	if r := residentRecipes(cat, nodeID); len(r) != 1 || r[0] != "second-generation" {
		t.Fatalf("catalog residents = %v, want the reconnected generation's snapshot", r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reply, err := srv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{
		Deploy: &pb.DeployCommand{RecipeId: "post-reconnect"},
	}})
	if err != nil {
		t.Fatalf("the reconnected node is unreachable after the stale teardown: %v", err)
	}
	if res := reply.GetDeployResult(); res == nil || res.ContainerId != "live" {
		t.Fatalf("reply = %+v, want the live connection's DeployResult", reply)
	}
}

// TestControlCommandSucceedsWhileALargeProxyBodyIsInFlight is the regression
// test for control/proxy interference. sendChunkedProxyBody emits 4096-byte
// frames, so one real upload at this fleet's own 32MB maxRequestBytes limit is
// over 8000 envelopes - more than enough to keep a 32-deep buffer saturated
// for the whole transfer. While that was true, every deploy/undeploy/fetch/
// weight-delete issued through Send failed IMMEDIATELY (its enqueue had a
// `default:` branch) with a "send queue is full" error that named nothing to
// do with the command the operator actually issued.
//
// The test deliberately saturates the proxy channel before issuing the
// control command, then lets the node resume reading: against the old
// single-channel code Send returns a "queue full" error instantly; against the
// fix the command is admitted, prioritised ahead of the remaining body frames,
// and answered.
func TestControlCommandSucceedsWhileALargeProxyBodyIsInFlight(t *testing.T) {
	const nodeID = "asus-gx10"
	cat := nodecatalog.New()
	srv := New(cat, nil)
	dial := newSousletDialer(t, srv)
	stream := dial()

	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: nodeID,
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}
	waitUntilTrue(t, 2*time.Second, func() bool { return srv.Connected(nodeID) },
		"node never showed as connected")

	// The node stops reading its stream, the way a node busy writing a large
	// upload to a local model container does. Nothing is consumed until
	// release is closed, so the transport's flow-control window fills and the
	// server's proxy frames back up in nc.send.
	release := make(chan struct{})
	go func() {
		<-release
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if cmd := env.GetDeploy(); cmd != nil {
				_ = stream.Send(&pb.Envelope{StreamId: env.StreamId, Payload: &pb.Envelope_DeployResult{
					DeployResult: &pb.DeployResult{RecipeId: cmd.RecipeId, ContainerId: "deployed-mid-upload"},
				}})
			}
		}
	}()

	ps, err := srv.OpenProxyStream(nodeID)
	if err != nil {
		t.Fatalf("OpenProxyStream: %v", err)
	}
	defer ps.Close()
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		if err := ps.Send(&pb.HTTPRequestHead{Method: "POST", Path: "/v1/audio/transcriptions"}); err != nil {
			return
		}
		// ~8MB in 4096-byte frames: the shape of a real audio upload, and far
		// more than any transport window, so the send channel genuinely
		// saturates rather than the whole body slipping through.
		frame := make([]byte, 4096)
		for i := 0; i < 2000; i++ {
			if err := ps.SendChunk(frame, false); err != nil {
				return
			}
		}
		_ = ps.SendChunk(nil, true)
	}()

	waitUntilTrue(t, 5*time.Second, func() bool {
		srv.mu.RLock()
		nc := srv.conns[nodeID]
		srv.mu.RUnlock()
		return nc != nil && len(nc.send) == cap(nc.send)
	}, "the proxy send buffer never filled - the test never reached the condition it exists to exercise")

	type result struct {
		reply *pb.Envelope
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		reply, err := srv.Send(ctx, nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{
			Deploy: &pb.DeployCommand{RecipeId: "deploy-during-upload"},
		}})
		resCh <- result{reply, err}
	}()

	// Let the command's enqueue happen against a genuinely full proxy buffer
	// (this is where the old code failed outright), then let the node start
	// draining again.
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case res := <-resCh:
		if res.err != nil {
			if strings.Contains(res.err.Error(), "queue is full") {
				t.Fatalf("a deploy issued during a large proxied upload was refused because of the UPLOAD: %v", res.err)
			}
			t.Fatalf("Send during a large proxied upload: %v", res.err)
		}
		if r := res.reply.GetDeployResult(); r == nil || r.ContainerId != "deployed-mid-upload" {
			t.Fatalf("reply = %+v, want the node's DeployResult", res.reply)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a control command issued during a large proxied upload never completed")
	}

	select {
	case <-uploadDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the proxied upload never finished")
	}
}
