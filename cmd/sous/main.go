// Command sous serves the recipe catalog and deploys models on one node.
package main

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

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
		DropCaches: dropCaches,
	}

	// The larder scans MODEL_DIR/hub, which is where huggingface_hub
	// places snapshots under the HF_HOME bind mount.
	h, err := httpapi.New(mgr, cat, mem.TotalGiB,
		filepath.Join(cfg.ModelDir, "hub"), filepath.Join(cfg.DataDir, "sources"))
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
