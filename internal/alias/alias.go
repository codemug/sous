// Package alias holds the extra names a deployed model answers to.
//
// NOT ON THE RECIPE, deliberately. recipe.ServedAs is what a model calls itself
// wherever it runs, and it travels with the recipe into git. An alias is a local
// routing decision - "on this box, `fast` means qwen38-dflash2" - and putting
// that in a recipe would carry one node's naming to every other node that
// imported it.
//
// KEYED BY RECIPE ID, not folded into the deployment record. An alias that
// vanished on undeploy would take every client calling it down with the
// redeploy, and redeploying is routine here: a flag change is an undeploy and a
// deploy. The names outlive the container.
package alias

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/codemug/sous/internal/recipe"
	"github.com/codemug/sous/internal/store"
)

// Catalog is the slice of the catalog this needs: every recipe, so a proposed
// alias can be checked against every name already in use.
type Catalog interface {
	List() ([]recipe.Recipe, error)
}

// Manager stores and validates aliases.
type Manager struct {
	Store *store.Store
	Cat   Catalog
}

type set struct {
	RecipeID string   `yaml:"recipe_id"`
	Names    []string `yaml:"names"`
}

// Of returns the aliases for one recipe. A recipe with none is not an error.
func (m *Manager) Of(recipeID string) []string {
	var s set
	if err := m.Store.ReadYAML(store.KindAlias, recipeID, &s); err != nil {
		return nil
	}
	return s.Names
}

// All returns every alias, by recipe id.
func (m *Manager) All() (map[string][]string, error) {
	ids, err := m.Store.List(store.KindAlias)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		if names := m.Of(id); len(names) > 0 {
			out[id] = names
		}
	}
	return out, nil
}

// Set replaces the alias list for one recipe.
//
// Validated against every OTHER name on the node, which is a wider check than
// alias-versus-alias. A name that collides with another model's recipe id or
// with its served_as is just as ambiguous - the gateway resolves aliases first
// and ids second, so such a name would silently shadow a model rather than
// fail, and shadowing is the failure mode worth refusing outright.
func (m *Manager) Set(recipeID string, names []string) error {
	recipes, err := m.Cat.List()
	if err != nil {
		return err
	}
	var known bool
	for _, r := range recipes {
		if r.ID == recipeID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("alias: no recipe %q", recipeID)
	}

	clean, err := normalise(names)
	if err != nil {
		return err
	}

	taken, err := m.taken(recipes, recipeID)
	if err != nil {
		return err
	}
	for _, n := range clean {
		if owner, ok := taken[strings.ToLower(n)]; ok {
			return fmt.Errorf("alias: %q already reaches %s; aliases must be unique across models", n, owner)
		}
	}

	// Own names too: an alias equal to this recipe's own id or served_as is a
	// no-op that reads as a working alias, which is worse than a refusal.
	for _, r := range recipes {
		if r.ID != recipeID {
			continue
		}
		self := append([]string{r.ID}, r.ServedAs...)
		for _, n := range clean {
			for _, s := range self {
				if strings.EqualFold(n, s) {
					return fmt.Errorf("alias: %s already answers to %q", recipeID, n)
				}
			}
		}
	}

	if len(clean) == 0 {
		return m.Clear(recipeID)
	}
	return m.Store.WriteYAML(store.KindAlias, recipeID, set{RecipeID: recipeID, Names: clean})
}

// Clear removes every alias for a recipe. Absent is not an error.
func (m *Manager) Clear(recipeID string) error {
	// os.IsNotExist rather than matching the message: Delete returns the raw
	// *PathError, and an error string is not an API.
	if err := m.Store.Delete(store.KindAlias, recipeID); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// taken maps every name already reachable on this node to the model it reaches,
// excluding one recipe so that recipe can keep its own aliases when its list is
// rewritten.
func (m *Manager) taken(recipes []recipe.Recipe, except string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range recipes {
		if r.ID == except {
			continue
		}
		out[strings.ToLower(r.ID)] = r.ID
		for _, s := range r.ServedAs {
			out[strings.ToLower(s)] = r.ID
		}
	}
	all, err := m.All()
	if err != nil {
		return nil, err
	}
	for id, names := range all {
		if id == except {
			continue
		}
		for _, n := range names {
			out[strings.ToLower(n)] = id
		}
	}
	return out, nil
}

// normalise trims, drops blanks, and refuses anything that cannot work as a
// model name in a request body.
func normalise(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if strings.ContainsAny(n, " \t\n\"'/\\") {
			return nil, fmt.Errorf("alias: %q contains a character a model name cannot carry", n)
		}
		if len(n) > 64 {
			return nil, errors.New("alias: name is longer than 64 characters")
		}
		// Case-insensitive, because that is how the gateway resolves. Two
		// aliases differing only in case would be one name that appears twice
		// in /v1/models.
		if seen[strings.ToLower(n)] {
			return nil, fmt.Errorf("alias: %q is listed twice", n)
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
