package httpapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codemug/sous/internal/grpcserver"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// dialFakeSousletServingAModel attaches a fake souslet to gsrv that reports
// recipeID as deployed and answers any proxied request with 200/"served-by-
// node". Unlike dialFakeSousletRecording (one reply per envelope), a proxied
// request needs a head plus one or more chunks, so this drives that
// multi-message shape.
func dialFakeSousletServingAModel(t *testing.T, gsrv *grpcserver.Server, nodeID, recipeID string) func() {
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
	stream, err := pb.NewSousletClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: nodeID, PoolGib: 121.6, ReserveGib: 24,
		Deployments: []*pb.DeploymentState{{RecipeId: recipeID, HostPort: 18000, Phase: "running"}},
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			env, err := stream.Recv()
			if err != nil {
				return
			}
			if env.GetHttpReqHead() == nil {
				continue // the body chunk that follows the head; nothing to answer
			}
			sid := env.StreamId
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespHead{
				HttpRespHead: &pb.HTTPResponseHead{Status: 200},
			}})
			_ = stream.Send(&pb.Envelope{StreamId: sid, Payload: &pb.Envelope_HttpRespChunk{
				HttpRespChunk: &pb.HTTPResponseChunk{Data: []byte("served-by-node"), Eof: true},
			}})
		}
	}()

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
		<-done
	}
}

// TestInferenceRequestReachesAModelRunningOnANode is the wiring test that was
// missing: gateway.Gateway's multi-node branch was fully implemented and
// tested in isolation, but New() never set Nodes/GRPC on the Gateway it
// actually builds, so in the shipped binary that branch was unreachable and
// every inference request for a model running on a connected node fell
// through to the local deploy.Manager and 404'd.
//
// This builds a REAL Server through New() - the same call cmd/sous-api makes -
// attaches a fake souslet reporting a deployed model, and drives an actual
// POST /v1/chat/completions through the whole handler chain.
func TestInferenceRequestReachesAModelRunningOnANode(t *testing.T) {
	h, _, gsrv := newTestServerWithGRPC(t)
	stop := dialFakeSousletServingAModel(t, gsrv, "asus-gx10", "kokoro")
	defer stop()

	rr := post(t, h, "/v1/chat/completions", "application/json", `{"model":"kokoro"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the request must reach the node running the model): %s", rr.Code, rr.Body)
	}
	if rr.Body.String() != "served-by-node" {
		t.Fatalf("body = %q, want the node's answer", rr.Body.String())
	}
}

// TestInferenceForAModelNoNodeRunsStillUsesTheLocalPath guards the other half
// of the same wiring: sous-api still carries a local deploy.Manager during the
// migration (see cmd/sous-api's package doc), so setting Nodes/GRPC must not
// hijack every request away from it. A model no connected node reports has to
// keep getting the local path's own answer - here, its 404 naming what IS
// deployed, rather than the node path's "no connected node is running it".
func TestInferenceForAModelNoNodeRunsStillUsesTheLocalPath(t *testing.T) {
	h, _, gsrv := newTestServerWithGRPC(t)
	stop := dialFakeSousletServingAModel(t, gsrv, "asus-gx10", "kokoro")
	defer stop()

	rr := post(t, h, "/v1/chat/completions", "application/json", `{"model":"qwen38"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body)
	}
	// The local path's 404 names what is deployed locally ("nothing is
	// deployed" here); the node path's says "no connected node is running".
	// Asserting on which one answered is what proves the fallback works.
	if body := rr.Body.String(); !strings.Contains(body, "nothing is deployed") {
		t.Fatalf("the local-forward path did not answer this request: %s", body)
	}
}
