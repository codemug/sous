// Package catalog is the set of recipes Sous knows about.
package catalog

import (
	"fmt"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

type Catalog struct{ s *store.Store }

func New(s *store.Store) *Catalog { return &Catalog{s: s} }

func (c *Catalog) Save(r recipe.Recipe) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return c.s.WriteYAML(store.KindRecipe, r.ID, r)
}

func (c *Catalog) Get(id string) (recipe.Recipe, error) {
	if !recipe.ValidID(id) {
		return recipe.Recipe{}, fmt.Errorf("catalog: invalid id %q", id)
	}
	var r recipe.Recipe
	if err := c.s.ReadYAML(store.KindRecipe, id, &r); err != nil {
		return recipe.Recipe{}, err
	}
	return r, nil
}

func (c *Catalog) List() ([]recipe.Recipe, error) {
	names, err := c.s.List(store.KindRecipe)
	if err != nil {
		return nil, err
	}
	out := make([]recipe.Recipe, 0, len(names))
	for _, n := range names {
		r, err := c.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SeedIfEmpty writes the measured catalog on first run and does nothing after,
// so an operator's edits are never overwritten by a restart.
func (c *Catalog) SeedIfEmpty() (int, error) {
	names, err := c.s.List(store.KindRecipe)
	if err != nil {
		return 0, err
	}
	if len(names) > 0 {
		return 0, nil
	}
	n := 0
	for _, r := range Seeds() {
		if err := c.Save(r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
