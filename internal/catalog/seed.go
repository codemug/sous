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
			Model: "nvidia/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4",
			Image: "vllm/vllm-openai:v0.27.1-aarch64",
			ServedAs: []string{"nemotron"}, Archived: true,
			Declared: recipe.Footprint{WeightsGiB: 17.86, KVGiB: 4},
			Args: map[string]any{
				"gpu-memory-utilization": 0.38, "kv-cache-memory-bytes": 4294967296,
				"max-model-len": 262144, "max-num-seqs": 4, "kv-cache-dtype": "fp8",
				"mamba-cache-mode": "align", "reasoning-parser": "nemotron_v3",
				"enable-auto-tool-choice": true, "tool-call-parser": "qwen3_coder",
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
			ID: "omni", Kind: recipe.KindVLLM, Modality: recipe.ModalityOmni,
			Model: "nvidia/Nemotron-3-Nano-Omni-30B-A3B-Reasoning-NVFP4",
			Image: "fleet/vllm-omni-gcc12:v0.27.1-aarch64",
			ServedAs: []string{"omni"}, Archived: true,
			Declared: recipe.Footprint{WeightsGiB: 21.59, KVGiB: 4},
			Args: map[string]any{
				"gpu-memory-utilization": 0.45, "kv-cache-memory-bytes": 4294967296,
				"max-model-len": 131072, "max-num-seqs": 4, "kv-cache-dtype": "fp8",
				"trust-remote-code": true, "reasoning-parser": "nemotron_v3",
				"enable-auto-tool-choice": true, "tool-call-parser": "qwen3_coder",
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
			Image:    "ghcr.io/remsky/kokoro-fastapi-cpu:latest",
			Declared: recipe.Footprint{},
			Notes: "CPU BY DESIGN and costs zero GPU - that is a choice, not a limitation.\n" +
				"Keeping TTS off the GPU leaves the ~273 GB/s of memory bandwidth for the\n" +
				"LLM, which is what decode is actually bound by. Co-resident GPU models\n" +
				"split that bandwidth negative-sum.\n\n" +
				"0.40x realtime, 68 voices. arm64 manifest verified before deploying.",
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
