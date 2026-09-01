package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealTLSHandshakeBetweenSousletAndSousAPI is the test whose absence let a
// completely broken mTLS handshake ship: every other test in this package
// verifies certificates in isolation (x509.Verify with an explicitly chosen
// KeyUsage), and every gRPC test in this repo dials over bufconn with
// insecure.NewCredentials(), so nothing anywhere actually performed a TLS
// handshake between a ClientTLSConfig client and a TLSConfigServer server.
//
// It fails against the pre-fix code three times over: TLSConfigServer built
// its own identity from IssueNodeCert, which produces a certificate with no
// SANs at all (modern Go rejects the legacy CommonName fallback outright) and
// with ExtKeyUsage {ClientAuth}, which x509 refuses when verifying a SERVER.
// This is a real tls.Listen/tls.Dial round trip over a real loopback socket -
// nothing stubbed, nothing skipped - and it exercises BOTH directions of
// mutual auth: the client verifies the server's certificate, and the server
// requires and verifies the client's.
func TestRealTLSHandshakeBetweenSousletAndSousAPI(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	nodeCert, nodeKey, err := ca.IssueNodeCert("asus-gx10")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}

	// 127.0.0.1 is the address the client dials below, so it is the address
	// the server's certificate has to be valid for - exactly the relationship
	// sous-api's -grpc-listen host has with souslet's -api-addr in production.
	serverTLS, err := ca.TLSConfigServer("127.0.0.1")
	if err != nil {
		t.Fatalf("TLSConfigServer: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		cn  string
		err error
	}
	accCh := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accCh <- accepted{err: err}
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			accCh <- accepted{err: err}
			return
		}
		chains := tc.ConnectionState().VerifiedChains
		if len(chains) == 0 || len(chains[0]) == 0 {
			accCh <- accepted{err: fmt.Errorf("server saw no verified client chain")}
			return
		}
		// Write one byte so the client's Read below only returns once this
		// side has genuinely finished the handshake.
		_, _ = tc.Write([]byte("k"))
		accCh <- accepted{cn: chains[0][0].Subject.CommonName}
	}()

	clientTLS, err := ClientTLSConfig(ca.CAPEM(), nodeCert, nodeKey)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("souslet could not complete the TLS handshake against sous-api: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("reading after the handshake: %v", err)
	}

	select {
	case acc := <-accCh:
		if acc.err != nil {
			t.Fatalf("server side of the handshake failed: %v", acc.err)
		}
		if acc.cn != "asus-gx10" {
			t.Fatalf("server saw client CommonName %q, want asus-gx10", acc.cn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported the accepted connection")
	}
}

// TestHandshakeFailsForAnAddressNotInTheServerCert is the negative half of the
// test above: the SAN list is doing real work, not decoration. A souslet
// pointed at an address sous-api's certificate does not cover must be refused,
// not silently accepted.
func TestHandshakeFailsForAnAddressNotInTheServerCert(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	nodeCert, nodeKey, err := ca.IssueNodeCert("asus-gx10")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	// Issued for a name this listener is NOT reached by; 127.0.0.1 is always
	// added on top, so dial by a hostname alias for loopback instead.
	serverTLS, err := ca.TLSConfigServer("some-other-host.internal")
	if err != nil {
		t.Fatalf("TLSConfigServer: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	clientTLS, err := ClientTLSConfig(ca.CAPEM(), nodeCert, nodeKey)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	// ServerName forces verification against a name that is not in the SANs
	// without needing DNS: the same check a souslet dialing an uncovered
	// address performs.
	clientTLS.ServerName = "not-in-the-cert.internal"
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded against a server certificate with no SAN for the dialed name")
	}
}

// TestServerCertCarriesServerAuthAndSANs pins the specific properties the
// handshake above depends on, so a regression reports WHICH property was lost
// rather than only "handshake failed".
func TestServerCertCarriesServerAuthAndSANs(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, _, err := ca.IssueServerCert("10.0.0.5", "sous-api.tail1234.ts.net")
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	var serverAuth bool
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Fatalf("server cert ExtKeyUsage = %v, want it to include ServerAuth", leaf.ExtKeyUsage)
	}
	// An IP literal must land in IPAddresses, not DNSNames: a client dialing
	// 10.0.0.5 checks the IP SANs, and an IP parked in DNSNames never matches.
	var sawIP bool
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "10.0.0.5" {
			sawIP = true
		}
	}
	if !sawIP {
		t.Fatalf("IPAddresses = %v, want it to include 10.0.0.5", leaf.IPAddresses)
	}
	var sawDNS, sawLoopback bool
	for _, n := range leaf.DNSNames {
		if n == "sous-api.tail1234.ts.net" {
			sawDNS = true
		}
		if n == "localhost" {
			sawLoopback = true
		}
	}
	if !sawDNS {
		t.Fatalf("DNSNames = %v, want it to include sous-api.tail1234.ts.net", leaf.DNSNames)
	}
	if !sawLoopback {
		t.Fatalf("DNSNames = %v, want localhost always included so a same-box souslet can dial", leaf.DNSNames)
	}
	// The control plane's own identity must not be registered as a NODE:
	// known is the fleet's node-registration set, which grpcserver.Connect
	// consults on every node handshake.
	if ca.IsKnown(ServerCN) {
		t.Fatalf("issuing the server cert registered %q as a known node", ServerCN)
	}
}

// TestSaveIsAtomicAndLeavesNoPartialFile proves Save never writes through the
// destination path itself: it renames a complete temp file into place. The CA
// state is the single most critical piece of persisted state in this system -
// a truncated ca-state.json both stops sous-api from starting and destroys the
// key material behind every node certificate ever issued from it.
func TestSaveIsAtomicAndLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca-state.json")

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

	// Make the destination itself unwritable in place. An in-place
	// os.WriteFile fails here (having, in production, already truncated an
	// existing file in the equivalent crash case); a temp-file-plus-rename
	// succeeds, because rename replaces the directory entry rather than
	// opening the existing file for writing.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, err := ca.IssueNodeCert("aorus-ubuntu"); err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	if err := ca.Save(path); err != nil {
		t.Fatalf("Save over a read-only destination must still succeed via rename: %v", err)
	}
	reloaded, err := LoadCA(path)
	if err != nil {
		t.Fatalf("LoadCA after the second Save: %v", err)
	}
	if !reloaded.IsKnown("aorus-ubuntu") || !reloaded.IsKnown("asus-gx10") {
		t.Fatal("the rewritten state lost a known node")
	}

	// Nothing left behind: a save must not litter the data directory with
	// temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "ca-state.json" {
			t.Fatalf("Save left a stray file behind: %s", e.Name())
		}
	}
}
