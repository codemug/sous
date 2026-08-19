package catalog

import (
	"fmt"
	"sort"

	"github.com/codemug/sous/internal/overlay"
	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/sources"
	"github.com/codemug/sous/internal/store"
)

// SourceLoader is the part of sources.Manager the catalog needs, kept as an
// interface so the catalog can be tested without git.
type SourceLoader interface {
	LoadRecipes(name string) ([]recipe.Recipe, error)
}

// Resolved is a recipe plus where it came from. Provenance matters: a recipe
// you can edit freely and one that a fetch may move under you are different
// things, and the UI has to be able to say which is which.
type Resolved struct {
	recipe.Recipe
	Source    string `json:"source,omitempty"`   // empty means local
	Overlaid  bool   `json:"overlaid,omitempty"` // a local patch is applied
	Shadowed  bool   `json:"shadowed,omitempty"` // a local recipe hides this one
	ShadowsID string `json:"shadows,omitempty"`  // this local recipe hides a source one
}

func (c *Catalog) Sources() ([]sources.Source, error) {
	names, err := c.s.List(store.KindSource)
	if err != nil {
		return nil, err
	}
	out := make([]sources.Source, 0, len(names))
	for _, n := range names {
		var src sources.Source
		if err := c.s.ReadYAML(store.KindSource, n, &src); err != nil {
			continue
		}
		out = append(out, src)
	}
	return out, nil
}

func (c *Catalog) SaveSource(src sources.Source) error {
	if src.Name == "" || src.URL == "" {
		return fmt.Errorf("catalog: a source needs a name and a URL")
	}
	return c.s.WriteYAML(store.KindSource, src.Name, src)
}

func overlayKey(source, recipeID string) string { return source + "--" + recipeID }

func (c *Catalog) Overlay(source, recipeID string) (overlay.Patch, bool) {
	var p overlay.Patch
	if err := c.s.ReadYAML(store.KindOverlay, overlayKey(source, recipeID), &p); err != nil {
		return overlay.Patch{}, false
	}
	return p, true
}

func (c *Catalog) SaveOverlay(p overlay.Patch) error {
	if p.Source == "" || p.Recipe == "" {
		return fmt.Errorf("catalog: an overlay needs a source and a recipe")
	}
	return c.s.WriteYAML(store.KindOverlay, overlayKey(p.Source, p.Recipe), p)
}

// Effective is everything on offer: local recipes plus every source recipe
// with its overlay applied.
//
// A local recipe with the same id SHADOWS the source one rather than merging
// with it, and both are reported. Silently replacing would make a fetch look
// like it did nothing when it actually delivered a change you are not seeing.
func (c *Catalog) Effective(loader SourceLoader) ([]Resolved, error) {
	local, err := c.List()
	if err != nil {
		return nil, err
	}
	localIDs := map[string]bool{}
	out := make([]Resolved, 0, len(local))
	for _, r := range local {
		localIDs[r.ID] = true
		out = append(out, Resolved{Recipe: r})
	}

	srcs, err := c.Sources()
	if err != nil {
		return nil, err
	}
	for _, src := range srcs {
		rs, err := loader.LoadRecipes(src.Name)
		if err != nil {
			// A broken mirror must not hide the recipes that do resolve.
			continue
		}
		for _, r := range rs {
			res := Resolved{Recipe: r, Source: src.Name}
			if p, ok := c.Overlay(src.Name, r.ID); ok && p.HasOverride() {
				res.Recipe = overlay.Apply(r, p)
				res.Overlaid = true
			}
			if localIDs[r.ID] {
				res.Shadowed = true
				for i := range out {
					if out[i].ID == r.ID && out[i].Source == "" {
						out[i].ShadowsID = src.Name
					}
				}
			}
			out = append(out, res)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Source < out[j].Source
	})
	return out, nil
}
