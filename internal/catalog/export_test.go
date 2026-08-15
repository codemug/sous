package catalog

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// recipes/ is not a second, hand-maintained copy of the catalog - README calls
// it the seeds "exported as recipes Sous can fetch as a git source". Nothing
// enforced that, and it drifted: the commit that gave every seed its Env block
// never reached the export, so seven of the eight published recipes shipped
// without TORCH_CUDA_ARCH_LIST or CUTE_DSL_ARCH.
//
// That is the failure TestVLLMSeedsCarryTheArchEnv already guards against -
// applied to the wrong artifact. The seeds were correct the whole time; the
// files people actually fetch were not. Missing arch env does not fail loudly,
// it just changes which kernels get JIT'd, so nobody would have noticed until a
// fetched recipe behaved differently from the identically-named seed.
//
// Regenerate with:
//
//	go test ./internal/catalog -run TestPublishedCatalogMatchesSeeds -rewrite-recipes
var rewriteRecipes = flag.Bool("rewrite-recipes", false,
	"rewrite recipes/*.yaml from the seeds instead of comparing against them")

func TestPublishedCatalogMatchesSeeds(t *testing.T) {
	dir := filepath.Join("..", "..", "recipes")

	expected := map[string]bool{}
	for _, r := range Seeds() {
		name := r.ID + ".yaml"
		expected[name] = true
		path := filepath.Join(dir, name)

		want, err := yaml.Marshal(r)
		if err != nil {
			t.Fatalf("%s: marshal: %v", r.ID, err)
		}

		if *rewriteRecipes {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatalf("%s: write: %v", r.ID, err)
			}
			continue
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: seed has no published recipe (%v) - run with -rewrite-recipes", r.ID, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("recipes/%s differs from the seed - run with -rewrite-recipes\n--- published ---\n%s\n--- seed ---\n%s",
				name, got, want)
		}
	}

	// A recipe file with no seed behind it is drift in the other direction: it
	// would be fetchable by anyone adding this repo as a source while being
	// invisible to every test that reasons about Seeds().
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read recipes dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		if !expected[e.Name()] {
			t.Errorf("recipes/%s has no seed behind it", e.Name())
		}
	}
}
