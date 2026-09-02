// Command sous-api is the control plane: the recipe catalog, the node
// catalog, the UI, and the gRPC server every souslet dials into.
//
// This is the migration-period binary described in
// docs/superpowers/specs/2026-09-01-sous-multinode-design.md's "Migration /
// Rollout" section: the existing single-node deploy path (a local
// deploy.Manager talking straight to Docker on this box) is landed here
// unchanged, alongside the new node-scoped path that routes to a connected
// souslet over gRPC. Both live side by side today.
//
// NOT YET DONE: the design's step 6 ("delete internal/larder, internal/
// httpapi's single-node deploy path, and cmd/sous/main.go") only partly
// landed as of the multi-node plan's Task 14. internal/larder and cmd/sous
// are gone. internal/deploy (this binary's local deploy.Manager) is NOT -
// it turned out to still be the only implementation behind several pages
// and endpoints with no node-scoped equivalent built in Tasks 1-13 (the
// Node dashboard's single-box section, /models, /model/{id} including its
// log viewer, /model/{id}/plan, the /events SSE stream, /api/status, /api/
// logs/{id}, and this Gateway's /v1/models listing - deploy.Runtime.Logs in
// particular has no gRPC equivalent in the wire protocol at all). Deleting
// internal/deploy now would mean inventing that node-scoped surface from
// scratch or deleting those features outright, neither of which Task 14
// was scoped to decide unilaterally - see the Task 14 report
// (.superpowers/sdd/2026-09-01-sous-multinode-implementation/task-14-report.md)
// for the full breakdown. Both paths live side by side until a follow-up
// task resolves this.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/codemug/sous/internal/apikey"
	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/config"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/grpcserver"
	"github.com/codemug/sous/internal/hf"
	"github.com/codemug/sous/internal/httpapi"
	"github.com/codemug/sous/internal/mtls"
	"github.com/codemug/sous/internal/nodecatalog"
	pb "github.com/codemug/sous/internal/pb/souslet/v1"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/reqlog"
	"github.com/codemug/sous/internal/store"
	"github.com/codemug/sous/internal/sysmem"
)

func main() {
	// "sous-api node add <node-id>" is the admin surface, not the server -
	// dispatched before fromFlags touches os.Args at all, since it parses
	// its own, unrelated flag set (see runNodeCmd in node.go) and must never
	// reach the "-listen is required" fatal below.
	if len(os.Args) > 1 && os.Args[1] == "node" {
		if err := runNodeCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, grpcListen, caStatePath := fromFlags(os.Args[1:])

	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	cat := catalog.New(st)
	n, err := cat.SeedIfEmpty()
	if err != nil {
		log.Fatalf("seeding the catalog: %v", err)
	}
	if n > 0 {
		log.Printf("seeded %d measured recipes", n)
	}

	// Then bring an already-seeded install up to date. SeedIfEmpty alone meant
	// a recipe added or corrected in a new release never reached a node that
	// had been seeded once - the fix shipped and stayed in the binary. This
	// adds what is missing and replaces only what Sous itself wrote and nobody
	// has since edited, so upgrading cannot silently undo a tuned recipe.
	sync, err := cat.SyncSeeds(false)
	if err != nil {
		log.Fatalf("syncing the catalog: %v", err)
	}
	if len(sync.Added) > 0 || len(sync.Updated) > 0 || len(sync.Kept) > 0 {
		log.Printf("catalog sync: added %v, updated %v, kept (edited locally) %v",
			sync.Added, sync.Updated, sync.Kept)
	}

	// The node catalog: the live, in-memory view of every connected souslet.
	// Populated by grpcserver as NodeSnapshot messages arrive, read by
	// httpapi's node-scoped routes and (eventually) by capacity planning
	// across the fleet.
	nodes := nodecatalog.New()

	// The node CA. A CA regenerated on every restart would invalidate every
	// already-issued node cert, disconnecting every souslet until each is
	// reissued by hand - persisting it across restarts is not optional
	// polish.
	ca, err := loadOrCreateCA(caStatePath)
	if err != nil {
		log.Fatalf("CA: %v", err)
	}
	// The server certificate has to carry the address souslets actually dial
	// as a SAN: souslet verifies this listener in full (mtls.ClientTLSConfig
	// sets no InsecureSkipVerify and no ServerName override), so a
	// certificate without a SAN for -grpc-listen's host is one no souslet can
	// complete a handshake against. requireBindable above already guaranteed
	// grpcListen splits into host:port and that the host is not a
	// bind-everything wildcard.
	grpcHost, _, err := net.SplitHostPort(grpcListen)
	if err != nil {
		log.Fatalf("split -grpc-listen %q: %v", grpcListen, err)
	}
	tlsConfig, err := ca.TLSConfigServer(grpcHost)
	if err != nil {
		log.Fatalf("build server TLS config: %v", err)
	}

	// ca is threaded into grpcserver so Connect can enforce node identity
	// against the VERIFIED peer certificate (and the CA's known-node set)
	// rather than trusting the node ID a connecting client asserts about
	// itself in its first snapshot.
	gsrv := grpcserver.New(nodes, ca)
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	pb.RegisterSousletServer(grpcSrv, gsrv)
	grpcLis, err := net.Listen("tcp", grpcListen)
	if err != nil {
		log.Fatalf("listen (gRPC) on %s: %v", grpcListen, err)
	}
	go func() {
		log.Printf("sous-api: gRPC (mTLS) listening on %s", grpcListen)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Fatalf("gRPC server: %v", err)
		}
	}()

	// Read the real pool. gx10 reports 121.6 GiB, not the nominal 128, and
	// planning against the nominal figure over-commits by 6 GiB before
	// anything is deployed.
	mem, err := sysmem.Read("/proc/meminfo")
	if err != nil {
		log.Fatalf("reading memory: %v", err)
	}
	log.Printf("pool %.1f GiB, %.1f available, swap used %.1f GiB, reserve %.0f GiB",
		mem.TotalGiB, mem.AvailableGiB, mem.SwapUsedGiB, cfg.Reserve)
	if mem.SwapUsedGiB > 1 {
		log.Printf("WARNING: %.1f GiB of swap is already in use, which is the "+
			"earliest signal of over-commitment on this box", mem.SwapUsedGiB)
	}

	// "cdi", matching this path's only prior behavior - the legacy local
	// deploy path has no per-node GPU-driver selection of its own, since it
	// never runs anywhere but asus-gx10 (CDI-only) today.
	rt, err := engine.New(cfg.Host(), "cdi")
	if err != nil {
		log.Fatalf("docker: %v", err)
	}

	// The HuggingFace token, when one is configured. Gated repos tie licence
	// acceptance to an ACCOUNT, so an anonymous pull of a repo whose agreement
	// was accepted in a browser still answers 401.
	hfs, err := hf.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("hf: %v", err)
	}

	mgr := &deploy.Manager{
		Store:   st,
		Catalog: cat,
		Runtime: rt,
		Planner: capacity.Planner{
			PoolGiB: mem.TotalGiB, ReserveGiB: cfg.Reserve, WarnFreeGiB: 12,
		},
		Ports:      ports.Allocator{Low: cfg.PortLow, High: cfg.PortHigh},
		BindHost:   cfg.Host(),
		ModelDir:   cfg.ModelDir,
		Secrets:    hfs,
		DropCaches: dropCaches,
		// Readiness is a port that answers, not a container that exists. A
		// vLLM model here is "running" for eight to ten minutes before it
		// serves anything, and without this every one of those minutes reads
		// as healthy.
		Probe: &deploy.Prober{Host: cfg.Host(), Timeout: 2 * time.Second},
	}

	// Read BEFORE anything is served. An install that forgot to configure
	// credentials should fail at startup, loudly, rather than come up open:
	// this process creates and destroys containers on its node (today,
	// directly; going forward, by dispatching to a souslet).
	guard, err := auth.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if guard.Disabled {
		log.Print("WARNING: SOUS_AUTH=none - anyone who can reach this port " +
			"can start and stop containers on this node and any connected souslet")
	}

	// API keys reach models and nothing else. Wiring the guard into auth is
	// what makes that true: without it a key would be an unrecognised bearer
	// token and simply fail, which is safe but useless.
	keys := &apikey.Manager{Store: st}
	guard.Keys = apikey.Guard{M: keys}

	// Buffered last-used timestamps are flushed on a timer rather than written
	// per request: a key used in a streaming loop would otherwise rewrite its
	// own file once per token.
	go func() {
		for range time.Tick(30 * time.Second) {
			keys.FlushLastUsed()
		}
	}()

	// Weight downloads run in a container carrying huggingface_hub, writing
	// into the same cache deployments read from. The default image is the one
	// most recipes already use, so it is present on the node and is the same
	// client that will later read what it writes.
	fx := &fetch.Manager{Runtime: rt, ModelDir: cfg.ModelDir, Image: cfg.FetchImage,
		Secrets: hfs}

	// Audit log of every chat-completion request: sender and payload,
	// append-only, one file per day under DataDir/reqlogs.
	reqLogW := &reqlog.Writer{Dir: filepath.Join(cfg.DataDir, "reqlogs")}
	reqLogR, err := reqlog.NewRetentionStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("reqlog: %v", err)
	}
	// Cleanup on an hourly tick rather than daily: a retention window an
	// operator just narrowed from the dashboard should take effect within the
	// hour, not sit for up to a day before anything acts on it. Deleting a
	// handful of already-expired daily files every hour costs nothing.
	go func() {
		for range time.Tick(time.Hour) {
			if n, err := reqLogW.Cleanup(reqLogR.Days(), time.Now()); err != nil {
				log.Printf("reqlog: cleanup: %v", err)
			} else if n > 0 {
				log.Printf("reqlog: cleanup removed %d file(s) past the retention window", n)
			}
		}
	}()

	// gsrv and nodes ARE populated here, unlike cmd/sous's nil, nil: this is
	// the control-plane binary, so deploy/undeploy/plan requests aimed at a
	// specific node route through gsrv to that node's souslet instead of (or
	// alongside, during migration) mgr's local deploy.Manager.
	h, err := httpapi.New(mgr, cat, keys, fx, hfs, reqLogW, reqLogR, mem.TotalGiB,
		filepath.Join(cfg.ModelDir, "hub"), filepath.Join(cfg.DataDir, "sources"), guard,
		gsrv, nodes)
	if err != nil {
		log.Fatalf("http: %v", err)
	}

	log.Printf("sous-api: HTTP listening on %s (models in %s)", cfg.Listen, cfg.ModelDir)
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: h}
	log.Fatal(httpSrv.ListenAndServe())
}

// loadOrCreateCA persists the node CA across restarts. A CA regenerated on
// every restart would invalidate every already-issued node cert,
// disconnecting every souslet until each is reissued by hand.
func loadOrCreateCA(path string) (*mtls.CA, error) {
	if _, err := os.Stat(path); err == nil {
		return mtls.LoadCA(path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat CA state %s: %w", path, err)
	}
	ca, err := mtls.NewCA()
	if err != nil {
		return nil, err
	}
	if err := ca.Save(path); err != nil {
		return nil, err
	}
	return ca, nil
}

// dropCaches must run before every model start. GB10 shares one pool between
// CPU and GPU, vLLM sizes its KV cache from CUDA-reported free memory, and the
// kernel does not count reclaimable page cache as free - so 25-35 GiB of
// just-read safetensors looks like memory that is gone. This node has OOM'd on
// a smaller model for exactly that reason.
//
// A failure here is not fatal to the deploy path's correctness, but it is
// reported rather than swallowed: a silently skipped drop shows up later as an
// inexplicably small KV cache.
func dropCaches() error {
	_ = exec.Command("sync").Run()
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200)
}

// fromFlags parses sous-api's flags into the same config.Config shape
// cmd/sous uses (for everything that isn't node/gRPC-specific), plus the
// two new flags this binary needs: -grpc-listen (the mTLS souslet-facing
// listener) and -ca-state (where the node CA is persisted across
// restarts). config.FromFlags itself isn't reused directly since it owns
// its own flag.FlagSet with no room for these two extra flags, but the
// validation it applies to -listen (never 0.0.0.0, host:port required) is
// mirrored here for both listeners: both are network-reachable and both
// carry the same "must not be reachable from everywhere" invariant this
// project applies to every listener it opens.
func fromFlags(args []string) (cfg config.Config, grpcListen, caStatePath string) {
	fs := flag.NewFlagSet("sous-api", flag.ExitOnError)
	fs.StringVar(&cfg.Listen, "listen", "", "HTTP listen address (host:port), tailnet IP only, never 0.0.0.0")
	fs.StringVar(&grpcListen, "grpc-listen", "", "gRPC listen address (host:port) for souslets to dial, tailnet IP only, never 0.0.0.0")
	fs.StringVar(&cfg.DataDir, "data", "/var/lib/sous-api", "data directory")
	fs.StringVar(&caStatePath, "ca-state", "", "path to persist the node CA across restarts")
	fs.StringVar(&cfg.ModelDir, "models", "", "host path holding model weights")
	fs.IntVar(&cfg.PortLow, "port-low", 18000, "low end of the deploy port range")
	fs.IntVar(&cfg.PortHigh, "port-high", 18100, "high end of the deploy port range")
	fs.Float64Var(&cfg.Reserve, "reserve-gib", 24,
		"memory reserved for OS, containers and CUDA contexts")
	fs.StringVar(&cfg.FetchImage, "fetch-image",
		"vllm/vllm-openai@sha256:d5a8e53ad2534e24b99ba1a2e3f183a213adc0da48ed83166cb75534a5903a17",
		"image used to download model weights; must carry huggingface_hub")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("config: %v", err)
	}

	for name, v := range map[string]string{
		"-listen": cfg.Listen, "-grpc-listen": grpcListen,
		"-data": cfg.DataDir, "-ca-state": caStatePath, "-models": cfg.ModelDir,
	} {
		if v == "" {
			log.Fatalf("config: %s is required", name)
		}
	}
	if err := requireBindable("-listen", cfg.Listen); err != nil {
		log.Fatal(err)
	}
	if err := requireBindable("-grpc-listen", grpcListen); err != nil {
		log.Fatal(err)
	}
	if cfg.PortLow > cfg.PortHigh {
		log.Fatal("config: -port-low is above -port-high")
	}
	return cfg, grpcListen, caStatePath
}

// requireBindable rejects an address that isn't host:port, or whose host
// would bind every interface. Sous generates container configuration and
// runs it (root-equivalent on its node by construction) and, as of this
// binary, also accepts mTLS connections that can drive that same
// machinery remotely - the network boundary is the protection for both, so
// neither listener may bind 0.0.0.0.
func requireBindable(flagName, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: %s must be host:port: %w", flagName, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || strings.EqualFold(host, "[::]") {
		return fmt.Errorf("config: %s refuses to bind %q; "+
			"a listener that can start and stop models must not be reachable from everywhere", flagName, host)
	}
	return nil
}
