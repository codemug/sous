package observe

import (
	"math"
	"os"
	"strings"
	"testing"
)

// The fixture is the real 2026-08-15 boot of the NVFP4 build, verbatim.
func TestParsesRealNVFP4BootLog(t *testing.T) {
	f, err := os.Open("testdata/qwen38-nvfp4.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := ParseBootLog("qwen38", f)

	if math.Abs(got.WeightsGiB-24.87) > 0.001 {
		t.Fatalf("weights: want 24.87, got %.3f", got.WeightsGiB)
	}
	// Comma-grouped. Parsing this as 352 would understate KV by 1000x and
	// nothing downstream would notice.
	if got.KVTokens != 352000 {
		t.Fatalf("kv tokens: want 352000, got %d", got.KVTokens)
	}
	if math.Abs(got.LoadSeconds-162.617841) > 0.001 {
		t.Fatalf("load seconds: want 162.617841, got %.6f", got.LoadSeconds)
	}
	if got.Backend != "FLASHINFER" {
		t.Fatalf("backend: want FLASHINFER, got %q", got.Backend)
	}
	// The silent throughput cliff: fp8 KV narrows the backend choice to
	// FlashInfer, which with spec-decode drops FULL graphs. Nothing errors.
	if got.CUDAGraphs != "PIECEWISE" {
		t.Fatalf("cuda graphs: want PIECEWISE, got %q", got.CUDAGraphs)
	}
	if math.Abs(got.KVGiB-45.67) > 0.001 {
		t.Fatalf("kv gib: want 45.67, got %.3f", got.KVGiB)
	}
	// 45.67 GiB over 352,000 tokens = 136 KiB/token.
	if math.Abs(got.KVKiBPerToken-136.0) > 1.0 {
		t.Fatalf("kv per token: want ~136, got %.1f", got.KVKiBPerToken)
	}
	if math.Abs(got.MaxConcurrency-4.55) > 0.01 {
		t.Fatalf("max concurrency: want 4.55, got %.2f", got.MaxConcurrency)
	}
}

// The FP8 build captured PIECEWISE=5 AND FULL=3. Reporting that as plain
// PIECEWISE would erase the distinction that explains the throughput
// difference between the two builds.
func TestFullGraphsAreDistinguishedFromPiecewiseOnly(t *testing.T) {
	log := `INFO Profiling CUDA graph memory: PIECEWISE=5 (largest=24), FULL=3 (largest=9)
INFO Model loading took 28.95 GiB memory and 235.517844 seconds
`
	got := ParseBootLog("qwen38-fp8", strings.NewReader(log))
	if got.CUDAGraphs != "FULL_AND_PIECEWISE" {
		t.Fatalf("want FULL_AND_PIECEWISE, got %q", got.CUDAGraphs)
	}
}

func TestPartialLogYieldsPartialObservation(t *testing.T) {
	got := ParseBootLog("x", strings.NewReader(
		"INFO Model loading took 10.00 GiB memory and 5.0 seconds\n"))
	if got.WeightsGiB != 10 {
		t.Fatalf("want 10, got %.2f", got.WeightsGiB)
	}
	if got.KVTokens != 0 {
		t.Fatalf("absent fields must stay zero, got %d", got.KVTokens)
	}
	if got.KVKiBPerToken != 0 {
		t.Fatal("must not divide by a missing token count")
	}
}

func TestRecipeIDIsCarried(t *testing.T) {
	got := ParseBootLog("kokoro", strings.NewReader(""))
	if got.RecipeID != "kokoro" {
		t.Fatalf("recipe id lost: %q", got.RecipeID)
	}
}
