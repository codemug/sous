package mtls

import (
	"crypto/tls"
	"crypto/x509"
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
