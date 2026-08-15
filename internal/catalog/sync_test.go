package catalog

import (
	"slices"
	"testing"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

// A freshly seeded install is already current, so an upgrade that changed
// nothing must report exactly that - not rewrite every recipe and claim work.
func TestSyncAfterSeedingIsANoOp(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Updated) != 0 || len(res.Kept) != 0 {
		t.Errorf("expected everything current, got added=%v updated=%v kept=%v",
			res.Added, res.Updated, res.Kept)
	}
	if len(res.Current) != len(Seeds()) {
		t.Errorf("current = %d, want %d", len(res.Current), len(Seeds()))
	}
}

// The gap this whole file exists to close: a recipe added in a new release
// could never reach an install that had already been seeded, because
// SeedIfEmpty returns early the moment any recipe exists.
func TestSyncAddsARecipeMissingFromAnExistingInstall(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}

	// Stand in for "this install predates the release that added it".
	missing := Seeds()[0].ID
	if err := c.s.Delete(store.KindRecipe, missing); err != nil {
		t.Fatal(err)
	}
	if err := c.s.Delete(store.KindSeedMark, missing); err != nil {
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Added, missing) {
		t.Fatalf("added = %v, want it to contain %q", res.Added, missing)
	}
	if _, err := c.Get(missing); err != nil {
		t.Errorf("%s should be back on disk: %v", missing, err)
	}
}

// The property that makes automatic sync safe to run on every boot. An
// operator who tuned a recipe because a model would not start must not have
// that undone by a version bump.
func TestSyncNeverOverwritesAnEditedRecipe(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}

	id := Seeds()[0].ID
	edited, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	edited.Args["gpu-memory-utilization"] = 0.11
	if err := c.Save(edited); err != nil {
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Kept, id) {
		t.Errorf("kept = %v, want it to contain %q", res.Kept, id)
	}

	after, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Args["gpu-memory-utilization"] != 0.11 {
		t.Errorf("the edit was lost: gpu-memory-utilization = %v, want 0.11",
			after.Args["gpu-memory-utilization"])
	}
}

// The other half: a recipe still byte-for-byte what Sous wrote has nothing
// worth preserving, so a corrected seed replaces it. Without this, shipping a
// fix to a recipe would still require deleting it on the node by hand.
func TestSyncUpdatesARecipeNobodyTouched(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}

	// Simulate an older release having written a now-superseded version: put
	// stale content on disk AND mark it as Sous-written, which is exactly the
	// state an install upgrading from that release would be in.
	id := Seeds()[0].ID
	stale, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	stale.Image = "vllm/vllm-openai:some-older-tag"
	staleDigest, err := digestOf(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.saveSeed(stale, staleDigest); err != nil {
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Updated, id) {
		t.Fatalf("updated = %v, want it to contain %q", res.Updated, id)
	}

	after, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Image != Seeds()[0].Image {
		t.Errorf("image = %q, want the seed's %q", after.Image, Seeds()[0].Image)
	}
}

// Installs seeded before marks existed have no provenance, so their recipes
// cannot be told apart from edited ones. Keeping them is the safe reading, and
// this pins that choice so nobody "fixes" it into clobbering real edits.
func TestSyncKeepsRecipesWithNoProvenance(t *testing.T) {
	c := newCatalog(t)

	old := Seeds()[0]
	old.Image = "vllm/vllm-openai:pre-seedmark-era"
	if err := c.Save(old); err != nil { // Save, not saveSeed: no mark written.
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Kept, old.ID) {
		t.Errorf("kept = %v, want it to contain %q", res.Kept, old.ID)
	}

	after, err := c.Get(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Image != "vllm/vllm-openai:pre-seedmark-era" {
		t.Errorf("an unprovenanced recipe was overwritten: image = %q", after.Image)
	}

	// The rest of the catalog is still missing and must have arrived.
	if len(res.Added) != len(Seeds())-1 {
		t.Errorf("added = %d, want %d", len(res.Added), len(Seeds())-1)
	}
}

// force is the escape hatch, and it has to actually override the guard or the
// operator's only remaining option is deleting files on the node.
func TestSyncForceOverwritesEvenAnEditedRecipe(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}

	id := Seeds()[0].ID
	edited, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	edited.Image = "vllm/vllm-openai:operator-pinned"
	if err := c.Save(edited); err != nil {
		t.Fatal(err)
	}

	res, err := c.SyncSeeds(true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Updated, id) {
		t.Fatalf("updated = %v, want it to contain %q", res.Updated, id)
	}

	after, err := c.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Image != Seeds()[0].Image {
		t.Errorf("force did not overwrite: image = %q", after.Image)
	}
}

// A sync must leave a valid catalog behind whatever path it took.
func TestSyncedRecipesStayValid(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SyncSeeds(false); err != nil {
		t.Fatal(err)
	}
	rs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != len(Seeds()) {
		t.Fatalf("catalog has %d recipes, want %d", len(rs), len(Seeds()))
	}
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r.ID, err)
		}
	}
	var _ recipe.Recipe = rs[0]
}
