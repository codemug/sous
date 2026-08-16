package recipe

import (
	"fmt"
	"sort"
	"strings"
)

// Change is one field-level difference between two recipes.
//
// Restart is the field that earns this type its place. Listing what changed is
// easy and not very useful on its own; the question an operator actually has
// is "do I have to restart the model for this to matter", and answering it
// wrongly in either direction is expensive. Saying yes when the answer is no
// means a needless multi-minute reload of tens of GiB. Saying no when the
// answer is yes leaves a container running configuration that no longer
// matches its recipe, which is the silent-drift failure this whole project
// exists to avoid.
type Change struct {
	Field   string `json:"field"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Restart bool   `json:"restart"`
}

// Diff is the ordered set of changes between two recipes.
type Diff struct {
	Changes []Change `json:"changes"`
}

func (d Diff) Empty() bool { return len(d.Changes) == 0 }

// NeedsRestart reports whether any change requires recreating the container.
func (d Diff) NeedsRestart() bool {
	for _, c := range d.Changes {
		if c.Restart {
			return true
		}
	}
	return false
}

// RestartFields lists just the fields forcing a restart, so a UI can say WHICH
// change costs the outage rather than only that one does.
func (d Diff) RestartFields() []string {
	var out []string
	for _, c := range d.Changes {
		if c.Restart {
			out = append(out, c.Field)
		}
	}
	return out
}

// Compare produces the field-level difference from old to new.
//
// Everything that ends up in the container's configuration forces a restart,
// because Docker cannot change those on a running container. Notes and
// Archived are catalog bookkeeping the running process never sees, so they do
// not.
func Compare(old, new Recipe) Diff {
	var d Diff
	add := func(field, o, n string, restart bool) {
		if o != n {
			d.Changes = append(d.Changes, Change{Field: field, Old: o, New: n, Restart: restart})
		}
	}

	add("id", old.ID, new.ID, true)
	add("kind", string(old.Kind), string(new.Kind), true)
	add("modality", string(old.Modality), string(new.Modality), true)
	add("model", old.Model, new.Model, true)
	add("image", old.Image, new.Image, true)
	add("build", old.Build, new.Build, true)
	add("served_as", strings.Join(old.ServedAs, ","), strings.Join(new.ServedAs, ","), true)
	add("entrypoint", strings.Join(old.Entrypoint, " "), strings.Join(new.Entrypoint, " "), true)

	// Declared footprint does NOT force a restart: it feeds capacity planning
	// for the NEXT deploy, and the running container's real usage is whatever
	// it already allocated. Restarting to adopt a corrected estimate would
	// change nothing about the process.
	add("declared.weights_gib", num(old.Declared.WeightsGiB), num(new.Declared.WeightsGiB), false)
	add("declared.kv_gib", num(old.Declared.KVGiB), num(new.Declared.KVGiB), false)

	diffAnyMap(&d, "args", old.Args, new.Args)
	diffStrMap(&d, "env", old.Env, new.Env)

	// Catalog bookkeeping. The running process never sees either.
	add("notes", short(old.Notes), short(new.Notes), false)
	add("archived", fmt.Sprint(old.Archived), fmt.Sprint(new.Archived), false)

	return d
}

// keysOf returns the union of both maps' keys, sorted, so a diff of the same
// pair is always reported in the same order. Map iteration order in Go is
// randomised, and an unstable diff is unreadable and untestable.
func keysOf[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func diffAnyMap(d *Diff, prefix string, a, b map[string]any) {
	for _, k := range keysOf(a, b) {
		ov, oOK := a[k]
		nv, nOK := b[k]
		o, n := "", ""
		if oOK {
			o = fmt.Sprint(ov)
		}
		if nOK {
			n = fmt.Sprint(nv)
		}
		if o != n {
			d.Changes = append(d.Changes, Change{
				Field: prefix + "." + k, Old: o, New: n, Restart: true})
		}
	}
}

func diffStrMap(d *Diff, prefix string, a, b map[string]string) {
	for _, k := range keysOf(a, b) {
		if a[k] != b[k] {
			d.Changes = append(d.Changes, Change{
				Field: prefix + "." + k, Old: a[k], New: b[k], Restart: true})
		}
	}
}

// num formats without trailing zeros, so 0.30 and 0.3 do not read as a change.
func num(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", f), "0"), ".")
}

// short truncates prose. Notes run to dozens of lines in this catalog, and a
// diff view that prints two of them in full buries every other change.
func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 60 {
		return s
	}
	return s[:57] + "..."
}
