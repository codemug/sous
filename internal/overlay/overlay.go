// Package overlay layers local edits on top of a recipe from a git source.
//
// Mirrors are never written to, so a fetch is always a clean fast-forward and
// the mirror stays a faithful copy of the repo. Your changes live here instead,
// as a sparse patch plus the base sha it was authored against - which is what
// gives a fetch all three sides of a real merge rather than last-write-wins.
//
// THE MERGE IS FIELD-LEVEL, NOT LINE-LEVEL, and that is the property that makes
// an overlay model tractable here at all. A recipe is a flat args map and a few
// scalars, so upstream raising max-model-len while you overrode
// gpu-memory-utilization merges silently and you keep the improvement. Only a
// collision on the same key needs a human, and it renders as a table rather
// than a text merge.
package overlay

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codemug/sous/internal/recipe"
)

// Patch is sparse: it holds only what you changed.
type Patch struct {
	Source string `yaml:"source" json:"source"`
	Recipe string `yaml:"recipe" json:"recipe"`
	// Base is the upstream sha this patch was authored against. Without it
	// there is no third side, and a fetch can only guess.
	Base   string         `yaml:"base" json:"base"`
	Args   map[string]any `yaml:"args,omitempty" json:"args,omitempty"`
	Fields map[string]any `yaml:"fields,omitempty" json:"fields,omitempty"`
}

func (p Patch) HasOverride() bool { return len(p.Args) > 0 || len(p.Fields) > 0 }

// Conflict carries all three sides so the UI can show what changed under you.
type Conflict struct {
	Key      string `json:"key"`
	Base     any    `json:"base"`
	Upstream any    `json:"upstream"`
	Yours    any    `json:"yours"`
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: base=%v upstream=%v yours=%v", c.Key, c.Base, c.Upstream, c.Yours)
}

// Apply writes the patch over a copy of base.
func Apply(base recipe.Recipe, p Patch) recipe.Recipe {
	out := base
	out.Args = make(map[string]any, len(base.Args)+len(p.Args))
	for k, v := range base.Args {
		out.Args[k] = v
	}
	for k, v := range p.Args {
		out.Args[k] = v
	}
	for k, v := range p.Fields {
		setField(&out, k, v)
	}
	return out
}

// Merge performs a three-way merge of an upstream change against a patch
// authored on an older base.
//
// Per key:
//   - changed only upstream            -> take upstream
//   - changed only by the patch        -> take the patch
//   - both changed to the SAME value   -> take it; agreeing is not conflicting
//   - the patch equals base            -> no opinion expressed, upstream wins
//   - both changed differently         -> keep yours, record a Conflict
//
// A conflict never silently adopts upstream: the local value stands until
// somebody resolves it, because a value that quietly changed under a running
// deployment is the failure this whole mechanism exists to avoid.
func Merge(base, upstream recipe.Recipe, p Patch) (recipe.Recipe, []Conflict) {
	out := upstream
	out.Args = make(map[string]any, len(upstream.Args)+len(p.Args))
	for k, v := range upstream.Args {
		out.Args[k] = v
	}

	var conflicts []Conflict

	for k, mine := range p.Args {
		baseVal, hadBase := base.Args[k]
		upVal, hadUp := upstream.Args[k]

		// The patch expressed no opinion: it repeats base.
		if hadBase && eq(mine, baseVal) {
			continue
		}
		upstreamChanged := (hadUp != hadBase) || !eq(upVal, baseVal)
		if upstreamChanged && !eq(upVal, mine) {
			conflicts = append(conflicts, Conflict{
				Key: k, Base: baseVal, Upstream: upVal, Yours: mine,
			})
		}
		out.Args[k] = mine
	}

	for k, mine := range p.Fields {
		// notes is free text and is the one field where a merge is genuinely
		// awkward, so upstream notes append instead of fighting.
		if k == "notes" {
			out.Notes = appendNotes(upstream.Notes, asString(mine))
			continue
		}
		baseVal := getField(base, k)
		upVal := getField(upstream, k)
		if eq(mine, baseVal) {
			continue
		}
		if !eq(upVal, baseVal) && !eq(upVal, mine) {
			conflicts = append(conflicts, Conflict{
				Key: k, Base: baseVal, Upstream: upVal, Yours: mine,
			})
		}
		setField(&out, k, mine)
	}

	// Stable order, so a reviewer can tell a real change from map iteration.
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Key < conflicts[j].Key })
	return out, conflicts
}

// eq compares by formatted value. YAML round-trips turn 0.62 into a float and
// 262144 into an int inconsistently across sources, and comparing the rendered
// form avoids reporting a conflict between two spellings of one number.
func eq(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func appendNotes(upstream, mine string) string {
	switch {
	case strings.TrimSpace(mine) == "":
		return upstream
	case strings.TrimSpace(upstream) == "":
		return mine
	case strings.Contains(upstream, mine):
		return upstream
	}
	return upstream + "\n\n--- local ---\n" + mine
}

// getField and setField cover the scalar fields worth overriding. This is a
// deliberately short list: anything structural (kind, id) is not an override,
// it is a different recipe.
func getField(r recipe.Recipe, key string) any {
	switch key {
	case "image":
		return r.Image
	case "model":
		return r.Model
	case "notes":
		return r.Notes
	case "build":
		return r.Build
	}
	return nil
}

func setField(r *recipe.Recipe, key string, v any) {
	switch key {
	case "image":
		r.Image = asString(v)
	case "model":
		r.Model = asString(v)
	case "notes":
		r.Notes = asString(v)
	case "build":
		r.Build = asString(v)
	}
}
