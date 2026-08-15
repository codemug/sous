package catalog

import (
	"testing"

	"github.com/codemug/sous/internal/recipe"
)

// Adopting a running service means the recipe must reproduce it. These env
// vars are not cosmetic: TORCH_CUDA_ARCH_LIST and CUTE_DSL_ARCH gate kernel
// JIT on GB10's sm_121, and omitting them does not fail loudly - the model
// starts and behaves differently, which is the worst kind of migration bug.
func TestVLLMSeedsCarryTheArchEnv(t *testing.T) {
	for _, r := range Seeds() {
		if r.Kind != recipe.KindVLLM {
			continue
		}
		for _, k := range []string{"TORCH_CUDA_ARCH_LIST", "CUTE_DSL_ARCH"} {
			if r.Env[k] == "" {
				t.Errorf("%s: missing %s", r.ID, k)
			}
		}
	}
}

// Everything that reads a HuggingFace cache needs to be told where it is; the
// bind lands at /root/.cache/huggingface.
func TestSeedsThatUseWeightsSetHFHome(t *testing.T) {
	for _, r := range Seeds() {
		if r.Model == "" {
			continue
		}
		if got := r.Env["HF_HOME"]; got != "/root/.cache/huggingface" {
			t.Errorf("%s: HF_HOME = %q, want the bind target", r.ID, got)
		}
	}
}
