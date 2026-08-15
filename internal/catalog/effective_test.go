package catalog

import (
	"testing"

	"github.com/codemug/sous/internal/overlay"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/sources"
)

type fakeLoader map[string][]recipe.Recipe

func (f fakeLoader) LoadRecipes(name string) ([]recipe.Recipe, error) {
	return f[name], nil
}

func sourceRecipe(id string) recipe.Recipe {
	return recipe.Recipe{
		ID: id, Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Org/" + id, Image: "img:1",
		Args: map[string]any{"gpu-memory-utilization": 0.62, "max-model-len": 131072},
	}
}

func TestEffectiveIncludesSourceRecipes(t *testing.T) {
	c := newCatalog(t)
	if err := c.SaveSource(sources.Source{Name: "community", URL: "file:///x", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	loader := fakeLoader{"community": {sourceRecipe("community-model")}}

	got, err := c.Effective(loader)
	if err != nil {
		t.Fatal(err)
	}
	var found *Resolved
	for i := range got {
		if got[i].ID == "community-model" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatal("source recipe missing from Effective")
	}
	if found.Source != "community" {
		t.Fatalf("provenance lost: %+v", found)
	}
}

func TestEffectiveAppliesOverlay(t *testing.T) {
	c := newCatalog(t)
	c.SaveSource(sources.Source{Name: "community", URL: "file:///x"})
	if err := c.SaveOverlay(overlay.Patch{
		Source: "community", Recipe: "community-model", Base: "sha1",
		Args: map[string]any{"gpu-memory-utilization": 0.55},
	}); err != nil {
		t.Fatal(err)
	}
	loader := fakeLoader{"community": {sourceRecipe("community-model")}}

	got, _ := c.Effective(loader)
	for _, r := range got {
		if r.ID != "community-model" {
			continue
		}
		if !r.Overlaid {
			t.Fatal("overlay not flagged")
		}
		if r.Args["gpu-memory-utilization"] != 0.55 {
			t.Fatalf("overlay not applied: %v", r.Args)
		}
		// An overlay is sparse: untouched upstream keys survive.
		if r.Args["max-model-len"] != 131072 {
			t.Fatalf("untouched upstream key lost: %v", r.Args)
		}
		return
	}
	t.Fatal("recipe not found")
}

// Shadowing must be visible from both sides, or a fetch looks like it did
// nothing when it actually delivered a change nobody is seeing.
func TestLocalRecipeShadowsSourceAndBothAreReported(t *testing.T) {
	c := newCatalog(t)
	local := sourceRecipe("clash")
	local.Image = "my/local:1"
	if err := c.Save(local); err != nil {
		t.Fatal(err)
	}
	c.SaveSource(sources.Source{Name: "community", URL: "file:///x"})
	loader := fakeLoader{"community": {sourceRecipe("clash")}}

	got, _ := c.Effective(loader)
	var localSide, sourceSide *Resolved
	for i := range got {
		if got[i].ID != "clash" {
			continue
		}
		if got[i].Source == "" {
			localSide = &got[i]
		} else {
			sourceSide = &got[i]
		}
	}
	if localSide == nil || sourceSide == nil {
		t.Fatal("both sides of a shadow must be present")
	}
	if !sourceSide.Shadowed {
		t.Fatal("the source recipe must be marked shadowed")
	}
	if localSide.ShadowsID != "community" {
		t.Fatalf("the local recipe must name what it shadows, got %q", localSide.ShadowsID)
	}
	if localSide.Image != "my/local:1" {
		t.Fatal("the local recipe must win")
	}
}

// A broken mirror must not hide the recipes that do resolve.
func TestBrokenSourceDoesNotHideOthers(t *testing.T) {
	c := newCatalog(t)
	c.SaveSource(sources.Source{Name: "good", URL: "file:///a"})
	c.SaveSource(sources.Source{Name: "broken", URL: "file:///b"})
	loader := fakeLoader{"good": {sourceRecipe("from-good")}} // "broken" returns nothing

	got, err := c.Effective(loader)
	if err != nil {
		t.Fatalf("one broken source failed the whole resolve: %v", err)
	}
	found := false
	for _, r := range got {
		if r.ID == "from-good" {
			found = true
		}
	}
	if !found {
		t.Fatal("a working source was lost because another was broken")
	}
}

func TestEffectiveWithNoSourcesIsJustLocal(t *testing.T) {
	c := newCatalog(t)
	if _, err := c.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Effective(fakeLoader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(Seeds()) {
		t.Fatalf("want %d, got %d", len(Seeds()), len(got))
	}
	for _, r := range got {
		if r.Source != "" {
			t.Fatalf("no sources configured, yet %s claims one", r.ID)
		}
	}
}

func TestSaveSourceRequiresNameAndURL(t *testing.T) {
	c := newCatalog(t)
	if err := c.SaveSource(sources.Source{Name: "x"}); err == nil {
		t.Fatal("accepted a source with no URL")
	}
	if err := c.SaveSource(sources.Source{URL: "u"}); err == nil {
		t.Fatal("accepted a source with no name")
	}
}
