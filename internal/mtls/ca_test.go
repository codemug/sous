package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
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

func TestSaveAndLoadRoundTripsIssuedCerts(t *testing.T) {
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

func TestLoadCAMissingFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCA(filepath.Join(dir, "does-not-exist.json")); err == nil {
		t.Fatal("LoadCA on a missing file should return an error, not a zero-value CA")
	}
}
