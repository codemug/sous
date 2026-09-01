// Command souslet is the per-node worker: it holds no UI, no HTTP server,
// and no persistent store of its own - only a Docker engine wrapper, a
// weight-fetch manager, and a gRPC client that dials sous-api and stays
// connected for the process lifetime. Everything it needs to report is
// derived live from Docker on every (re)connect.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/grpcclient"
	"github.com/codemug/sous/internal/mtls"
	"github.com/codemug/sous/internal/ports"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	apiAddr := flag.String("api-addr", "", "sous-api's gRPC address, host:port")
	nodeID := flag.String("node-id", "", "this node's ID, must match what sous-api issued the cert for")
	modelDir := flag.String("model-dir", "", "host directory bound as the HF cache")
	caPath := flag.String("ca", "", "path to the CA cert PEM")
	certPath := flag.String("cert", "", "path to this node's issued cert PEM")
	keyPath := flag.String("key", "", "path to this node's issued key PEM")
	poolGiB := flag.Float64("pool-gib", 0, "this node's total usable memory pool")
	reserveGiB := flag.Float64("reserve-gib", 24, "GiB reserved for the OS, never committed to a deployment")
	// The port range models on THIS node are published on. Allocation
	// happens here rather than on sous-api because ports.Allocator decides
	// availability by binding, which is only meaningful on the machine the
	// container runs on - see Handlers.Ports. Defaults match sous-api's own
	// -port-low/-port-high.
	portLow := flag.Int("port-low", 18000, "low end of this node's deploy port range")
	portHigh := flag.Int("port-high", 18100, "high end of this node's deploy port range")
	bindHost := flag.String("bind-host", "127.0.0.1", "host deployed models are published on, and the host port availability is probed against")
	flag.Parse()

	for name, v := range map[string]string{"-api-addr": *apiAddr, "-node-id": *nodeID, "-model-dir": *modelDir, "-ca": *caPath, "-cert": *certPath, "-key": *keyPath} {
		if v == "" {
			log.Fatalf("%s is required", name)
		}
	}

	caPEM, err := os.ReadFile(*caPath)
	if err != nil {
		log.Fatalf("read CA: %v", err)
	}
	certPEM, err := os.ReadFile(*certPath)
	if err != nil {
		log.Fatalf("read cert: %v", err)
	}
	keyPEM, err := os.ReadFile(*keyPath)
	if err != nil {
		log.Fatalf("read key: %v", err)
	}
	tlsConfig, err := mtls.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		log.Fatalf("build TLS config: %v", err)
	}

	dockerEngine, err := engine.New("")
	if err != nil {
		log.Fatalf("connect to local Docker: %v", err)
	}
	fetchMgr := &fetch.Manager{Runtime: dockerEngine, ModelDir: *modelDir}

	client := &grpcclient.Client{
		Addr:        *apiAddr,
		DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))},
		NodeID:      *nodeID,
		PoolGiB:     *poolGiB,
		ReserveGiB:  *reserveGiB,
		Handlers: &grpcclient.Handlers{
			Runtime: dockerEngine, Fetch: fetchMgr, ModelDir: *modelDir,
			Ports:    ports.Allocator{Low: *portLow, High: *portHigh},
			BindHost: *bindHost,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log.Printf("souslet: connecting to %s as node %q", *apiAddr, *nodeID)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("souslet: %v", err)
	}
}
