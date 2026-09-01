package grpcserver

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

	// srv.Send fails fast if the node isn't registered in s.conns yet (by
	// design - see the "fail fast, don't buffer" comment on Send). The
	// client's stream.Send above only hands the initial snapshot to the
	// local transport; it does not wait for the server's Connect goroutine
	// to finish registering the connection. Retry briefly rather than
	// racing that registration.
	var reply *pb.Envelope
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		reply, err = srv.Send("asus-gx10", &pb.Envelope{Payload: &pb.Envelope_Deploy{Deploy: &pb.DeployCommand{RecipeId: "dflash2"}}})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Send: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	res := reply.GetDeployResult()
	if res == nil || res.ContainerId != "abc123" {
		t.Fatalf("got %+v, want DeployResult{ContainerId: abc123}", reply)
	}
}

// TestSendUnblocksWithErrorWhenNodeDisconnectsMidWait guards against the
// second half of the reply-wait leak: a Send call that has already handed
// its envelope off (so it's sitting in the second select, blocked on
// <-waiter) when the node disconnects. Before the fix this select only had
// <-waiter and a dead context.Background().Done() branch, so it would
// block forever - leaking both the calling goroutine and, via the stale
// nc.pending[stream_id] entry, the whole nodeConn (its maps, channels,
// buffered envelopes) it was blocked against. This is not a rare edge
// case: it's a command legitimately in flight when the node it was sent
// to drops, in a system whose whole premise is nodes reconnecting.
func TestSendUnblocksWithErrorWhenNodeDisconnectsMidWait(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat)
	stream := dialFakeSouslet(t, srv)

	const nodeID = "mid-wait-node"
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{NodeId: nodeID}}}); err != nil {
		t.Fatalf("Send snapshot: %v", err)
	}
	waitUntilTrue(t, 2*time.Second, func() bool {
		view, ok := cat.Node(nodeID)
		return ok && view.Connected
	}, fmt.Sprintf("node %q never showed as connected", nodeID))

	// Deliberately do not drive a reply loop for the fake souslet: nothing
	// is ever going to answer the DeployCommand below, so srv.Send is
	// genuinely stuck on <-waiter, not racing an incoming reply.
	type result struct {
		reply *pb.Envelope
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		reply, err := srv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{Deploy: &pb.DeployCommand{RecipeId: "never-answered"}}})
		resCh <- result{reply, err}
	}()

	// Give Send time to clear the enqueue select and reach the reply-wait
	// select before disconnecting - both steps are local channel/mutex
	// operations with no I/O, so this settles in microseconds; 100ms is
	// generous headroom, not a tight race.
	time.Sleep(100 * time.Millisecond)

	// Disconnect: half-close from the client, which drives the server's
	// read loop to observe io.EOF and tear the connection down - the exact
	// path that used to leave this Send call blocked forever.
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	select {
	case res := <-resCh:
		if res.err == nil {
			t.Fatalf("Send returned no error after the node disconnected mid-wait; got reply %+v", res.reply)
		}
		if !strings.Contains(res.err.Error(), "disconnected while waiting for reply") {
			t.Fatalf("Send returned an error, but not the expected one: %v", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not unblock within 2s of the node disconnecting while its reply was pending - this is the leak this test guards against")
	}

	// The node should also settle into the catalog as disconnected -
	// confirms this really was the normal teardown path, not some other
	// error shortcut.
	waitUntilTrue(t, 2*time.Second, func() bool {
		view, ok := cat.Node(nodeID)
		return ok && !view.Connected
	}, fmt.Sprintf("node %q never showed as disconnected", nodeID))
}

// TestConnectDoesNotLeakGoroutinesOnDisconnect guards against the write-loop
// goroutine inside Connect never exiting when the *read* loop is the one
// that notices the stream died (the common case: the client hangs up, the
// server observes it via stream.Recv returning io.EOF). The write loop
// previously had no way to learn about that and would block forever on the
// now-orphaned nc.send channel - once per disconnect, forever, in a system
// whose whole premise is nodes connecting and disconnecting repeatedly.
func TestConnectDoesNotLeakGoroutinesOnDisconnect(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat)

	// One grpc.Server/ClientConn for the whole test, reused across cycles -
	// each opens its own new Connect stream over it. Standing up a fresh
	// server+conn per cycle would swamp the goroutine count with transport
	// setup/teardown noise unrelated to the thing under test.
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

	const cycles = 60
	baseline := settledGoroutines(t)

	for i := 0; i < cycles; i++ {
		nodeID := fmt.Sprintf("leak-node-%d", i)
		stream, err := client.Connect(context.Background())
		if err != nil {
			t.Fatalf("Connect (cycle %d): %v", i, err)
		}
		if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{NodeId: nodeID}}}); err != nil {
			t.Fatalf("Send snapshot (cycle %d): %v", i, err)
		}

		// Wait for the server's Connect handler to actually register this
		// node - its two inner goroutines (the ones under test) aren't
		// running until it does.
		waitUntilTrue(t, 2*time.Second, func() bool {
			view, ok := cat.Node(nodeID)
			return ok && view.Connected
		}, fmt.Sprintf("node %q never showed as connected", nodeID))

		// Simulate the node disconnecting: half-close from the client side.
		// This drives the server's read loop to observe io.EOF - exactly
		// the teardown path that used to leave the write loop orphaned.
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("CloseSend (cycle %d): %v", i, err)
		}
		for { // drain until the server ends the RPC on its side too
			if _, err := stream.Recv(); err != nil {
				break
			}
		}

		waitUntilTrue(t, 2*time.Second, func() bool {
			view, ok := cat.Node(nodeID)
			return ok && !view.Connected
		}, fmt.Sprintf("node %q never showed as disconnected", nodeID))
	}

	after := settledGoroutines(t)
	t.Logf("goroutines: baseline=%d after=%d cycles=%d delta=%d", baseline, after, cycles, after-baseline)
	// Slack covers one-time transport setup (observed: a constant +5,
	// independent of cycle count - confirmed by running this same test at
	// cycles=20 and cycles=60 and seeing an identical delta both times)
	// plus incidental background goroutines (GC workers etc.). The leak
	// this guards against grows by ~1 goroutine per cycle (60 here), so
	// any real regression blows straight past this budget.
	const slack = 10
	if after > baseline+slack {
		t.Fatalf("goroutine count grew from %d to %d over %d connect/disconnect cycles (slack %d) - suspected leaked write-loop goroutine", baseline, after, cycles, slack)
	}
}

// TestSendDoesNotPanicWhenRacingDisconnect fires a burst of concurrent
// (*Server).Send calls against a node while the client half of its
// connection is torn down mid-flight. This is the exact scenario the fix
// for the write-loop leak had to stay safe under: Send and Connect's
// cleanup both touch nc.send and nc.done from different goroutines, and
// the wrong fix (closing nc.send directly) would panic here with "send on
// closed channel." Every Send must either return normally (success or a
// clean error) or, if it loses the race and its envelope never gets
// delivered/replied to, simply block - Send has no cancellation of its own
// yet (it blocks on a bare context.Background(), unrelated to this fix and
// already tracked as future work in Task 10's ctx-plumbing change), so a
// goroutine hanging here is expected and not what this test checks. What
// it checks is panics: a panic in any of the spawned goroutines fails the
// test via recover() instead of silently crashing the whole test binary,
// so a regression in the fix is visible as a normal, readable test
// failure rather than a process crash.
func TestSendDoesNotPanicWhenRacingDisconnect(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat)
	stream := dialFakeSouslet(t, srv)

	const nodeID = "race-node"
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{NodeId: nodeID}}}); err != nil {
		t.Fatalf("Send snapshot: %v", err)
	}
	waitUntilTrue(t, 2*time.Second, func() bool {
		view, ok := cat.Node(nodeID)
		return ok && view.Connected
	}, fmt.Sprintf("node %q never showed as connected", nodeID))

	// Echo replies for whatever DeployCommands do make it through before
	// teardown, so Sends that win the race complete normally instead of
	// adding to the "expected to hang" pile.
	go func() {
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if cmd := env.GetDeploy(); cmd != nil {
				_ = stream.Send(&pb.Envelope{
					StreamId: env.StreamId,
					Payload:  &pb.Envelope_DeployResult{DeployResult: &pb.DeployResult{RecipeId: cmd.RecipeId}},
				})
			}
		}
	}()

	const concurrency = 50
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Send panicked (goroutine %d): %v", i, r)
				}
			}()
			_, _ = srv.Send(nodeID, &pb.Envelope{Payload: &pb.Envelope_Deploy{Deploy: &pb.DeployCommand{RecipeId: fmt.Sprintf("recipe-%d", i)}}})
		}(i)
	}

	// Tear the connection down while those Sends are in flight - this is
	// what races nc.done closing against concurrent writers of nc.send.
	_ = stream.CloseSend()

	// Give every spawned goroutine a bounded window to run (and, if it
	// were going to, panic) rather than wg.Wait()-ing unboundedly: some of
	// them are expected to be left blocked forever on Send's reply wait
	// per the doc comment above, which is a separate, already-known,
	// out-of-scope issue, not a hang this test should itself get stuck on.
	waitCh := make(chan struct{})
	go func() { wg.Wait(); close(waitCh) }()
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
	}
}

// settledGoroutines samples runtime.NumGoroutine() a few times with GC and
// short sleeps in between, so goroutines that are in the process of exiting
// (but haven't been descheduled yet) don't inflate a one-shot reading.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	var n int
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(15 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

func waitUntilTrue(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(failMsg)
}
