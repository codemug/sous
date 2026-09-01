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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// IssueServerCert signs sous-api's OWN listener certificate - the identity
// souslet verifies during the TLS handshake, which is a fundamentally
// different kind of certificate from IssueNodeCert's.
//
// TLSConfigServer used to build the server's identity by calling
// IssueNodeCert("sous-api"), and that could never complete a handshake with
// a souslet built by ClientTLSConfig (which does full verification - no
// InsecureSkipVerify, no ServerName override). Three separate stdlib
// rejections stacked up: a node cert carries no SANs at all, so modern Go
// rejects it outright rather than falling back to the legacy CommonName
// match; and even once SANs are added, a node cert's ExtKeyUsage is
// {ClientAuth}, which x509 refuses when it is asked to verify a SERVER. So
// a server cert needs its own issuance path: ServerAuth key usage, plus real
// DNS/IP SANs covering every address a souslet might dial.
//
// hosts are the addresses this listener will be reached on - each is
// classified as an IP SAN or a DNS SAN by whether it parses as an IP.
// Loopback is always included on top of them, so a souslet running on the
// same box as sous-api can dial 127.0.0.1/localhost without the operator
// having to remember to list it.
func (c *CA) IssueServerCert(hosts ...string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: ServerCN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	tmpl.DNSNames, tmpl.IPAddresses = splitHostSANs(hosts)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign server cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal server key: %w", err)
	}
	// Deliberately NOT recorded in c.known: that set answers "is this node
	// registered with the control plane" (see IsKnown, consulted by
	// grpcserver.Connect on every node handshake), and sous-api is not a
	// node. Registering its own listener identity there would put a
	// non-node name in the fleet's registration list for no purpose.
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

// ServerCN is the CommonName on sous-api's own listener certificate. It is
// deliberately not a valid node ID (nodeIDRE in cmd/sous-api/node.go allows
// no dots), so a node can never be registered under a name that collides
// with the control plane's own identity.
const ServerCN = "sous-api.internal"

// splitHostSANs sorts hosts into DNS names and IP addresses - x509 has
// separate SAN fields for the two, and putting an IP literal in DNSNames
// makes verification of a request for that IP fail. Loopback is always
// present; duplicates are dropped so a caller passing 127.0.0.1 explicitly
// does not get it twice.
func splitHostSANs(hosts []string) (dns []string, ips []net.IP) {
	seen := make(map[string]bool)
	add := func(h string) {
		h = strings.TrimSpace(h)
		// A bracketed IPv6 literal ("[::1]") is how an address appears in a
		// host:port string; net.ParseIP wants it unbracketed.
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			return
		}
		dns = append(dns, h)
	}
	for _, h := range hosts {
		add(h)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	return dns, ips
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

// TLSConfigServer builds the listener-side TLS config: present a server
// certificate souslet can actually verify, and require and verify a client
// cert signed by this CA. Actual node-identity/revocation enforcement
// (IsKnown) happens one layer up in grpcserver, since a tls.Config's
// ClientAuth check alone can't consult per-connection state.
//
// hosts must cover every address souslets dial this listener on - in
// practice the host half of sous-api's -grpc-listen flag (see
// cmd/sous-api/main.go). souslet verifies the server certificate in full
// (mtls.ClientTLSConfig sets no InsecureSkipVerify and no ServerName
// override), so an address missing from the SAN list is an address no
// souslet can connect on.
func (c *CA) TLSConfigServer(hosts ...string) (*tls.Config, error) {
	serverCert, serverKeyPEM, err := c.IssueServerCert(hosts...)
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

// caState is the on-disk shape of a *CA: enough to reconstruct cert, key
// and the known-node set on the next LoadCA. JSON, not DER/PEM-on-disk
// directly, to match this project's general preference for readable
// on-disk state (the CA cert and node key are still opaque PEM/DER bytes
// inside it - only the wrapping is human-legible).
type caState struct {
	CertPEM []byte          `json:"cert_pem"`
	KeyDER  []byte          `json:"key_der"` // ecdsa private key, ASN.1 DER (x509.MarshalECPrivateKey)
	Known   map[string]bool `json:"known"`
}

// Save persists the CA's cert+key and known-node set to path so a restart
// does not have to (and must not) regenerate the CA: a fresh CA would
// invalidate every already-issued node cert, disconnecting every souslet
// until each is reissued by hand. The file carries private key material,
// so it is written 0o600.
//
// WRITTEN ATOMICALLY - temp file in the same directory, then os.Rename.
// A plain os.WriteFile truncates in place, so a crash or a full disk
// halfway through would leave a truncated ca-state.json; LoadCA then fails
// to parse it and cmd/sous-api's loadOrCreateCA turns that into a
// log.Fatalf. That is not merely a failed start: the CA's own key material
// would be gone with the file, permanently invalidating every node cert
// ever issued from it. os.Rename on the same filesystem is atomic, so a
// crash leaves either the previous complete file or the new complete one,
// never a hybrid.
func (c *CA) Save(path string) error {
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return fmt.Errorf("marshal CA key: %w", err)
	}
	c.mu.Lock()
	known := make(map[string]bool, len(c.known))
	for k, v := range c.known {
		known[k] = v
	}
	c.mu.Unlock()
	data, err := json.Marshal(caState{CertPEM: c.certPEM, KeyDER: keyDER, Known: known})
	if err != nil {
		return fmt.Errorf("marshal CA state: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// writeFileAtomic writes data to path via a temp file in the SAME directory
// (a temp file elsewhere - /tmp, say - could be on another filesystem,
// where os.Rename is not atomic and may not even work) and renames it into
// place. The temp file is created with the final mode, so the key material
// is never briefly world-readable, and is removed on any failure path so a
// failed save does not litter the data directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below has succeeded
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Sync before rename: without it the rename can be durable while the
	// data it points at is not, which on a crash produces exactly the
	// truncated/empty file this whole function exists to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s into place at %s: %w", tmpName, path, err)
	}
	return nil
}

// LoadCA reconstructs a *CA previously written by Save. The returned CA
// signs with the same key as the original, so certs it already issued
// keep verifying, and issues new certs that verify against the original's
// cert pool too - it is the same CA, not a new one with the same shape.
func LoadCA(path string) (*CA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA state: %w", err)
	}
	var st caState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse CA state: %w", err)
	}
	key, err := x509.ParseECPrivateKey(st.KeyDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	block, _ := pem.Decode(st.CertPEM)
	if block == nil {
		return nil, fmt.Errorf("invalid stored CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	known := st.Known
	if known == nil {
		known = make(map[string]bool)
	}
	return &CA{cert: cert, certPEM: st.CertPEM, key: key, known: known}, nil
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
