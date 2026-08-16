package recipe

import (
	"slices"
	"testing"
)

func base() Recipe {
	return Recipe{
		ID: "qwen38", Kind: KindVLLM, Modality: ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai:x",
		ServedAs: []string{"qwen38", "qwen"},
		Declared: Footprint{WeightsGiB: 24.87, KVGiB: 45.67},
		Args:     map[string]any{"gpu-memory-utilization": 0.62, "max-model-len": 262144},
		Env:      map[string]string{"HF_HOME": "/root/.cache/huggingface"},
	}
}

func fields(d Diff) []string {
	out := make([]string, 0, len(d.Changes))
	for _, c := range d.Changes {
		out = append(out, c.Field)
	}
	return out
}

func TestIdenticalRecipesDiffToNothing(t *testing.T) {
	if d := Compare(base(), base()); !d.Empty() {
		t.Errorf("identical recipes produced %d change(s): %v", len(d.Changes), fields(d))
	}
}

// The distinction the type exists for: an image change cannot be applied to a
// running container, so it must be reported as needing a restart.
func TestImageChangeNeedsRestart(t *testing.T) {
	b := base()
	n := base()
	n.Image = "vllm/vllm-openai:different"

	d := Compare(b, n)
	if !d.NeedsRestart() {
		t.Fatal("an image change was reported as applying without a restart")
	}
	if got := d.RestartFields(); !slices.Contains(got, "image") {
		t.Errorf("restart fields = %v, want image", got)
	}
}

// The other half, and the one that saves needless outages: editing prose must
// not propose reloading tens of GiB.
func TestNotesAndArchivedDoNotNeedRestart(t *testing.T) {
	b := base()
	n := base()
	n.Notes = "measured 2026-08-16: 23.97 tok/s structured"
	n.Archived = true

	d := Compare(b, n)
	if d.Empty() {
		t.Fatal("notes and archived changes were not reported at all")
	}
	if d.NeedsRestart() {
		t.Errorf("catalog-only changes demanded a restart: %v", d.RestartFields())
	}
}

// A corrected footprint estimate feeds the NEXT capacity plan; the running
// container already allocated whatever it allocated.
func TestDeclaredFootprintDoesNotNeedRestart(t *testing.T) {
	b := base()
	n := base()
	n.Declared.KVGiB = 11.28

	d := Compare(b, n)
	if !slices.Contains(fields(d), "declared.kv_gib") {
		t.Fatalf("footprint change not reported: %v", fields(d))
	}
	if d.NeedsRestart() {
		t.Errorf("a declared-footprint change demanded a restart: %v", d.RestartFields())
	}
}

func TestArgAddedRemovedAndChanged(t *testing.T) {
	b := base()
	n := base()
	n.Args = map[string]any{
		"gpu-memory-utilization": 0.30,     // changed
		"tool-call-parser":       "hermes", // added
		// max-model-len removed
	}
	d := Compare(b, n)
	got := fields(d)
	for _, want := range []string{"args.gpu-memory-utilization", "args.tool-call-parser", "args.max-model-len"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if !d.NeedsRestart() {
		t.Error("argument changes must need a restart; they are container config")
	}
}

func TestEnvChangesAreReportedAndNeedRestart(t *testing.T) {
	b := base()
	n := base()
	n.Env = map[string]string{
		"HF_HOME":              "/root/.cache/huggingface",
		"CUDA_VISIBLE_DEVICES": "0",
	}
	d := Compare(b, n)
	if !slices.Contains(fields(d), "env.CUDA_VISIBLE_DEVICES") {
		t.Errorf("env addition not reported: %v", fields(d))
	}
	if !d.NeedsRestart() {
		t.Error("env changes must need a restart")
	}
}

// Go randomises map iteration. An unstable diff is unreadable in a UI and
// impossible to test, so ordering must be deterministic.
func TestChangeOrderIsStable(t *testing.T) {
	b := base()
	n := base()
	n.Args = map[string]any{"zulu": 1, "alpha": 2, "mike": 3}

	first := fields(Compare(b, n))
	for i := 0; i < 20; i++ {
		if got := fields(Compare(b, n)); !slices.Equal(got, first) {
			t.Fatalf("diff order changed between runs:\n  %v\n  %v", first, got)
		}
	}
}

// 0.30 and 0.3 are the same number. Reporting them as a change would train
// operators to ignore the diff.
func TestEquivalentFloatsAreNotAChange(t *testing.T) {
	b := base()
	b.Declared.WeightsGiB = 24.870
	n := base()
	n.Declared.WeightsGiB = 24.87
	if d := Compare(b, n); !d.Empty() {
		t.Errorf("24.870 vs 24.87 reported as %v", fields(d))
	}
}

// Notes in this catalog run to dozens of lines. Printing two in full would
// bury every other change in the view.
func TestLongNotesAreTruncatedInTheDiff(t *testing.T) {
	b := base()
	n := base()
	n.Notes = string(make([]byte, 500))
	for _, c := range Compare(b, n).Changes {
		if c.Field == "notes" && len(c.New) > 64 {
			t.Errorf("notes value not truncated: %d chars", len(c.New))
		}
	}
}
