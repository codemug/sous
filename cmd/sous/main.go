// Command sous serves the recipe catalog and deploys models on one node.
package main

import (
	"github.com/codemug/sous/internal/apikey"
	"github.com/codemug/sous/internal/fetch"
	"github.com/codemug/sous/internal/hf"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/codemug/sous/internal/auth"
	"github.com/codemug/sous/internal/capacity"
	"github.com/codemug/sous/internal/catalog"
	"github.com/codemug/sous/internal/config"
	"github.com/codemug/sous/internal/deploy"
	"github.com/codemug/sous/internal/engine"
	"github.com/codemug/sous/internal/httpapi"
	"github.com/codemug/sous/internal/ports"
	"github.com/codemug/sous/internal/store"
	"github.com/codemug/sous/internal/sysmem"
)

func main() {
	cfg, err := config.FromFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}

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

	rt, err := engine.New(cfg.Host())
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
	// this process creates and destroys containers on its node.
	guard, err := auth.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if guard.Disabled {
		log.Print("WARNING: SOUS_AUTH=none - anyone who can reach this port " +
			"can start and stop containers on this node")
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

	// The larder scans MODEL_DIR/hub, which is where huggingface_hub places
	// snapshots under the HF_HOME bind mount.
	h, err := httpapi.New(mgr, cat, keys, fx, hfs, mem.TotalGiB,
		filepath.Join(cfg.ModelDir, "hub"), filepath.Join(cfg.DataDir, "sources"), guard)
	if err != nil {
		log.Fatalf("http: %v", err)
	}

	log.Printf("sous listening on %s (models in %s)", cfg.Listen, cfg.ModelDir)
	srv := &http.Server{Addr: cfg.Listen, Handler: h}
	log.Fatal(srv.ListenAndServe())
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
