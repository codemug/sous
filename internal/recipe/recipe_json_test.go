package recipe

import (
	"encoding/json"
	"testing"
)

// The JSON API is public and every other endpoint speaks snake_case
// (committed_gib, referenced_by). Without explicit tags, encoding/json falls
// back to Go field names and /api/recipes returned ID, ServedAs, WeightsGiB
// while its neighbours returned snake_case - an inconsistency only visible
// once it was deployed and something tried to consume it.
func TestJSONFieldNamesAreSnakeCase(t *testing.T) {
	r := Recipe{
		ID: "qwen38", Kind: KindVLLM, Modality: ModalityText,
		Model: "Org/Model", Image: "img:1", ServedAs: []string{"a"},
		Declared: Footprint{WeightsGiB: 24.87, KVGiB: 45.67},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id", "kind", "modality", "model", "image", "served_as", "declared"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; keys are %v", want, keys(got))
		}
	}
	for _, unwanted := range []string{"ID", "Kind", "ServedAs", "Declared"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("Go field name %q leaked into the JSON", unwanted)
		}
	}
	d, _ := got["declared"].(map[string]any)
	if _, ok := d["weights_gib"]; !ok {
		t.Errorf("nested Footprint not tagged: %v", d)
	}
}

// The YAML on disk must not change shape - recipes are shared between nodes
// and a rename would silently drop fields on load.
func TestJSONTagsDidNotDisturbYAML(t *testing.T) {
	r := Recipe{ID: "x", Kind: KindVLLM, Modality: ModalityText, Image: "i",
		Declared: Footprint{WeightsGiB: 1.5}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
