package grpcclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/codemug/sous/internal/pb/souslet/v1"
)

// weightsHub builds a fake HuggingFace cache using the real directory
// naming, under a "hub" subdirectory of a fresh ModelDir - mirroring
// internal/larder/larder_test.go's own hub() helper, but rooted at
// ModelDir/hub rather than ModelDir directly, since that is the real
// on-disk layout deleteWeights expects (see weights.go's hubDir).
func weightsHub(t *testing.T, repos map[string]int) (modelDir string) {
	t.Helper()
	modelDir = t.TempDir()
	for name, kb := range repos {
		d := filepath.Join(modelDir, "hub", name, "snapshots", "abc123")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "weights.bin"),
			make([]byte, kb*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return modelDir
}

// TestDeleteWeightsRefusesADeployedRecipesWeightsEvenWithForce is the
// brief's own failing test (Task 11, Step 2), adapted to this package's real
// on-disk layout (ModelDir/hub, not ModelDir directly) and to
// currentlyDeployed's real shape (recipe ID -> model, not repo -> bool - see
// that field's doc comment in handlers.go for why: a bare repo->bool map
// cannot correctly handle two different recipes sharing one repo without
// either extra reference-counting or silently under-protecting on an
// undeploy race, and recipe-ID-keyed is exactly what HandleDeploy/
// HandleUndeploy already populate/clear in lockstep for footprints and
// ports). The guard under test is identical either way: a currently-
// deployed repo's weights are never deleted, force included.
func TestDeleteWeightsRefusesADeployedRecipesWeightsEvenWithForce(t *testing.T) {
	dir := weightsHub(t, map[string]int{"models--org--Name": 4})
	h := &Handlers{ModelDir: dir, currentlyDeployed: map[string]string{"some-recipe": "org/Name"}}
	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "org/Name", Force: true})
	if result.Error == "" {
		t.Fatal("expected an error deleting weights for a currently-deployed recipe, even with force")
	}
	// Still on disk: a refused delete must not have touched anything.
	if _, err := os.Stat(filepath.Join(dir, "hub", "models--org--Name")); err != nil {
		t.Fatal("a refused delete disturbed the hub")
	}
}

// TestDeleteWeightsSucceedsForANonDeployedRepoAndReportsBytesFreed is the
// success path this package's version of the guard still allows: nothing on
// this node currently deploys the repo, so its weights are reclaimable
// (matching the original StateStale case) without force.
func TestDeleteWeightsSucceedsForANonDeployedRepoAndReportsBytesFreed(t *testing.T) {
	dir := weightsHub(t, map[string]int{"models--Kwaipilot--KAT-Coder-V2.5-Dev": 16})
	h := &Handlers{ModelDir: dir}
	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "Kwaipilot/KAT-Coder-V2.5-Dev"})
	if result.Error != "" {
		t.Fatalf("expected a clean delete, got error: %s", result.Error)
	}
	if result.BytesFreed < 16*1024 {
		t.Fatalf("freed %d, want at least 16 KiB", result.BytesFreed)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub", "models--Kwaipilot--KAT-Coder-V2.5-Dev")); !os.IsNotExist(err) {
		t.Fatal("directory survived a delete that should have succeeded")
	}
}

// TestDeleteWeightsRejectsPathEscapeEvenWithForce proves the SAFETY guard
// against a malicious/malformed repo id survived relocation unchanged -
// mirrors internal/larder/larder_test.go's
// TestDeleteRejectsPathEscapeEvenWithForce.
func TestDeleteWeightsRejectsPathEscapeEvenWithForce(t *testing.T) {
	dir := weightsHub(t, map[string]int{"models--a--b": 1})
	h := &Handlers{ModelDir: dir}
	for _, bad := range []string{"../../etc", "a/../../b", "/etc", ".."} {
		result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: bad, Force: true})
		if result.Error == "" {
			t.Fatalf("accepted dangerous repo %q even with force", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "hub", "models--a--b")); err != nil {
		t.Fatal("a rejected delete disturbed the hub")
	}
}

// TestDeleteWeightsErrorsForUnknownRepo mirrors
// internal/larder/larder_test.go's TestDeleteUnknownRepoErrors.
func TestDeleteWeightsErrorsForUnknownRepo(t *testing.T) {
	dir := weightsHub(t, map[string]int{"models--a--b": 1})
	h := &Handlers{ModelDir: dir}
	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "never/downloaded"})
	if result.Error == "" {
		t.Fatal("expected an error deleting a repo that is not on disk")
	}
}

// TestDeleteWeightsRefusalDropsNothingWhenTwoRecipesShareARepo guards the
// exact edge case a bare repo->bool currentlyDeployed map (as the brief's
// own illustrative test literally used) would get wrong: undeploying ONE of
// two recipes that both name the same model repo must not stop protecting
// that repo while the OTHER recipe is still deployed with it.
func TestDeleteWeightsRefusalDropsNothingWhenTwoRecipesShareARepo(t *testing.T) {
	dir := weightsHub(t, map[string]int{"models--org--Name": 4})
	h := &Handlers{ModelDir: dir, currentlyDeployed: map[string]string{
		"recipe-a": "org/Name",
		"recipe-b": "org/Name",
	}}
	h.forgetModel("recipe-a")
	result := h.HandleDeleteWeights(context.Background(), &pb.DeleteWeightsCommand{Repo: "org/Name", Force: true})
	if result.Error == "" {
		t.Fatal("expected the guard to still refuse: recipe-b is still deployed with this repo")
	}
}

// TestSnapshotReportsCachedWeightRepos proves the relocated disk scan closes
// the loop this task's UI step needs: sous-api's nodecatalog only knows a
// repo is "resident" on a node (and can offer a delete action for it) if
// Snapshot actually reports it, which nothing did before this task (the
// field existed on the wire since Task 1 but nothing populated it - see this
// task's report for why closing that gap was in scope here).
func TestSnapshotReportsCachedWeightRepos(t *testing.T) {
	dir := weightsHub(t, map[string]int{
		"models--org--Name":    4,
		"models--other--Model": 8,
	})
	h := &Handlers{ModelDir: dir, Runtime: &fakeRuntime{}}
	snap := h.Snapshot(context.Background(), "test-node", 100, 10)
	got := map[string]bool{}
	for _, r := range snap.CachedWeightRepos {
		got[r] = true
	}
	if !got["org/Name"] || !got["other/Model"] {
		t.Fatalf("Snapshot.CachedWeightRepos = %v, want both cached repos listed", snap.CachedWeightRepos)
	}
}
