package larder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codemug/sous/internal/recipe"
)

// hub builds a fake HuggingFace cache using the real directory naming.
func hub(t *testing.T, repos map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for name, kb := range repos {
		d := filepath.Join(root, name, "snapshots", "abc123")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "weights.bin"),
			make([]byte, kb*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRepoFromDir(t *testing.T) {
	cases := map[string]string{
		"models--Qwen--Qwen3.8-27B-FP8":                   "Qwen/Qwen3.8-27B-FP8",
		"models--nvidia--nemotron-3.5-asr-streaming-0.6b": "nvidia/nemotron-3.5-asr-streaming-0.6b",
		"models--Inferact--Qwen3.8-27B-NVFP4":             "Inferact/Qwen3.8-27B-NVFP4",
	}
	for dir, want := range cases {
		if got := RepoFromDir(dir); got != want {
			t.Errorf("RepoFromDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestScanClassifiesReferencedAndStale(t *testing.T) {
	root := hub(t, map[string]int{
		"models--Qwen--Qwen3.6-35B-A3B-FP8":     8,
		"models--Kwaipilot--KAT-Coder-V2.5-Dev": 16,
	})
	recipes := []recipe.Recipe{{ID: "qwen36", Model: "Qwen/Qwen3.6-35B-A3B-FP8"}}
	got, err := Scan(root, recipes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	byRepo := map[string]Entry{}
	for _, e := range got {
		byRepo[e.Repo] = e
	}
	if byRepo["Qwen/Qwen3.6-35B-A3B-FP8"].State != StateReferenced {
		t.Error("a repo named by a recipe must be referenced")
	}
	if byRepo["Kwaipilot/KAT-Coder-V2.5-Dev"].State != StateStale {
		t.Error("a repo no recipe names must be stale")
	}
	// Largest first: the operator wants the 65 GB entry at the top.
	if got[0].Bytes < got[1].Bytes {
		t.Error("entries must be sorted largest first")
	}
}

// Sizes come from the disk, not from a manifest. The model card describes the
// repo; the disk holds what actually landed.
func TestScanMeasuresRealBytes(t *testing.T) {
	root := hub(t, map[string]int{"models--a--b": 32})
	got, _ := Scan(root, nil, nil)
	if got[0].Bytes < 32*1024 {
		t.Fatalf("want at least 32 KiB measured, got %d", got[0].Bytes)
	}
}

func TestDeployedModelIsReferenced(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.8-27B-NVFP4": 4})
	recipes := []recipe.Recipe{{ID: "qwen38", Model: "Qwen/Qwen3.8-27B-NVFP4"}}
	got, _ := Scan(root, recipes, []string{"qwen38"})
	if got[0].State != StateReferenced {
		t.Fatalf("deployed model classified as %v", got[0].State)
	}
	if len(got[0].ReferencedBy) == 0 {
		t.Fatal("must name the referencing recipe")
	}
}

// The weights of a model whose replacement has not proven itself are the
// cheapest rollback available: deleting them costs a 25 GB re-download during
// an outage.
func TestArchivedRecipeStillProtectsItsWeights(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.8-27B-FP8": 4})
	recipes := []recipe.Recipe{
		{ID: "qwen38-fp8", Model: "Qwen/Qwen3.8-27B-FP8", Archived: true},
	}
	got, _ := Scan(root, recipes, nil)
	if got[0].State == StateStale {
		t.Fatal("an archived recipe's weights are protected, not stale")
	}
	if got[0].State != StateProtected {
		t.Fatalf("want protected, got %v", got[0].State)
	}
}

func TestTotalAndReclaimable(t *testing.T) {
	root := hub(t, map[string]int{
		"models--keep--me": 4,
		"models--drop--me": 8,
		"models--hold--me": 4,
	})
	recipes := []recipe.Recipe{
		{ID: "k", Model: "keep/me"},
		{ID: "h", Model: "hold/me", Archived: true},
	}
	got, _ := Scan(root, recipes, nil)
	if Total(got) <= 0 {
		t.Fatal("Total must sum entries")
	}
	// Only stale bytes are reclaimable without force.
	rec := Reclaimable(got)
	if rec <= 0 {
		t.Fatal("the stale entry must be reclaimable")
	}
	if rec >= Total(got) {
		t.Fatal("referenced and protected bytes must not count as reclaimable")
	}
}

// A fresh node has no hub directory yet, and that is not a failure.
func TestMissingHubIsNotAnError(t *testing.T) {
	got, err := Scan(filepath.Join(t.TempDir(), "absent"), nil, nil)
	if err != nil {
		t.Fatalf("missing hub should be empty, not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("expected no entries")
	}
}

func TestNonModelDirsAreIgnored(t *testing.T) {
	root := hub(t, map[string]int{"models--a--b": 4})
	// The hub also holds xet/ and modules/, which are not model snapshots.
	if err := os.MkdirAll(filepath.Join(root, "xet"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, _ := Scan(root, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want only the models-- entry, got %d", len(got))
	}
}

// ---------- deletion ----------

func TestDeleteRefusesReferenced(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.6-35B-A3B-FP8": 4})
	rs := []recipe.Recipe{{ID: "qwen36", Model: "Qwen/Qwen3.6-35B-A3B-FP8"}}
	entries, _ := Scan(root, rs, nil)
	if _, err := Delete(root, "Qwen/Qwen3.6-35B-A3B-FP8", entries, false); err == nil {
		t.Fatal("deleted weights a recipe references")
	}
}

// Referenced is a safety guard, not a policy one: force must not override it
// while a model could be using those files.
func TestForceDoesNotOverrideReferenced(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.6-35B-A3B-FP8": 4})
	rs := []recipe.Recipe{{ID: "qwen36", Model: "Qwen/Qwen3.6-35B-A3B-FP8"}}
	entries, _ := Scan(root, rs, nil)
	if _, err := Delete(root, "Qwen/Qwen3.6-35B-A3B-FP8", entries, true); err == nil {
		t.Fatal("force deleted weights an active recipe references")
	}
}

func TestDeleteRefusesProtectedRollback(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.8-27B-FP8": 4})
	rs := []recipe.Recipe{{ID: "qwen38-fp8", Model: "Qwen/Qwen3.8-27B-FP8", Archived: true}}
	entries, _ := Scan(root, rs, nil)
	_, err := Delete(root, "Qwen/Qwen3.8-27B-FP8", entries, false)
	if err == nil {
		t.Fatal("deleted a protected rollback without force")
	}
	var ge *GuardError
	if !errors.As(err, &ge) {
		t.Fatalf("want *GuardError, got %T", err)
	}
	if ge.Reason == "" {
		t.Fatal("a guard must say why")
	}
}

func TestForceDeletesProtected(t *testing.T) {
	root := hub(t, map[string]int{"models--Qwen--Qwen3.8-27B-FP8": 4})
	rs := []recipe.Recipe{{ID: "qwen38-fp8", Model: "Qwen/Qwen3.8-27B-FP8", Archived: true}}
	entries, _ := Scan(root, rs, nil)
	freed, err := Delete(root, "Qwen/Qwen3.8-27B-FP8", entries, true)
	if err != nil {
		t.Fatalf("force must proceed for a protected entry: %v", err)
	}
	if freed == 0 {
		t.Fatal("must report bytes freed")
	}
	if _, err := os.Stat(filepath.Join(root, "models--Qwen--Qwen3.8-27B-FP8")); !os.IsNotExist(err) {
		t.Fatal("directory survived a forced delete")
	}
}

func TestDeleteStaleSucceedsAndReportsBytes(t *testing.T) {
	root := hub(t, map[string]int{"models--Kwaipilot--KAT-Coder-V2.5-Dev": 16})
	entries, _ := Scan(root, nil, nil)
	freed, err := Delete(root, "Kwaipilot/KAT-Coder-V2.5-Dev", entries, false)
	if err != nil {
		t.Fatalf("stale weights should delete: %v", err)
	}
	if freed < 16*1024 {
		t.Fatalf("freed %d, want at least 16 KiB", freed)
	}
}

// Path validation survives force: force overrides a POLICY guard, never a
// SAFETY one.
func TestDeleteRejectsPathEscapeEvenWithForce(t *testing.T) {
	root := hub(t, map[string]int{"models--a--b": 1})
	entries, _ := Scan(root, nil, nil)
	for _, bad := range []string{"../../etc", "a/../../b", "/etc", ".."} {
		if _, err := Delete(root, bad, entries, true); err == nil {
			t.Fatalf("accepted dangerous repo %q even with force", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "models--a--b")); err != nil {
		t.Fatal("a rejected delete disturbed the hub")
	}
}

func TestDeleteUnknownRepoErrors(t *testing.T) {
	root := hub(t, map[string]int{"models--a--b": 1})
	entries, _ := Scan(root, nil, nil)
	if _, err := Delete(root, "never/downloaded", entries, false); err == nil {
		t.Fatal("deleted a repo that is not on disk")
	}
}
