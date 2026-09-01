// Package mtls issues short-lived-infrastructure-scale client certificates
// for souslets to authenticate to sous-api with, signed by a CA sous-api
// generates and owns itself. No external CA, no rotation automation in
// this version - certs are treated as long-lived, reissued by hand on
// revocation.
package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"
)

type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey

	mu    sync.Mutex
	known map[string]bool // node IDs with a currently-valid issued cert
}

func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sous-api node CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	return &CA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		known:   make(map[string]bool),
	}, nil
}

func (c *CA) CAPEM() []byte { return c.certPEM }

// IssueNodeCert signs a fresh client certificate for nodeID, valid for
// client auth only. The node's ID becomes the certificate's CommonName -
// grpcserver reads it back out of the verified peer chain to know which
// node just connected.
func (c *CA) IssueNodeCert(nodeID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign node cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal node key: %w", err)
	}
	c.mu.Lock()
	c.known[nodeID] = true
	c.mu.Unlock()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

func (c *CA) Revoke(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.known, nodeID)
}

func (c *CA) IsKnown(nodeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known[nodeID]
}

// TLSConfigServer builds the listener-side TLS config: require and verify
// a client cert signed by this CA. Actual node-identity/revocation
// enforcement (IsKnown) happens one layer up in grpcserver, since a
// tls.Config's ClientAuth check alone can't consult per-connection state.
func (c *CA) TLSConfigServer() (*tls.Config, error) {
	serverCert, serverKeyPEM, err := c.IssueNodeCert("sous-api")
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(serverCert, serverKeyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.certPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// ClientTLSConfig builds souslet's dial-side TLS config from the CA cert
// and this node's issued cert+key (all handed to souslet out of band, the
// same way this fleet already distributes onboarding material).
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load node cert/key: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
	}, nil
}
