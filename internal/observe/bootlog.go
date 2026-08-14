// Package observe turns a vLLM boot log into measured truth.
//
// These values are node-local by construction and must never be written back
// into a recipe: 24.87 GiB of weights and 136 KiB/token are facts about this
// box and this engine build, not about the model. Recipes carry declared
// estimates; observations carry what actually happened.
package observe

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

type Observation struct {
	RecipeID      string  `yaml:"recipe_id" json:"recipe_id"`
	WeightsGiB    float64 `yaml:"weights_gib" json:"weights_gib"`
	KVGiB         float64 `yaml:"kv_gib" json:"kv_gib"`
	KVTokens      int     `yaml:"kv_tokens" json:"kv_tokens"`
	KVKiBPerToken float64 `yaml:"kv_kib_per_token" json:"kv_kib_per_token"`
	LoadSeconds   float64 `yaml:"load_seconds" json:"load_seconds"`

	// Backend and CUDAGraphs are first-class because they change performance
	// with no error message. Setting --kv-cache-dtype fp8 narrows the backend
	// candidates from four to two, the engine then picks FlashInfer, and
	// FlashInfer with speculative decoding drops FULL CUDA graphs. Nothing
	// fails; throughput just changes.
	Backend        string  `yaml:"attention_backend,omitempty" json:"attention_backend,omitempty"`
	CUDAGraphs     string  `yaml:"cuda_graphs,omitempty" json:"cuda_graphs,omitempty"`
	MaxConcurrency float64 `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
}

var (
	reLoading = regexp.MustCompile(`Model loading took ([0-9.]+) GiB memory and ([0-9.]+) seconds`)
	reKVSize  = regexp.MustCompile(`GPU KV cache size: ([0-9,]+) tokens`)
	reKVMem   = regexp.MustCompile(`Available KV cache memory: ([0-9.]+) GiB`)
	reConc    = regexp.MustCompile(`Maximum concurrency for [0-9,]+ tokens per request: ([0-9.]+)x`)
	reBackend = regexp.MustCompile(`Using ([A-Z_]+) attention backend`)
	rePiece   = regexp.MustCompile(`Profiling CUDA graph memory: PIECEWISE=\d+`)
	reFull    = regexp.MustCompile(`FULL=\d+`)
)

func ParseBootLog(recipeID string, r io.Reader) Observation {
	o := Observation{RecipeID: recipeID}
	sc := bufio.NewScanner(r)
	// vLLM emits very long lines during multimodal profiling; the default
	// 64 KiB limit truncates them and silently drops later matches.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	sawFull := false
	for sc.Scan() {
		line := sc.Text()
		if m := reLoading.FindStringSubmatch(line); m != nil {
			o.WeightsGiB, _ = strconv.ParseFloat(m[1], 64)
			o.LoadSeconds, _ = strconv.ParseFloat(m[2], 64)
		}
		if m := reKVSize.FindStringSubmatch(line); m != nil {
			// Commas are grouping, not decimals. Without this replace,
			// 352,000 parses as 352.
			o.KVTokens, _ = strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		}
		if m := reKVMem.FindStringSubmatch(line); m != nil {
			o.KVGiB, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := reConc.FindStringSubmatch(line); m != nil {
			o.MaxConcurrency, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := reBackend.FindStringSubmatch(line); m != nil {
			o.Backend = m[1]
		}
		if rePiece.MatchString(line) {
			o.CUDAGraphs = "PIECEWISE"
			if reFull.MatchString(line) {
				sawFull = true
			}
		}
	}
	if sawFull {
		o.CUDAGraphs = "FULL_AND_PIECEWISE"
	}
	if o.KVTokens > 0 && o.KVGiB > 0 {
		o.KVKiBPerToken = o.KVGiB * 1024 * 1024 / float64(o.KVTokens)
	}
	return o
}
