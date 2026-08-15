package catalog

import (
	"strings"
	"testing"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

func newCatalog(t *testing.T) *Catalog {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(s)
}

func TestSaveRejectsInvalidRecipe(t *testing.T) {
	c := newCatalog(t)
	if err := c.Save(recipe.Recipe{ID: "Bad Id"}); err == nil {
		t.Fatal("catalog saved an invalid recipe")
	}
}

func TestSaveThenGet(t *testing.T) {
	c := newCatalog(t)
	in := Seeds()[0]
	if err := c.Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != in.ID || got.Kind != in.Kind || got.Image != in.Image {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// Notes carry the hard-won reasoning; losing them on a round trip would
	// quietly discard the most expensive part of the recipe.
	if got.Notes == "" {
		t.Fatal("notes lost on round trip")
	}
}

func TestGetRejectsTraversalID(t *testing.T) {
	c := newCatalog(t)
	for _, bad := range []string{"../escape", "a/b", ""} {
		if _, err := c.Get(bad); err == nil {
			t.Fatalf("accepted dangerous id %q", bad)
		}
	}
}

func TestSeedIfEmptyIsIdempotent(t *testing.T) {
	c := newCatalog(t)
	n, err := c.SeedIfEmpty()
	if err != nil || n == 0 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	again, err := c.SeedIfEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second seed wrote %d recipes; must be a no-op", again)
	}
}

// Every seed is a real, measured configuration. If one does not validate, the
// schema and the catalog have drifted apart.
func TestAllSeedsValidate(t *testing.T) {
	for _, r := range Seeds() {
		if err := r.Validate(); err != nil {
			t.Errorf("seed %s invalid: %v", r.ID, err)
		}
	}
}

func TestSeedsHaveUniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Seeds() {
		if seen[r.ID] {
			t.Errorf("duplicate seed id %q", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestSeedsCoverEveryKindAndModality(t *testing.T) {
	kinds := map[recipe.Kind]bool{}
	mods := map[recipe.Modality]bool{}
	for _, r := range Seeds() {
		kinds[r.Kind] = true
		mods[r.Modality] = true
	}
	for _, k := range []recipe.Kind{recipe.KindVLLM, recipe.KindTransformers, recipe.KindContainer} {
		if !kinds[k] {
			t.Errorf("no seed exercises kind %q", k)
		}
	}
	for _, m := range []recipe.Modality{recipe.ModalityText, recipe.ModalityOmni,
		recipe.ModalityASR, recipe.ModalityTTS} {
		if !mods[m] {
			t.Errorf("no seed exercises modality %q", m)
		}
	}
}

// A CPU-only recipe declaring zero is a real answer, not a missing value, and
// the capacity model must accept it. whisper is the remaining example - kokoro
// moved to the GPU on 2026-08-16 and now declares 3 GiB.
func TestZeroGPUIsAcceptedForCPUOnlyRecipes(t *testing.T) {
	found := false
	for _, r := range Seeds() {
		if r.ID == "whisper" {
			found = true
			if r.Declared.TotalGiB() != 0 {
				t.Fatalf("whisper is CPU-only, want 0 GiB, got %.2f", r.Declared.TotalGiB())
			}
		}
	}
	if !found {
		t.Fatal("expected a CPU-only seed to exercise the zero case")
	}
}

// If a recipe declares GPU weights it must actually be a GPU service - a
// footprint that disagrees with reality makes every capacity decision wrong.
func TestKokoroDeclaresItsGPUFootprint(t *testing.T) {
	for _, r := range Seeds() {
		if r.ID != "kokoro" {
			continue
		}
		if r.Declared.WeightsGiB < 2 {
			t.Fatalf("kokoro runs on the GPU and measured 3 GiB; declared %.2f", r.Declared.WeightsGiB)
		}
		if !strings.Contains(r.Image, "gpu") {
			t.Fatalf("declared a GPU footprint but the image is %q", r.Image)
		}
	}
}

// Negative results are kept on purpose so nobody re-derives why they lost.
func TestSupersededRecipesAreArchivedNotAbsent(t *testing.T) {
	want := map[string]bool{"qwen38-fp8": false, "omni": false, "whisper": false}
	for _, r := range Seeds() {
		if _, ok := want[r.ID]; ok {
			want[r.ID] = true
			if !r.Archived {
				t.Errorf("%s is superseded and must be archived", r.ID)
			}
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("expected a seed for the retired %q", id)
		}
	}
}

func TestListReturnsSavedRecipes(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	all, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(Seeds()) {
		t.Fatalf("want %d recipes, got %d", len(Seeds()), len(all))
	}
}
