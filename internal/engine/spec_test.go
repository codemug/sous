package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/codemug/sous/internal/recipe"
)

func TestVLLMSpecInjectsPortAndFlags(t *testing.T) {
	r := recipe.Recipe{
		ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Model: "Inferact/Qwen3.8-27B-NVFP4", Image: "vllm/vllm-openai@sha256:abc",
		ServedAs: []string{"qwen38", "qwen"},
		Args: map[string]any{"max-model-len": 262144, "kv-cache-dtype": "fp8",
			"enable-prefix-caching": true},
		Declared: recipe.Footprint{WeightsGiB: 24.87},
	}
	s, err := BuildSpec(r, 8000, "/models")
	if err != nil {
		t.Fatal(err)
	}
	if s.ContainerPort != 8000 {
		t.Fatalf("want container port 8000, got %d", s.ContainerPort)
	}
	if !slices.Contains(s.Cmd, "--port=8000") {
		t.Fatalf("--port must be injected, got %v", s.Cmd)
	}
	if !slices.Contains(s.Cmd, "--model=Inferact/Qwen3.8-27B-NVFP4") {
		t.Fatalf("model flag missing: %v", s.Cmd)
	}
	if !slices.Contains(s.Cmd, "--max-model-len=262144") {
		t.Fatalf("int arg not rendered: %v", s.Cmd)
	}
	// Boolean true renders as a bare flag: vLLM rejects
	// --enable-prefix-caching=true.
	if !slices.Contains(s.Cmd, "--enable-prefix-caching") {
		t.Fatalf("bool arg not rendered as bare flag: %v", s.Cmd)
	}
	if slices.Contains(s.Cmd, "--enable-prefix-caching=true") {
		t.Fatalf("bool arg rendered with =true: %v", s.Cmd)
	}
	joined := strings.Join(s.Cmd, " ")
	if !strings.Contains(joined, "--served-model-name qwen38 qwen") {
		t.Fatalf("served names missing or malformed: %v", s.Cmd)
	}
	if !s.GPU {
		t.Fatal("vllm recipes must request the GPU")
	}
}

func TestFalseBoolIsOmitted(t *testing.T) {
	r := recipe.Recipe{
		ID: "x", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
		Image: "i", Args: map[string]any{"trust-remote-code": false},
	}
	s, _ := BuildSpec(r, 8000, "/models")
	if slices.Contains(s.Cmd, "--trust-remote-code") {
		t.Fatalf("false bool must not emit a flag: %v", s.Cmd)
	}
}

func TestArgsAreDeterministic(t *testing.T) {
	r := recipe.Recipe{
		ID: "x", Kind: recipe.KindVLLM, Modality: recipe.ModalityText, Image: "i",
		Args: map[string]any{"z": 1, "a": 2, "m": 3},
	}
	first, _ := BuildSpec(r, 8000, "/models")
	for range 5 {
		again, _ := BuildSpec(r, 8000, "/models")
		if !slices.Equal(first.Cmd, again.Cmd) {
			t.Fatalf("map iteration leaked into the command:\n%v\n%v", first.Cmd, again.Cmd)
		}
	}
}

// The trap that cost 46 crash loops: the vLLM base image sets
// ENTRYPOINT ["vllm","serve"], so a derived service's command becomes
// arguments to vLLM unless the entrypoint is overridden explicitly.
func TestTransformersOverridesEntrypoint(t *testing.T) {
	r := recipe.Recipe{
		ID: "asr", Kind: recipe.KindTransformers, Modality: recipe.ModalityASR,
		Image: "fleet/nemotron-asr:0.6b", Build: "stacks/nemotron-asr",
		Entrypoint: []string{"python3", "-m", "uvicorn", "app:app",
			"--host", "0.0.0.0", "--port", "8000"},
		Declared: recipe.Footprint{WeightsGiB: 1.19},
	}
	s, err := BuildSpec(r, 8006, "/models")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entrypoint) == 0 {
		t.Fatal("transformers spec must set an explicit entrypoint")
	}
	if len(s.Cmd) != 0 {
		t.Fatalf("transformers spec must not also set Cmd, got %v", s.Cmd)
	}
	if !s.GPU {
		t.Fatal("asr has weights on the GPU and must request it")
	}
}

func TestContainerKindPassesImageThrough(t *testing.T) {
	r := recipe.Recipe{
		ID: "kokoro", Kind: recipe.KindContainer, Modality: recipe.ModalityTTS,
		Image: "ghcr.io/remsky/kokoro-fastapi-cpu:latest",
	}
	s, err := BuildSpec(r, 8004, "/models")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cmd) != 0 || len(s.Entrypoint) != 0 {
		t.Fatal("container kind must not synthesise a command")
	}
	// Kokoro is CPU-only by design: keeping TTS off the GPU leaves the
	// bandwidth for the LLM, which is what decode is bound by.
	if s.GPU {
		t.Fatal("kokoro is CPU-only; it must not request the GPU")
	}
	if s.HostPort != 8004 {
		t.Fatalf("host port not carried: %d", s.HostPort)
	}
}

func TestNameIsDerivedFromID(t *testing.T) {
	r := recipe.Recipe{ID: "qwen38", Kind: recipe.KindContainer,
		Modality: recipe.ModalityText, Image: "x"}
	s, _ := BuildSpec(r, 9000, "/models")
	if s.Name != "sous-qwen38" {
		t.Fatalf("want sous-qwen38, got %q", s.Name)
	}
}

func TestInvalidRecipeIsRejected(t *testing.T) {
	if _, err := BuildSpec(recipe.Recipe{ID: "Bad"}, 8000, "/models"); err == nil {
		t.Fatal("BuildSpec accepted an invalid recipe")
	}
}

func TestModelDirIsBindMounted(t *testing.T) {
	r := recipe.Recipe{ID: "x", Kind: recipe.KindContainer,
		Modality: recipe.ModalityText, Image: "i"}
	s, _ := BuildSpec(r, 8000, "/srv/models")
	found := false
	for _, b := range s.Binds {
		if strings.HasPrefix(b, "/srv/models:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("model dir not bind mounted: %v", s.Binds)
	}
}
