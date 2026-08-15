package overlay

import (
	"strings"
	"testing"

	"github.com/codemug/sous/internal/recipe"
)

func base() recipe.Recipe {
	return recipe.Recipe{
		ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai@sha256:aaa",
		Args: map[string]any{
			"gpu-memory-utilization": 0.62,
			"max-model-len":          131072,
			"kv-cache-dtype":         "fp8",
		},
		Notes: "upstream notes",
	}
}

func TestApplyOverridesOnlyPatchedKeys(t *testing.T) {
	got := Apply(base(), Patch{Args: map[string]any{"gpu-memory-utilization": 0.55}})
	if got.Args["gpu-memory-utilization"] != 0.55 {
		t.Fatalf("patch not applied: %v", got.Args)
	}
	if got.Args["max-model-len"] != 131072 {
		t.Fatalf("untouched key changed: %v", got.Args)
	}
	if got.Args["kv-cache-dtype"] != "fp8" {
		t.Fatalf("untouched key changed: %v", got.Args)
	}
}

func TestApplyDoesNotMutateBase(t *testing.T) {
	b := base()
	Apply(b, Patch{Args: map[string]any{"gpu-memory-utilization": 0.1}})
	if b.Args["gpu-memory-utilization"] != 0.62 {
		t.Fatal("Apply mutated the base recipe")
	}
}

func TestApplyEmptyPatchIsIdentity(t *testing.T) {
	got := Apply(base(), Patch{})
	if got.Args["gpu-memory-utilization"] != 0.62 || got.Image != base().Image {
		t.Fatalf("empty patch changed the recipe: %+v", got)
	}
}

func TestApplyOverridesScalarFields(t *testing.T) {
	got := Apply(base(), Patch{Fields: map[string]any{"image": "my/fork:v1"}})
	if got.Image != "my/fork:v1" {
		t.Fatalf("scalar field not applied: %q", got.Image)
	}
	if got.Model != base().Model {
		t.Fatal("unrelated scalar changed")
	}
}

// THE CASE THAT JUSTIFIES THE WHOLE OVERLAY MODEL: upstream improves a field
// you never touched, and you keep both.
func TestMergeTakesUpstreamChangeToAnUntouchedKey(t *testing.T) {
	up := base()
	up.Args["max-model-len"] = 262144 // upstream raised the context

	p := Patch{Base: "sha1", Args: map[string]any{"gpu-memory-utilization": 0.55}}

	got, conflicts := Merge(base(), up, p)
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflict, got %v", conflicts)
	}
	if got.Args["max-model-len"] != 262144 {
		t.Fatal("upstream improvement lost")
	}
	if got.Args["gpu-memory-utilization"] != 0.55 {
		t.Fatal("local override lost")
	}
}

func TestMergeConflictsOnlyOnTheSameKey(t *testing.T) {
	up := base()
	up.Args["gpu-memory-utilization"] = 0.75 // upstream changed the same key

	p := Patch{Base: "sha1", Args: map[string]any{"gpu-memory-utilization": 0.55}}

	got, conflicts := Merge(base(), up, p)
	if len(conflicts) != 1 {
		t.Fatalf("want exactly one conflict, got %v", conflicts)
	}
	c := conflicts[0]
	if c.Key != "gpu-memory-utilization" {
		t.Fatalf("wrong key: %q", c.Key)
	}
	// All three sides must be reportable, or the table cannot be rendered.
	if c.Base != 0.62 || c.Upstream != 0.75 || c.Yours != 0.55 {
		t.Fatalf("conflict must carry all three sides: %+v", c)
	}
	// Nothing is silently chosen: yours stands until resolved.
	if got.Args["gpu-memory-utilization"] != 0.55 {
		t.Fatal("a conflict must not silently adopt upstream")
	}
}

// Agreeing is not conflicting.
func TestNoConflictWhenUpstreamAdoptsYourValue(t *testing.T) {
	up := base()
	up.Args["gpu-memory-utilization"] = 0.55

	p := Patch{Base: "sha1", Args: map[string]any{"gpu-memory-utilization": 0.55}}
	_, conflicts := Merge(base(), up, p)
	if len(conflicts) != 0 {
		t.Fatalf("upstream adopting your value is not a conflict: %v", conflicts)
	}
}

// A no-op override is not an opinion worth conflicting over.
func TestNoConflictWhenPatchRepeatsBase(t *testing.T) {
	up := base()
	up.Args["gpu-memory-utilization"] = 0.75

	p := Patch{Base: "sha1", Args: map[string]any{"gpu-memory-utilization": 0.62}}
	got, conflicts := Merge(base(), up, p)
	if len(conflicts) != 0 {
		t.Fatalf("a patch equal to base is not an override: %v", conflicts)
	}
	if got.Args["gpu-memory-utilization"] != 0.75 {
		t.Fatal("upstream should win when the patch expressed no opinion")
	}
}

func TestMergeAddsNewUpstreamKeys(t *testing.T) {
	up := base()
	up.Args["enable-prefix-caching"] = true

	got, conflicts := Merge(base(), up, Patch{Base: "sha1"})
	if len(conflicts) != 0 {
		t.Fatalf("a new upstream key is not a conflict: %v", conflicts)
	}
	if got.Args["enable-prefix-caching"] != true {
		t.Fatal("new upstream key lost")
	}
}

// notes is the one free-text field, so upstream notes append rather than
// fighting a scalar merge.
func TestNotesAppendRatherThanConflict(t *testing.T) {
	up := base()
	up.Notes = "upstream notes\nplus a new warning about the pinned digest"

	p := Patch{Base: "sha1", Fields: map[string]any{"notes": "my local note"}}
	got, conflicts := Merge(base(), up, p)
	for _, c := range conflicts {
		if c.Key == "notes" {
			t.Fatal("notes must not conflict")
		}
	}
	if !strings.Contains(got.Notes, "my local note") {
		t.Fatal("local note lost")
	}
	if !strings.Contains(got.Notes, "pinned digest") {
		t.Fatal("upstream note lost")
	}
}

func TestScalarFieldConflict(t *testing.T) {
	up := base()
	up.Image = "vllm/vllm-openai@sha256:bbb"

	p := Patch{Base: "sha1", Fields: map[string]any{"image": "my/fork:v1"}}
	_, conflicts := Merge(base(), up, p)
	if len(conflicts) != 1 || conflicts[0].Key != "image" {
		t.Fatalf("want an image conflict, got %v", conflicts)
	}
}

// Deterministic output: the conflict table must not reorder between runs, or
// a reviewer cannot tell a real change from map iteration.
func TestMergeIsDeterministic(t *testing.T) {
	up := base()
	up.Args["gpu-memory-utilization"] = 0.75
	up.Args["kv-cache-dtype"] = "auto"
	up.Image = "vllm/vllm-openai@sha256:bbb"

	p := Patch{Base: "sha1",
		Args:   map[string]any{"gpu-memory-utilization": 0.55, "kv-cache-dtype": "fp8x"},
		Fields: map[string]any{"image": "my/fork:v1"},
	}
	_, first := Merge(base(), up, p)
	if len(first) != 3 {
		t.Fatalf("want 3 conflicts, got %d: %v", len(first), first)
	}
	for range 5 {
		_, again := Merge(base(), up, p)
		for i := range first {
			if first[i].Key != again[i].Key {
				t.Fatalf("conflict order is not stable: %v vs %v", first, again)
			}
		}
	}
	// Sorted by key so the table reads predictably.
	if first[0].Key != "gpu-memory-utilization" || first[1].Key != "image" {
		t.Fatalf("conflicts not sorted by key: %v", first)
	}
}

func TestHasOverride(t *testing.T) {
	if (Patch{}).HasOverride() {
		t.Fatal("an empty patch is not an override")
	}
	if !(Patch{Args: map[string]any{"a": 1}}).HasOverride() {
		t.Fatal("an args patch is an override")
	}
	if !(Patch{Fields: map[string]any{"image": "x"}}).HasOverride() {
		t.Fatal("a fields patch is an override")
	}
}
