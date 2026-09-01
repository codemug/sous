package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/codemug/sous/internal/mtls"
)

// nodeIDRE mirrors internal/recipe's idRE: a node ID becomes a certificate's
// CommonName and three filenames below, so it is constrained to exactly what
// is safe as both without needing a second escaping scheme for either.
var nodeIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func validNodeID(s string) bool { return nodeIDRE.MatchString(s) }

// runNodeCmd implements sous-api's node-registration admin surface (the
// design's "Node Registration" section: "an operator runs `sous-api node add
// <node-id>` ... which generates a keypair, signs it with sous-api's CA, and
// prints/writes the cert+key pair to copy onto the node"). Today this is the
// only subcommand: `sous-api node add <node-id>`.
func runNodeCmd(args []string) error {
	// -ca-state/-out MUST precede <node-id> below: flag.FlagSet.Parse stops
	// parsing flags at the first non-flag argument, so a node id given
	// before a flag would swallow the flag as a second positional argument
	// rather than an option.
	const usage = "usage: sous-api node add -ca-state <path> [-out <dir>] <node-id>"
	if len(args) == 0 || args[0] != "add" {
		return errors.New(usage)
	}

	fs := flag.NewFlagSet("sous-api node add", flag.ContinueOnError)
	caStatePath := fs.String("ca-state", "", "path to the CA state file the running sous-api server was started with (required - see loadOrCreateCA)")
	outDir := fs.String("out", ".", "directory to write ca.pem/<node-id>.cert.pem/<node-id>.key.pem into")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(usage)
	}
	nodeID := fs.Arg(0)
	if !validNodeID(nodeID) {
		return fmt.Errorf("invalid node id %q (want %s)", nodeID, nodeIDRE)
	}
	if *caStatePath == "" {
		return fmt.Errorf("-ca-state is required")
	}

	certPEM, keyPEM, caPEM, err := issueNodeCert(*caStatePath, nodeID)
	if err != nil {
		return err
	}
	paths, err := writeNodeCertFiles(*outDir, nodeID, certPEM, keyPEM, caPEM)
	if err != nil {
		return err
	}
	fmt.Printf("issued a node cert for %q, signed by %s\n\n", nodeID, *caStatePath)
	fmt.Printf("copy these onto the node and point souslet's -ca/-cert/-key at them:\n")
	fmt.Printf("  -ca   %s\n", paths.ca)
	fmt.Printf("  -cert %s\n", paths.cert)
	fmt.Printf("  -key  %s\n\n", paths.key)
	fmt.Printf("NOTE: the running sous-api server loaded its CA into memory at startup " +
		"and will not see this node as known until it is restarted (or reloads " +
		"-ca-state) - the on-disk state is updated, but the live process is not.\n")
	return nil
}

// issueNodeCert loads (never creates) the CA persisted at caStatePath and
// issues a fresh client cert for nodeID from it, then persists the CA's
// updated known-node set back to the same file.
//
// LOAD-ONLY, deliberately not loadOrCreateCA's create-if-missing behavior
// (cmd/sous-api/main.go): a fresh CA created here would sign a cert that
// verifies against nobody's trust pool but its own - a souslet using it would
// mTLS-reject against the actually-running server on every Connect, and it
// would fail that way silently rather than with an error naming the mistake.
// caStatePath must already exist; it is the running server's own state file
// (or a copy of it), the same one loadOrCreateCA loads at startup - not a new
// path the operator picks freely.
func issueNodeCert(caStatePath, nodeID string) (certPEM, keyPEM, caPEM []byte, err error) {
	ca, err := mtls.LoadCA(caStatePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load CA state %s: %w (this must be the same "+
			"-ca-state file the running sous-api server was started with)", caStatePath, err)
	}
	certPEM, keyPEM, err = ca.IssueNodeCert(nodeID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("issue cert for %s: %w", nodeID, err)
	}
	// Persist the updated known-node set immediately: IssueNodeCert only
	// updated the in-memory copy this process just loaded, and the whole
	// point of a node cert is that some OTHER process (the running
	// sous-api server, on its next restart) has to recognize it via
	// CA.IsKnown.
	if err := ca.Save(caStatePath); err != nil {
		return nil, nil, nil, fmt.Errorf("persist updated CA state %s: %w", caStatePath, err)
	}
	return certPEM, keyPEM, ca.CAPEM(), nil
}

type nodeCertPaths struct{ ca, cert, key string }

// writeNodeCertFiles writes the CA cert, the node's cert, and the node's key
// as three separate PEM files under dir - the exact three inputs souslet's
// own -ca/-cert/-key flags expect (cmd/souslet/main.go), named so a second
// node's files dropped into the same directory do not collide.
func writeNodeCertFiles(dir, nodeID string, certPEM, keyPEM, caPEM []byte) (nodeCertPaths, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nodeCertPaths{}, fmt.Errorf("create %s: %w", dir, err)
	}
	paths := nodeCertPaths{
		ca:   filepath.Join(dir, "ca.pem"),
		cert: filepath.Join(dir, nodeID+".cert.pem"),
		key:  filepath.Join(dir, nodeID+".key.pem"),
	}
	// The CA cert is public (souslet's RootCAs pool); the node cert is public
	// too (it is presented on the wire during the TLS handshake). The key is
	// the only one of the three that is a secret, and it gets the same 0o600
	// this project already uses for the CA's own on-disk state
	// (internal/mtls/ca.go's Save).
	if err := os.WriteFile(paths.ca, caPEM, 0o644); err != nil {
		return nodeCertPaths{}, fmt.Errorf("write %s: %w", paths.ca, err)
	}
	if err := os.WriteFile(paths.cert, certPEM, 0o644); err != nil {
		return nodeCertPaths{}, fmt.Errorf("write %s: %w", paths.cert, err)
	}
	if err := os.WriteFile(paths.key, keyPEM, 0o600); err != nil {
		return nodeCertPaths{}, fmt.Errorf("write %s: %w", paths.key, err)
	}
	return paths, nil
}
