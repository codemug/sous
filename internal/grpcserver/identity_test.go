package grpcserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/codemug/sous/internal/mtls"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// startRealMTLSServer stands up this package's Server behind an ACTUAL mTLS
// listener on loopback - a real TCP socket, real certificates, real
// handshake - rather than the bufconn+insecure.NewCredentials() setup every
// other test in this repo uses. That setup is structurally blind to
// everything the identity checks depend on: it can present no peer
// certificate at all, so a Connect handler could ignore peer identity
// entirely and still pass every one of those tests.
func startRealMTLSServer(t *testing.T, srv *Server, ca *mtls.CA) string {
	t.Helper()
	tlsCfg, err := ca.TLSConfigServer("127.0.0.1")
	if err != nil {
		t.Fatalf("TLSConfigServer: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	pb.RegisterSousletServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// dialAsNode dials addr the way cmd/souslet does: mtls.ClientTLSConfig over
// the node's issued cert/key, full server verification, no ServerName
// override.
func dialAsNode(t *testing.T, addr string, ca *mtls.CA, certPEM, keyPEM []byte) pb.Souslet_ConnectClient {
	t.Helper()
	clientTLS, err := mtls.ClientTLSConfig(ca.CAPEM(), certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := pb.NewSousletClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return stream
}

// TestARegisteredNodeConnectsOverRealMTLS is the positive control for the two
// rejection tests below (and, incidentally, the only place in this repo where
// souslet's real dial path and sous-api's real listener path meet): a node
// whose certificate CommonName matches the ID it claims, and which the CA has
// registered, must connect normally and land in the catalog.
func TestARegisteredNodeConnectsOverRealMTLS(t *testing.T) {
	ca, err := mtls.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueNodeCert("asus-gx10")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	cat := nodecatalog.New()
	addr := startRealMTLSServer(t, New(cat, ca), ca)

	stream := dialAsNode(t, addr, ca, certPEM, keyPEM)
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: "asus-gx10", PoolGib: 121.6,
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}
	waitUntilTrue(t, 5*time.Second, func() bool {
		view, ok := cat.Node("asus-gx10")
		return ok && view.Connected
	}, "a properly registered node never showed as connected over real mTLS")
}

// TestANodeCannotClaimAnotherNodesID is the impersonation case. Every node in
// the fleet holds a certificate this CA signed, so mTLS alone proves only
// "some registered node"; without checking the peer certificate's CommonName
// against the claimed node_id, any node could claim any OTHER node's ID -
// evicting the real node from the connection map and receiving its deploys
// and its proxied inference traffic.
func TestANodeCannotClaimAnotherNodesID(t *testing.T) {
	ca, err := mtls.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueNodeCert("asus-gx10")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	// The victim is registered too, so this is specifically about identity,
	// not about the impersonated ID being unknown.
	if _, _, err := ca.IssueNodeCert("aorus-ubuntu"); err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	cat := nodecatalog.New()
	srv := New(cat, ca)
	addr := startRealMTLSServer(t, srv, ca)

	stream := dialAsNode(t, addr, ca, certPEM, keyPEM)
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: "aorus-ubuntu", // NOT this certificate's CommonName
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("a node presenting asus-gx10's certificate was allowed to register as aorus-ubuntu")
	}
	if !strings.Contains(err.Error(), "claims to be") {
		t.Fatalf("connection was rejected, but not as an identity mismatch: %v", err)
	}
	if srv.Connected("aorus-ubuntu") {
		t.Fatal("the impersonated node ID was registered as a live connection")
	}
	if _, ok := cat.Node("aorus-ubuntu"); ok {
		t.Fatal("the impersonated node ID reached the node catalog")
	}
}

// TestARevokedNodeIsRefused is the revocation case. Before this, CA.Revoke had
// no production call site at all: revoking a decommissioned node changed
// nothing, and that node kept full control-plane access - deploys, undeploys,
// weight deletion, proxied inference - for as long as it cared to reconnect.
func TestARevokedNodeIsRefused(t *testing.T) {
	ca, err := mtls.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueNodeCert("decommissioned")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	ca.Revoke("decommissioned")

	cat := nodecatalog.New()
	srv := New(cat, ca)
	addr := startRealMTLSServer(t, srv, ca)

	stream := dialAsNode(t, addr, ca, certPEM, keyPEM)
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: "decommissioned",
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a revoked node's certificate still bought a working connection")
	} else if !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("connection was rejected, but not as an unregistered node: %v", err)
	}
	if srv.Connected("decommissioned") {
		t.Fatal("a revoked node was registered as a live connection")
	}
}

// fakeAuthority is a NodeAuthority with no certificate machinery behind it,
// for the plaintext case below where no certificate exists to check.
type fakeAuthority struct{ known map[string]bool }

func (f fakeAuthority) IsKnown(nodeID string) bool { return f.known[nodeID] }

// TestAPlaintextConnectionIsRefusedWhenAnAuthorityIsConfigured closes the
// obvious bypass: if identity enforcement were skipped whenever no peer
// certificate is present, an attacker who reached the listener without TLS
// would be MORE privileged than one holding a revoked certificate. A Server
// built with an authority must refuse any connection it cannot identify.
func TestAPlaintextConnectionIsRefusedWhenAnAuthorityIsConfigured(t *testing.T) {
	cat := nodecatalog.New()
	srv := New(cat, fakeAuthority{known: map[string]bool{"asus-gx10": true}})

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
	stream, err := pb.NewSousletClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&pb.Envelope{Payload: &pb.Envelope_Snapshot{Snapshot: &pb.NodeSnapshot{
		NodeId: "asus-gx10",
	}}}); err != nil {
		t.Fatalf("send snapshot: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a plaintext connection registered a node on a Server with an authority configured")
	}
	if srv.Connected("asus-gx10") {
		t.Fatal("a plaintext connection was registered as a live node connection")
	}
}
