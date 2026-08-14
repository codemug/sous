package recipe

import "testing"

func valid() Recipe {
	return Recipe{
		ID: "qwen38", Kind: KindVLLM, Modality: ModalityText,
		Model:    "Inferact/Qwen3.8-27B-NVFP4",
		Image:    "vllm/vllm-openai@sha256:d5a8e53a",
		Declared: Footprint{WeightsGiB: 24.6, KVGiB: 46},
		Args:     map[string]any{"max-model-len": 262144},
	}
}

func TestValidRecipePasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid recipe rejected: %v", err)
	}
}

func TestRejectsBadIDs(t *testing.T) {
	for _, bad := range []string{"", "Qwen38", "qwen 38", "../x", "a/b", "-lead"} {
		r := valid()
		r.ID = bad
		if err := r.Validate(); err == nil {
			t.Fatalf("accepted bad id %q", bad)
		}
	}
}

// A port in a recipe is a design error, not a typo: placement is a deploy-time
// decision. Two recipes hardcoding :8000 is exactly why archiving one and
// starting the other collided on this node.
func TestRejectsPortArgs(t *testing.T) {
	for _, key := range []string{"port", "host-port", "hostport"} {
		r := valid()
		r.Args = map[string]any{key: 8000}
		if err := r.Validate(); err == nil {
			t.Fatalf("accepted port arg %q in a recipe", key)
		}
	}
}

func TestZeroGPUIsValidForCPUOnlyKinds(t *testing.T) {
	r := valid()
	r.ID, r.Kind, r.Modality = "kokoro", KindContainer, ModalityTTS
	r.Image, r.Model = "ghcr.io/remsky/kokoro-fastapi-cpu:latest", ""
	r.Args = nil
	r.Declared = Footprint{} // Kokoro costs no GPU at all. Zero is a real answer.
	if err := r.Validate(); err != nil {
		t.Fatalf("CPU-only container recipe rejected: %v", err)
	}
}

func TestTransformersRequiresBuildAndEntrypoint(t *testing.T) {
	r := valid()
	r.ID, r.Kind = "asr", KindTransformers
	r.Build, r.Entrypoint = "", nil
	if err := r.Validate(); err == nil {
		t.Fatal("transformers recipe without build context accepted")
	}
	r.Build = "stacks/nemotron-asr"
	r.Entrypoint = []string{"python3", "-m", "uvicorn", "app:app"}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid transformers recipe rejected: %v", err)
	}
}

func TestVLLMRequiresImage(t *testing.T) {
	r := valid()
	r.Image = ""
	if err := r.Validate(); err == nil {
		t.Fatal("vllm recipe without image accepted")
	}
}

func TestRejectsUnknownKindAndModality(t *testing.T) {
	r := valid()
	r.Kind = "sglang"
	if err := r.Validate(); err == nil {
		t.Fatal("accepted unknown kind")
	}
	r = valid()
	r.Modality = "video"
	if err := r.Validate(); err == nil {
		t.Fatal("accepted unknown modality")
	}
}

func TestFootprintTotal(t *testing.T) {
	f := Footprint{WeightsGiB: 24.87, KVGiB: 45.67}
	if got := f.TotalGiB(); got < 70.53 || got > 70.55 {
		t.Fatalf("want 70.54, got %.2f", got)
	}
}
