package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/codemug/sous/internal/mtls"
)

// newTestCAState writes a fresh CA to a state file under t.TempDir and
// returns its path - the on-disk shape issueNodeCert expects to load, the
// same shape loadOrCreateCA (main.go) produces for the running server.
func newTestCAState(t *testing.T) string {
	t.Helper()
	ca, err := mtls.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca-state.json")
	if err := ca.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

// TestIssueNodeCertIsRecognizedByTheCA is the core correctness property this
// subcommand exists for: a cert it issues must be signed by, and recorded as
// known against, the SAME CA the running sous-api server trusts - not a
// freshly generated one - so a souslet using it actually connects.
func TestIssueNodeCertIsRecognizedByTheCA(t *testing.T) {
	path := newTestCAState(t)

	certPEM, keyPEM, caPEM, err := issueNodeCert(path, "asus-gx10")
	if err != nil {
		t.Fatalf("issueNodeCert: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 || len(caPEM) == 0 {
		t.Fatalf("issueNodeCert returned empty PEM: cert=%d key=%d ca=%d",
			len(certPEM), len(keyPEM), len(caPEM))
	}

	// The mutation (the new node added to the known set) must have been
	// persisted back to path - reload a SEPARATE *CA from disk, the same way
	// a restarted sous-api server would, and check it, not the in-memory one
	// issueNodeCert already returned success against.
	reloaded, err := mtls.LoadCA(path)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !reloaded.IsKnown("asus-gx10") {
		t.Error("reloaded CA does not recognize the node issueNodeCert just issued a cert for - " +
			"the known-set update was not persisted")
	}
	if reloaded.IsKnown("some-other-node") {
		t.Error("reloaded CA reports an unrelated node id as known")
	}
}

// TestIssuedCertParsesAndMatchesTheCA checks the artifact itself, not just
// the CA's bookkeeping: a valid x509 cert, CN set to the node id (grpcserver
// reads the CN back out of the peer chain to know which node connected - see
// mtls.CA.IssueNodeCert's own doc comment), and a cert+key pair that
// actually loads as a TLS credential.
func TestIssuedCertParsesAndMatchesTheCA(t *testing.T) {
	path := newTestCAState(t)

	certPEM, keyPEM, caPEM, err := issueNodeCert(path, "aorus-ubuntu")
	if err != nil {
		t.Fatalf("issueNodeCert: %v", err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("cert/key do not form a valid TLS pair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	if leaf.Subject.CommonName != "aorus-ubuntu" {
		t.Errorf("cert CommonName = %q, want %q", leaf.Subject.CommonName, "aorus-ubuntu")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("returned CA PEM did not parse")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("issued cert does not verify against the returned CA PEM: %v", err)
	}
}

// TestIssueNodeCertRefusesAMissingCAState guards the load-only contract: a
// typo'd or not-yet-created -ca-state path must fail loudly, not silently
// mint a brand-new CA that a souslet signed against it would never verify
// against the real running server.
func TestIssueNodeCertRefusesAMissingCAState(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, _, _, err := issueNodeCert(missing, "asus-gx10"); err == nil {
		t.Fatal("expected an error for a nonexistent -ca-state path, got nil")
	}
}

// TestWriteNodeCertFilesWritesValidPEM checks the on-disk artifacts an
// operator actually copies onto a node: three files, the key private
// (0o600), all three round-tripping into the exact bytes issued.
func TestWriteNodeCertFilesWritesValidPEM(t *testing.T) {
	path := newTestCAState(t)
	certPEM, keyPEM, caPEM, err := issueNodeCert(path, "asus-gx10")
	if err != nil {
		t.Fatalf("issueNodeCert: %v", err)
	}

	dir := t.TempDir()
	paths, err := writeNodeCertFiles(dir, "asus-gx10", certPEM, keyPEM, caPEM)
	if err != nil {
		t.Fatalf("writeNodeCertFiles: %v", err)
	}

	for _, tc := range []struct {
		path string
		want []byte
	}{
		{paths.ca, caPEM},
		{paths.cert, certPEM},
		{paths.key, keyPEM},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if block, _ := pem.Decode(got); block == nil {
			t.Errorf("%s does not contain valid PEM", tc.path)
		}
		if string(got) != string(tc.want) {
			t.Errorf("%s content does not match what was issued", tc.path)
		}
	}

	info, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 0600 (it carries private key material)", perm)
	}
}

// TestValidNodeID matches the same "safe as a filename, a container name and
// a cert CommonName" constraint internal/recipe.ValidID enforces for recipe
// ids, since a node id becomes exactly those three things too (see
// writeNodeCertFiles' filenames and IssueNodeCert's CommonName).
func TestValidNodeID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"asus-gx10", true},
		{"aorus-ubuntu", true},
		{"a", true},
		{"", false},
		{"-leading-dash", false},
		{"Has-Upper", false},
		{"has space", false},
		{"has/slash", false},
		{"../traversal", false},
	} {
		if got := validNodeID(tc.id); got != tc.want {
			t.Errorf("validNodeID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestRunNodeCmdEndToEnd exercises the actual CLI entry point
// (runNodeCmd, "sous-api node add <id> -ca-state ... -out ...") rather than
// only its internal pieces, so a wiring mistake in flag parsing or dispatch
// is caught even if the pieces it calls are individually correct.
func TestRunNodeCmdEndToEnd(t *testing.T) {
	caPath := newTestCAState(t)
	outDir := t.TempDir()

	if err := runNodeCmd([]string{"add", "-ca-state", caPath, "-out", outDir, "asus-gx10"}); err != nil {
		t.Fatalf("runNodeCmd: %v", err)
	}

	for _, name := range []string{"ca.pem", "asus-gx10.cert.pem", "asus-gx10.key.pem"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}

	reloaded, err := mtls.LoadCA(caPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !reloaded.IsKnown("asus-gx10") {
		t.Error("CA state on disk does not know about the node runNodeCmd just added")
	}
}

// TestRunNodeCmdRejectsBadUsage checks the error paths a mistyped invocation
// hits, so a missing subcommand, missing node id, or missing -ca-state fails
// with a usage error instead of doing something unintended.
func TestRunNodeCmdRejectsBadUsage(t *testing.T) {
	caPath := newTestCAState(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"unknown subcommand", []string{"remove", "asus-gx10"}},
		{"no node id", []string{"add", "-ca-state", caPath}},
		{"missing ca-state", []string{"add", "asus-gx10"}},
		{"invalid node id", []string{"add", "-ca-state", caPath, "Not Valid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runNodeCmd(tc.args); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}
