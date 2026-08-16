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
			ID: "qwen38-fp8", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model: "Qwen/Qwen3.8-27B-FP8", Image: pinnedVLLM,
			ServedAs: []string{"qwen38"}, Archived: true,
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
			ID: "nemotron35", Kind: recipe.KindVLLM, Modality: recipe.ModalityText,
			Model:    "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4",
			Image:    "vllm/vllm-openai:v0.27.1-aarch64",
			ServedAs: []string{"nemotron"}, Archived: true,
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
				"containers against the image, which costs nothing. Ask the image first.",
		},
		{
			ID: "omni", Kind: recipe.KindVLLM, Modality: recipe.ModalityOmni,
			Model:    "nvidia/Nemotron-3-Nano-Omni-30B-A3B-Reasoning-NVFP4",
			Image:    "fleet/vllm-omni-gcc12:v0.27.1-aarch64",
			ServedAs: []string{"omni"}, Archived: true,
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
			Image:    "ghcr.io/remsky/kokoro-fastapi-gpu:latest",
			Declared: recipe.Footprint{WeightsGiB: 3.0},
			Env:      map[string]string{"HF_HOME": "/root/.cache/huggingface"},
			Notes: "ON THE GPU since 2026-08-16, after measuring. Was CPU-only, and the\n" +
				"argument for that was inherited rather than checked:\n\n" +
				"  CPU  1194 / 1131 / 1391 ms   GPU  202 / 201 / 193 / 198 / 194 ms\n\n" +
				"~6x faster for 3 GiB. TTS had been the largest single component of the\n" +
				"voice loop - larger than ASR (~440 ms) and the LLM (~260 ms) combined.\n\n" +
				"The old reasoning was 'keeping TTS off the GPU leaves the bandwidth for\n" +
				"the LLM'. True when a 25.6 GiB Omni was co-resident; false here. Kokoro\n" +
				"is 82M parameters reading ~0.16 GB per forward against qwen38's 24.87 GB\n" +
				"- under 1% of this box's ~273 GB/s.\n\n" +
				"arm64 confirmed in the manifest AND at runtime ('Model warmed up on\n" +
				"cuda: kokoro_v1' on GB10). The second check is not optional: GPU whisper\n" +
				"reported the GPU as visible, silently ran on CPU, and was slower.\n\n" +
				"68 voices. Revert by pinning the -cpu tag; it still has an arm64 image.",
		},
		{
			ID: "whisper", Kind: recipe.KindContainer, Modality: recipe.ModalityASR,
			Image: "ghcr.io/speaches-ai/speaches:latest-cpu", Archived: true,
			Declared: recipe.Footprint{},
			Env: map[string]string{
				"WHISPER__MODEL": "Systran/faster-whisper-tiny",
				"WHISPER__TTL":   "-1",
				// NOT /root/.cache - this image runs as the unprivileged user
				// `ubuntu`, and the /root convention produced a permission error
				// on every request while /health still answered 200.
				"HF_HOME": "/home/ubuntu/.cache/huggingface",
			},
			Notes: "RETIRED, replaced by asr. Could not use the GPU here at all:\n" +
				"faster-whisper is built on CTranslate2, which publishes no aarch64 CUDA\n" +
				"build. The cuda image was tried and was SLOWER (4.54s vs 3.92s) because\n" +
				"it silently ran on CPU anyway - nvidia-smi saw the GPU while\n" +
				"ctranslate2.get_cuda_device_count() returned 0. No device flag fixes\n" +
				"that; only a different runtime would.",
		},
	}
}
