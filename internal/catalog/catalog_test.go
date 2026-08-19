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
// the capacity model must accept it.
//
// SELF-CONTAINED ON PURPOSE. This used to assert against whichever seed
// happened to be CPU-only - kokoro, then whisper. kokoro moved to the GPU and
// whisper was deleted, and the test then failed for having nothing to look at
// rather than because the zero case broke. The behaviour under test belongs to
// Footprint, not to the catalog's current contents.
func TestZeroGPUIsAcceptedForCPUOnlyRecipes(t *testing.T) {
	cpuOnly := recipe.Recipe{
		ID: "cpu-only", Kind: recipe.KindContainer, Modality: recipe.ModalityTTS,
		Image: "example/cpu:latest", Declared: recipe.Footprint{},
	}
	if err := cpuOnly.Validate(); err != nil {
		t.Fatalf("a zero footprint must be valid: %v", err)
	}
	if got := cpuOnly.Declared.TotalGiB(); got != 0 {
		t.Fatalf("TotalGiB = %.2f, want 0", got)
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

// ARCHIVED MEANS "CANNOT RUN HERE", NOT "NOT RUNNING RIGHT NOW.
//
// The UI hides the deploy control on an archived recipe, so marking a working
// model archived because it happens to be stopped makes it undeployable from
// the catalog. That is exactly what happened to nemotron35, qwen38-fp8 and
// omni: each is a configuration that works on this box, each was flagged
// archived for being merely superseded or stopped, and all three became
// unreachable in the UI.
//
// Superseded is not the same as impossible. A slower or beaten configuration
// stays ACTIVE and keeps its notes; only something proven unrunnable on this
// hardware earns the flag - and whisper, the one honest case (CTranslate2
// ships no aarch64 CUDA build, so it could not use the GPU at all), was
// deleted outright rather than archived.
func TestWorkingRecipesAreNotArchived(t *testing.T) {
	mustBeActive := map[string]bool{"nemotron35": false, "qwen38-fp8": false, "omni": false}
	for _, r := range Seeds() {
		if _, ok := mustBeActive[r.ID]; ok {
			mustBeActive[r.ID] = true
			if r.Archived {
				t.Errorf("%s works on this box and must be deployable, not archived", r.ID)
			}
		}
	}
	for id, found := range mustBeActive {
		if !found {
			t.Errorf("expected a seed for %q - superseded is not a reason to delete it", id)
		}
	}
}

// whisper could not use the GPU on this hardware at all, which is a fact about
// the box rather than a preference. It is deleted, not archived, so nobody
// spends a deploy window rediscovering it.
func TestWhisperIsGone(t *testing.T) {
	for _, r := range Seeds() {
		if r.ID == "whisper" {
			t.Fatal("whisper is back; it cannot run on aarch64 and should not be offered")
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
