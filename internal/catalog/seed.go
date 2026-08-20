package catalog

import "github.com/codemug/sous/internal/recipe"

// Seeds is the catalog of every model measured on asus-gx10.
//
// Three entries are archived negative results, kept on purpose: qwen38-fp8,
// omni and whisper exist so nobody re-derives why they lost. Declared
// footprints are the measured figures, which is legitimate here because these
// recipes were authored on the node that measured them - a recipe fetched from
// elsewhere would start with estimates and earn observations on first load.
func Seeds() []recipe.Recipe {
	const pinnedVLLM = "vllm/vllm-openai@sha256:d5a8e53ad2534e24b99ba1a2e3f183a213adc0da48ed83166cb75534a5903a17"

	return []recipe.Recipe{
		{
			ID: "qwen38", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "Inferact/Qwen3.8-27B-NVFP4", Image: pinnedVLLM,
			ServedAs: []string{"qwen38", "qwen"},
			Declared: recipe.Footprint{WeightsGiB: 24.87, KVGiB: 45.67},
			Args: map[string]any{
				"tensor-parallel-size": 1, "gpu-memory-utilization": 0.62,
				"max-model-len": 262144, "max-num-seqs": 4, "kv-cache-dtype": "fp8",
				"enable-prefix-caching": true, "max-num-batched-tokens": 8192,
				"reasoning-parser": "qwen3", "enable-auto-tool-choice": true,
				"tool-call-parser":   "qwen3_coder",
				"speculative-config": `{"method":"mtp","num_speculative_tokens":3}`,
			},
			Env: map[string]string{
				// sm_121a is what GB10's Blackwell actually reports, and some
				// kernels gate on this list at JIT/Triton compile time. Omitting
				// them does not fail loudly - the model starts and behaves
				// differently, which is worse.
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "Dense 27B but HYBRID: 48 Gated DeltaNet + 16 full-attention layers\n" +
				"(full_attention_interval 4). The full-attention geometry is heavier than\n" +
				"the 35B MoE it replaced - 16 layers x 4 KV heads vs 10 x 2 - so KV costs\n" +
				"~3.1x per token DESPITE the smaller parameter count. '27B dense is\n" +
				"cheaper' is exactly backwards here.\n\n" +
				"MTP ships in the checkpoint (mtp_num_hidden_layers 1) and is load-bearing:\n" +
				"+123% over no speculation, where the same flag buys qwen3.6 only +13%.\n" +
				"The dense model is more bandwidth-bound, so amortising the weight read\n" +
				"across accepted tokens is worth more. k=3 beats k=2 but the gain is\n" +
				"workload-dependent: +19.6% on structured output, +0.4% on prose.\n\n" +
				"fp8 KV halves KV to 136 KiB/token AND silently changes the attention\n" +
				"backend: candidates drop from four to two, FlashInfer is chosen over\n" +
				"FLASH_ATTN, and FlashInfer with spec-decode forces CUDA graphs off\n" +
				"FULL_AND_PIECEWISE. Measured anyway and NVFP4 still wins.\n\n" +
				"PINNED DIGEST, not a tag: vLLM 0.27.1 crashes FP8 checkpoints on GB10 in\n" +
				"deepgemm with 'Unknown SF transformation'.",
		},
		{
			ID: "qwen36", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "Qwen/Qwen3.6-35B-A3B-FP8", Image: pinnedVLLM,
			ServedAs: []string{"qwen"},
			Declared: recipe.Footprint{WeightsGiB: 35.02, KVGiB: 26},
			Args: map[string]any{
				"tensor-parallel-size": 1, "gpu-memory-utilization": 0.46,
				"kv-cache-memory-bytes": 27917287424, "max-model-len": 262144,
				"max-num-seqs": 4, "kv-cache-dtype": "auto",
				"enable-prefix-caching": true, "max-num-batched-tokens": 8192,
				"reasoning-parser": "qwen3", "enable-auto-tool-choice": true,
				"tool-call-parser":   "qwen3_coder",
				"speculative-config": `{"method":"mtp","num_speculative_tokens":2}`,
			},
			Env: map[string]string{
				// sm_121a is what GB10's Blackwell actually reports, and some
				// kernels gate on this list at JIT/Triton compile time. Omitting
				// them does not fail loudly - the model starts and behaves
				// differently, which is worse.
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "MoE, ~3B active per token: 60.6 tok/s, roughly 4x qwen38 on prose.\n" +
				"Decode is bandwidth-bound and this reads far fewer bytes per token.\n" +
				"88.3 KiB/token KV, 308,736 tokens at the 26 GiB pin. MTP acceptance\n" +
				"81.7%, 1.63 accepted per draft.\n\n" +
				"tool-call-parser is load-bearing: without it tool calls silently stop\n" +
				"working while the model still answers, which reads as the model getting\n" +
				"dumber rather than as a configuration error.",
		},
		{
			ID: "ornith15", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "ornith-ai/Ornith-1.5-35B-A3B-FP8",
			// SAME PINNED DIGEST AS qwen36, deliberately. This model is
			// architecturally identical to it - Qwen3_5MoeForConditionalGeneration,
			// 40 layers, 256 experts, 2 KV heads, head_dim 256, vocab 248320 - so
			// the image that already serves one should serve the other. The only
			// structural difference is how it is quantized.
			Image:    pinnedVLLM,
			ServedAs: []string{"ornith"},
			Declared: recipe.Footprint{WeightsGiB: 36.66, KVGiB: 26},
			Args: map[string]any{
				"enable-auto-tool-choice": true,
				"enable-prefix-caching":   true,
				// 0.48 rather than qwen36's 0.46: same shape, 1.64 GiB more weights.
				"gpu-memory-utilization": 0.48,
				"kv-cache-dtype":         "auto",
				// Pinned to the same 26 GiB as qwen36 so a throughput comparison
				// between them is not secretly a comparison of cache sizes.
				"kv-cache-memory-bytes":  27917287424,
				"max-model-len":          262144,
				"max-num-batched-tokens": 8192,
				"max-num-seqs":           4,
				"reasoning-parser":       "qwen3",
				// MTP ships INSIDE this checkpoint: 785 mtp.* tensors, and the
				// quantization config explicitly excludes "re:.*mtp.*" from being
				// quantized. No separate drafter is needed, unlike DFlash2.
				//
				// k=1, NOT the 2 this shipped with, and not 0. Measured three ways on
				// the code case: none 38.27, k=2 48.07, k=1 55.28 tok/s decode.
				// Speculation is worth +44% here, but the SECOND draft token loses
				// 13% of that back - because "excluded from quantization" means the
				// MTP layer runs in bf16, so every extra draft is a full bf16 MoE
				// forward on a box that is bandwidth-bound at decode. k=2 is
				// dominated by k=1 on every case measured.
				"speculative-config":   `{"method":"mtp","num_speculative_tokens":1}`,
				"tensor-parallel-size": 1,
				// READ FROM ITS chat_template.jinja, not assumed: the template
				// emits "<tool_call>\n<function=NAME>", which is the qwen3_coder
				// shape rather than the JSON-in-tool_call shape hermes parses.
				// Guessing this on a previous model cost an afternoon.
				"tool-call-parser": "qwen3_coder",
			},
			Env: map[string]string{
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
				// vLLM ships 316 tuned MoE tables and not one is for this device, so
				// the hottest loop in a 256-expert model runs the default Triton
				// config. This points the loader at a table generated on this GPU.
				// The models dir is already bind-mounted, so no image rebuild.
				//
				// The filename encodes the shape and is matched exactly:
				// E=256,N=512,device_name=NVIDIA_GB10,dtype=fp8_w8a8.json. N is the
				// intermediate size PER RANK, so a table tuned at benchmark_moe.py's
				// default tp=2 lands at N=256 and is SILENTLY never read - which cost
				// an hour of tuning before anyone noticed.
				"VLLM_TUNED_CONFIG_FOLDER": "/root/.cache/huggingface/moe-configs",
			},
			Notes: "MEASURED 2026-08-19/20 against qwen36, same prompts, same node.\n" +
				"Streamed, so TTFT and decode are separated - an earlier pass used\n" +
				"non-streaming and folded prefill into decode, which hid half of this.\n\n" +
				"                 ttft-short  ttft-16k   decode-code\n" +
				"  qwen36              121ms     395ms     68.19 t/s\n" +
				"  ornith k=1          130ms     435ms     55.28 t/s\n" +
				"  ornith k=2          135ms     419ms     48.07 t/s\n" +
				"  ornith no-spec       87ms     227ms     38.27 t/s\n\n" +
				"TTFT IS NOT THE PROBLEM and needs no work: within 10% of qwen36 at\n" +
				"both prompt lengths, and 16k tokens prefill in 435ms either way. The\n" +
				"whole gap is decode.\n\n" +
				"SPECULATION IS WORTH +44% (38.27 -> 55.28) and the shipped k=2 threw\n" +
				"13% of it away. The MTP layer is EXCLUDED FROM QUANTIZATION by the\n" +
				"checkpoint's own config - 785 bf16 tensors - so each extra draft token\n" +
				"is a full bf16 MoE forward, and the second one does not earn it back.\n\n" +
				"SPECULATION COSTS TTFT, which is the trade to know: 87 -> 130ms short\n" +
				"and 227 -> 435ms at 16k, because the first decode step now drafts too.\n" +
				"If this slot ever becomes latency-critical rather than throughput-\n" +
				"critical, dropping speculation nearly halves time to first token.\n\n" +
				"WHY IT IS STILL BEHIND qwen36 at k=1, from the boot logs: the two pick\n" +
				"DIFFERENT linear kernels for the same architecture -\n" +
				"CutlassFP8ScaledMMLinearKernel here against qwen36's\n" +
				"CutlassFp8BlockScaledMMKernel - which follows from channel-wise plus\n" +
				"dynamic per-token activations rather than block-scaled weights. That\n" +
				"is a property of the checkpoint, not a setting.\n\n" +
				"NOT the reason, ruled out by measurement: KV is identical (308,736\n" +
				"tokens at the same pin, same 10 full-attention layers), CUDA graph\n" +
				"capture is identical (FULL 3 / PIECEWISE 5), and the attention backend\n" +
				"is FLASH_ATTN for both.\n\n" +
				"OPEN, and it would help qwen36 too: both log 'Using default MoE\n" +
				"config. Performance might be sub-optimal!' - 316 tuned MoE tables ship\n" +
				"and none is for GB10, so a 256-expert MoE runs untuned Triton on both.\n" +
				"benchmark_moe.py ships in the image. Untested.\n\n" +
				"THE TRADE, unchanged: fewer tokens per second for the vendor's claimed\n" +
				"coding gains over this exact incumbent - Terminal-Bench 2.1 67.8 vs\n" +
				"52.5, SWE-bench Verified 79 vs 73.4. Their numbers; nothing here\n" +
				"verifies quality and a throughput bench never could.\n\n" +
				"Same slot as qwen36: the planner refuses it beside qwen36 with\n" +
				"must_free=[qwen36].\n\n" +
				"tool-call-parser was READ from chat_template.jinja and then exercised,\n" +
				"not merely configured. Multimodal weights are present (333 visual\n" +
				"tensors) but modality stays text: nothing here sends it images, and\n" +
				"claiming an untested capability is how a recipe starts lying.",
		},
		{
			ID: "qwen38-fp8", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "Qwen/Qwen3.8-27B-FP8", Image: pinnedVLLM,
			ServedAs: []string{"qwen38"},
			Declared: recipe.Footprint{WeightsGiB: 28.95, KVGiB: 62.03},
			Args: map[string]any{
				"gpu-memory-utilization": 0.80, "max-model-len": 131072,
				"max-num-seqs": 4, "kv-cache-dtype": "auto",
				"enable-prefix-caching": true, "reasoning-parser": "qwen3",
				"enable-auto-tool-choice": true, "tool-call-parser": "qwen3_coder",
				"speculative-config": `{"method":"mtp","num_speculative_tokens":3}`,
			},
			Env: map[string]string{
				// sm_121a is what GB10's Blackwell actually reports, and some
				// kernels gate on this list at JIT/Triton compile time. Omitting
				// them does not fail loudly - the model starts and behaves
				// differently, which is worse.
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "SUPERSEDED by qwen38 (NVFP4). Kept as evidence, not nostalgia.\n" +
				"21.91 structured / 13.83 prose tok/s, 273 KiB/token, 238,400 tokens.\n" +
				"Chose FLASH_ATTN from four candidates and captured FULL CUDA graphs\n" +
				"(PIECEWISE=5, FULL=3), which the NVFP4 build does not - and NVFP4 still\n" +
				"won on throughput. That comparison is the reason to keep this entry.\n\n" +
				"util 0.80 left the box at 113/121 GiB with 4.2 GiB of swap engaged: a\n" +
				"fraction taken at its word rather than a budget that was needed.",
		},
		{
			ID: "qwen38-dflash2", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "Qwen/Qwen3.8-27B-FP8", Image: "fleet/vllm-dflash2:pr52816-aarch64",
			ServedAs: []string{"dflash2"},
			// Target 28.77 PLUS drafter 3.58: the drafter is resident too, and a
			// footprint that ignored it would under-plan by 3.58 GiB. vLLM later
			// reported 32.28 GiB for the pair, against the 32.35 declared here.
			Declared: recipe.Footprint{WeightsGiB: 32.35, KVGiB: 20},
			Args: map[string]any{
				// 0.5, not the 0.33 this started with. At 0.33 vLLM caps itself
				// near 40 GiB and weights plus drafter already take 32.33, so it
				// refuses to start for want of KV - while Sous reports 53 GiB of
				// margin going unused. The flag gates against free memory; it
				// does not budget, and a low value only starves the KV cache.
				"gpu-memory-utilization": 0.5, "max-model-len": 32768,
				// SEVEN, not eight. DFlash2's block size of 8 is 7 draft tokens
				// PLUS the verified one. Asking for 8 wants 9 slots in a buffer
				// sized for 8, and it dies in a CUDA device-side assert -
				// "index out of bounds" - the instant the drafter first runs,
				// which reads like an FP8 or sm_121 fault and is neither.
				"speculative-config": `{"method":"dflash","model":"incoai/Qwen3.8-27B-DFlash2","num_speculative_tokens":7}`,
			},
			Env: map[string]string{
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "MEASURED 2026-08-19: 27.42 tok/s aggregate against the 23.97\n" +
				"NVFP4 + MTP baseline, +14.4%. The aggregate is the least useful\n" +
				"number here, because the spread is the finding:\n\n" +
				"  42.96 tok/s  JSON list      +79%\n" +
				"  28.40 tok/s  code + tests   +18%\n" +
				"  19.16 tok/s  prose          -20%\n\n" +
				"Mean acceptance length 4.45-6.10 of 7 drafted. Speculation pays\n" +
				"where the drafter can guess - syntax it has seen a thousand times -\n" +
				"and COSTS on prose, where every rejected draft is bandwidth spent\n" +
				"for nothing on a box already bandwidth-bound at decode. A model\n" +
				"serving mixed traffic gets the average; one serving structured\n" +
				"output gets most of the 79%.\n\n" +
				"DFlash2 speculative decoding against Qwen3.8-27B-FP8.\n\n" +
				"ITS IMAGE IS LOCALLY BUILT AND ON NO REGISTRY. vLLM PR 52816 adds\n" +
				"DFlash2DraftModel, which no released vLLM and no SGLang carries. A\n" +
				"node without that image cannot deploy this recipe.\n\n" +
				"FP8 rather than NVFP4: the DFlash2 selector cannot take a quantized\n" +
				"target lm_head yet, and NVFP4 quantizes it. This checkpoint does not.\n\n" +
				"FP8 rather than bf16: decode here is bandwidth-bound near 273 GB/s,\n" +
				"so bf16's 51.77 GiB against NVFP4's 24.87 roughly halves the base\n" +
				"rate - and ~3x speculation on a halved base lands BELOW the 23.97\n" +
				"tok/s already measured on NVFP4 + MTP. bf16 clears every software\n" +
				"blocker and loses on physics.\n\n" +
				"Block-scaled FP8 DOES work on sm_121: this build logs\n" +
				"CutlassFp8BlockScaledMMKernel and auto-disables DeepGemm on\n" +
				"Blackwell, which is the path that crashed earlier attempts.",
		},
		{
			ID: "nemotron35", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model:    "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4",
			Image:    "vllm/vllm-openai:v0.27.1-aarch64",
			ServedAs: []string{"nemotron"},
			Declared: recipe.Footprint{WeightsGiB: 17.86, KVGiB: 4},
			Args: map[string]any{
				"gpu-memory-utilization": 0.38, "kv-cache-memory-bytes": 4294967296,
				"max-model-len": 262144, "max-num-seqs": 4, "kv-cache-dtype": "fp8",
				"mamba-cache-mode": "align", "reasoning-parser": "nemotron_v3",
				"enable-auto-tool-choice": true, "tool-call-parser": "qwen3_coder",
			},
			Env: map[string]string{
				// sm_121a is what GB10's Blackwell actually reports, and some
				// kernels gate on this list at JIT/Triton compile time. Omitting
				// them does not fail loudly - the model starts and behaves
				// differently, which is worse.
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "78-79 tok/s single-stream, flat across 20 / 1.5k / 9k context.\n\n" +
				"The DSpark drafter is a 48% LOSS on this box and must not be re-added\n" +
				"without re-measuring. Acceptance was fine at 45.6% and 1.37 tokens per\n" +
				"draft; the cost was elsewhere - enabling speculation downgraded CUDA\n" +
				"graphs FULL -> PIECEWISE under FlashInfer, and at max-num-seqs 4 the\n" +
				"per-token graph overhead exceeded what speculation saved.\n\n" +
				"reasoning-parser nemotron_v3, not nemotron: the KeyError lists the valid\n" +
				"set, and --help does not.",
		},
		{
			ID: "qwen3-omni", Kind: recipe.KindVLLM, Modality: recipe.ModalityOmni,
			Model:    "YihongJin/Qwen3-Omni-30B-A3B-Instruct-NVFP4-W4A4-full-thinker-awqclip",
			Image:    "vllm/vllm-omni:nightly-61a678bd2671237fda844c5c88ff51614bc7c579",
			ServedAs: []string{"qwen3omni"},
			Declared: recipe.Footprint{WeightsGiB: 19.53, KVGiB: 11.28},
			Args: map[string]any{
				// --omni is what activates the Talker stage. Without it this
				// loads as a text model with the speech path inert, which is
				// exactly the audio-in-only shape that made `omni` below
				// pointless here.
				"omni": true,
				// PER-STAGE, because a single --gpu-memory-utilization is
				// applied to EVERY stage independently. At 0.55 stage 0 alone
				// took 66.89 GiB and starved the talker.
				//
				// devices:"0" is REQUIRED, not tuning. Without it stage 1
				// spawns with no GPU at all: vllm_omni's device-placement
				// helper disables itself because upstream vLLM removed
				// set_device_control_env_var, and api_server.py blanks
				// CUDA_VISIBLE_DEVICES - which CDI never sets - so the child
				// inherits "".
				"stage-overrides": `{"0":{"gpu_memory_utilization":0.30,"devices":"0"},` +
					`"1":{"gpu_memory_utilization":0.20,"devices":"0","enforce_eager":true},` +
					`"2":{"gpu_memory_utilization":0.10,"devices":"0","enforce_eager":true}}`,
				"gpu-memory-utilization": 0.30,
				"max-model-len":          32768,
				// hermes, NOT the qwen3_coder every other recipe here uses.
				// Read from this checkpoint's chat_template.jinja, which asks
				// for JSON inside <tool_call> tags. qwen3_coder expects
				// <function=name><parameter=x>. Both emit <tool_call>, so the
				// tag alone does not identify the format - and the wrong
				// parser fails SILENTLY, delivering the call as text in
				// content.
				"enable-auto-tool-choice": true,
				"tool-call-parser":        "hermes",
			},
			Env: map[string]string{
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
				// Explicit because CDI injects devices rather than
				// enumerating them, so this is otherwise unset - and
				// api_server.py restores the SAVED value after blanking it.
				"CUDA_VISIBLE_DEVICES": "0",
			},
			Notes: "MEASURED WORKING 2026-08-16. Serves on :8010 and emits real speech:\n" +
				"24 kHz mono 16-bit WAV, 7.82s of audio generated in 2.77s - faster than\n" +
				"realtime. Voices chelsie and ethan both confirmed.\n\n" +
				"TEXT IS SLOW: 7.6-8.2 tok/s, against qwen38's 23.97 structured / 14.85\n" +
				"prose. The earlier note here predicted a 30B-A3B MoE would BEAT dense\n" +
				"qwen38 because it activates ~3B per token. That was WRONG - stages 1 and\n" +
				"2 stay resident and run eager, and that is the cost. This is a voice\n" +
				"model that thinks adequately, not a faster qwen38.\n\n" +
				"Footprint is now measured, not arithmetic: 19.53 GiB weights and 11.28\n" +
				"GiB KV for stage 0 at gpu_memory_utilization 0.30.\n\n" +
				"IMAGE SELECTION IS BY VERSION PAIRING, NOT RECENCY - two windows were\n" +
				"burned learning this. vllm_omni and the vLLM packaged beside it must\n" +
				"agree, and version.py warns on EVERY boot when they do not:\n" +
				"  v0.27.0rc1-aarch64  ImportError on MistralToolCall, dead at import\n" +
				"  rc2 nightlies       omni 0.27.0rc2 calling a vLLM 0.26.0 API that does\n" +
				"                      not exist (MultiModalConfig.mm_hasher_algorithm)\n" +
				"  THIS ONE            vLLM 0.26.0 + omni 0.26.1, self-consistent\n\n" +
				"WHAT IT BUYS: real speech OUTPUT. The cascade it replaces (asr -> qwen38\n" +
				"-> kokoro) runs ~0.5s and discards prosody at the text boundary.\n\n" +
				"NOT the mistake `omni` below made. That checkpoint was audio-IN only and\n" +
				"still needed separate TTS. This one carries the speech stack - verified\n" +
				"in the safetensors index: thinker 75,615 / talker 8,037 / code2wav 230,\n" +
				"where talker and code2wav are IDENTICAL to the bf16 original.\n\n" +
				"QUANT SELECTION IS LOAD-BEARING. Sibling repos named -talker-safe and\n" +
				"-text-only exist because naive quantisation BREAKS speech output. This\n" +
				"build leaves code2wav*, talker*, thinker.audio_tower*, thinker.lm_head\n" +
				"and thinker.visual* unquantised, and it demonstrably speaks.\n\n" +
				"NVFP4 W4A4 IS PROVEN ON GB10 by this deployment: ModelOpt NVFP4 accepted,\n" +
				"FlashInferCutlassNvFp4LinearKernel and FLASHINFER_CUTLASS MoE backend\n" +
				"selected, 1759 weights loaded. sm_121 was never the obstacle.\n\n" +
				"Request audio with {\"modalities\": [\"audio\"]} and an audio block\n" +
				"selecting the voice. Returns base64 WAV in message.audio.data.\n\n" +
				"METHOD NOTE: the two windows spent guessing cost real downtime, while\n" +
				"everything that actually solved this - the --stage-overrides syntax, the\n" +
				"device-placement bug, devices:\"0\" - came from running throwaway\n" +
				"containers against the image, which costs nothing. Ask the image first.\n\n" +
				"VOICES - exactly three, from talker_config.speaker_id: chelsie (2301),\n" +
				"ethan (2302), aiden (2303). Same as upstream Qwen, so quantising the\n" +
				"thinker cost no voices.\n\n" +
				"LANGUAGES: 119 text, 19 speech IN, only 10 speech OUT. Arabic and Urdu\n" +
				"are input-only - it understands them and is not documented to speak them.\n" +
				"Speech out: English, Chinese, French, German, Russian, Italian, Spanish,\n" +
				"Portuguese, Japanese, Korean.\n\n" +
				"IT FAILS SILENTLY, WHICH IS THE MOST IMPORTANT THING TO KNOW. Every\n" +
				"out-of-range request returns plausible audio instead of an error, so a\n" +
				"client MUST validate before sending:\n" +
				"  - an unknown voice name returns audio in a fallback voice, no error\n" +
				"  - an unsupported output language returns audio anyway\n" +
				"  - modalities [\"text\",\"audio\"] returns text and DROPS the audio\n" +
				"  - requesting audio SUPPRESSES tool calls: same prompt and tools that\n" +
				"    yield finish_reason tool_calls in text mode return finish_reason\n" +
				"    stop with audio and no tool_calls\n\n" +
				"So a voice agent on this model is necessarily TWO-PHASE: a text pass to\n" +
				"settle tool calls, then a second request for the spoken answer. That is\n" +
				"an extra round trip per turn and there is no flag that avoids it.\n\n" +
				"MEASURED: voice-in understood correctly; tool calls fire from spoken\n" +
				"input (get_weather, get_current_time, calculate all verified); audio\n" +
				"generation runs ~2.5x realtime across all three voices.",
		},
		{
			ID: "omni", Kind: recipe.KindVLLM, Modality: recipe.ModalityOmni,
			Model:    "nvidia/Nemotron-3-Nano-Omni-30B-A3B-Reasoning-NVFP4",
			Image:    "fleet/vllm-omni-gcc12:v0.27.1-aarch64",
			ServedAs: []string{"omni"},
			Declared: recipe.Footprint{WeightsGiB: 21.59, KVGiB: 4},
			Args: map[string]any{
				"gpu-memory-utilization": 0.45, "kv-cache-memory-bytes": 4294967296,
				"max-model-len": 131072, "max-num-seqs": 4, "kv-cache-dtype": "fp8",
				"trust-remote-code": true, "reasoning-parser": "nemotron_v3",
				"enable-auto-tool-choice": true, "tool-call-parser": "qwen3_coder",
			},
			Env: map[string]string{
				"TORCH_CUDA_ARCH_LIST": "12.1a",
				"CUTE_DSL_ARCH":        "sm_121a",
				"HF_HOME":              "/root/.cache/huggingface",
				"VLLM_LOGGING_LEVEL":   "INFO",
			},
			Notes: "RETIRED 2026-08-14. Used 25.6 GiB to do ASR that a 1.19 GiB model does\n" +
				"better and faster - a 21x memory saving for equal transcription quality.\n\n" +
				"Needs the GCC 12 image: the stock vllm image ships g++ 11.4, which\n" +
				"rejects the ARMv9 -march that torch inductor emits on GB10. The failure\n" +
				"presents as a per-modality processor error and MIGRATES when you suppress\n" +
				"one modality, which sends you chasing the wrong thing entirely. Look for\n" +
				"CppCompileError first.",
		},
		{
			ID: "asr", Kind: recipe.KindTransformers, Modality: recipe.ModalityASR,
			Model: "nvidia/nemotron-3.5-asr-streaming-0.6b",
			Image: "fleet/nemotron-asr:0.6b", Build: "stacks/nemotron-asr",
			Entrypoint: []string{"python3", "-m", "uvicorn", "app:app",
				"--host", "0.0.0.0", "--port", "8000"},
			Declared: recipe.Footprint{WeightsGiB: 1.19},
			Env: map[string]string{
				"ASR_MODEL":  "nvidia/nemotron-3.5-asr-streaming-0.6b",
				"ASR_DEVICE": "cuda",
				"HF_HOME":    "/root/.cache/huggingface",
			},
			Notes: "0.42s mean for ~3.7s clips (RTF 0.11), 0.0% WER, 35 languages.\n\n" +
				"A hand-written FastAPI service because vLLM registers every ASR\n" +
				"architecture but ships NO transcription entrypoint - the modules under\n" +
				"vllm.entrypoints.openai are api_server, chat_completion, cli_args,\n" +
				"completion, dp_supervisor, engine, models, parser, responses, run_batch.\n" +
				"Nothing audio-shaped.\n\n" +
				"The entrypoint override MUST be at container level. The vLLM base image\n" +
				"sets ENTRYPOINT [\"vllm\",\"serve\"], turning CMD into arguments to vLLM;\n" +
				"a Dockerfile ENTRYPOINT [] did not survive the rebuild. 46 crash loops.\n\n" +
				"The processor rejects any sample rate but 16 kHz outright rather than\n" +
				"resampling, so the service resamples before calling it.",
		},
		{
			ID: "kokoro", Kind: recipe.KindContainer, Modality: recipe.ModalityTTS,
			Image: "ghcr.io/remsky/kokoro-fastapi-gpu:latest",
			// The image declares EXPOSE 8000 and the process listens on 8880.
			// Without this the container starts, warms up on the GPU, reports
			// healthy, and answers nothing.
			ContainerPort: 8880,
			Declared:      recipe.Footprint{WeightsGiB: 3, KVGiB: 0},
			Env:           map[string]string{"HF_HOME": "/root/.cache/huggingface"},
			Notes: "MOVED TO GPU 2026-08-16, AFTER MEASURING. This recipe previously\n" +
				"specified the -cpu image with a 0 GiB footprint, and the reasoning for\n" +
				"that was written when a 25.6 GiB Omni was co-resident. It does not\n" +
				"survive the arithmetic at this model's size.\n\n" +
				"Kokoro is 82M parameters and reads ~0.16 GB per forward pass against a\n" +
				"27B model's 24.87 GB - under 1% of this box's ~273 GB/s. The\n" +
				"'co-resident models split bandwidth negative-sum' argument is real for\n" +
				"two large models and irrelevant for this one.\n\n" +
				"Measured side by side on gx10, identical 14-word input:\n" +
				"  CPU   1194 / 1131 / 1391 ms\n" +
				"  GPU    202 /  201 /  193 / 198 / 194 ms      ~6x faster\n" +
				"  cost  3 GiB resident\n\n" +
				"TTS was the largest single component of the voice loop - larger than ASR\n" +
				"(~440 ms) and the LLM (~260 ms) combined. This takes the round trip from\n" +
				"~1.41s to roughly 0.5s, the difference between dictation and\n" +
				"conversation.\n\n" +
				"THE DECLARED FOOTPRINT IS LOAD-BEARING, NOT DOCUMENTATION: spec.go sets\n" +
				"GPU: Declared.WeightsGiB > 0, so a 0 here does not merely misreport the\n" +
				"capacity plan, it withholds the device and silently returns this service\n" +
				"to the CPU path it was moved off.\n\n" +
				"ARM64 was checked before the move, because this box has lost hours to it:\n" +
				"the image publishes linux/arm64 AND confirmed at runtime, not just in the\n" +
				"manifest - 'Found GPU0 NVIDIA GB10 ... cuda capability 12.1', 'CUDA: True'.\n" +
				"GPU whisper was tried here and silently ran on CPU because CTranslate2\n" +
				"ships no aarch64 CUDA build, and was SLOWER than the CPU image while\n" +
				"reporting the GPU as visible.\n\n" +
				"To go back: set the image to the -cpu tag AND set weights_gib to 0, or\n" +
				"the container gets a GPU it does not use."},
	}
}
