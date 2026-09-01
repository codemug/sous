package grpcserver

import (
	"context"
	"net"
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
