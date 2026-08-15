# Recipe catalog

Every model measured on asus-gx10 (NVIDIA GB10, 121.6 GiB unified memory),
exported as recipes Sous can fetch as a git **source**:

```
sous → Sources → add  https://github.com/codemug/sous  →  Fetch
```

Three entries are archived negative results, kept on purpose so nobody
re-derives why they lost:

| recipe | why it is here |
|---|---|
| `qwen38-fp8` | superseded by NVFP4; holds the comparison that proved it |
| `omni` | used 25.6 GiB to do ASR a 1.19 GiB model does better |
| `whisper` | could not use the GPU at all on aarch64 |

## What is deliberately absent

**No ports.** Placement is decided at deploy time against the real host.

**No measured values.** `24.87 GiB of weights` and `136 KiB/token` are facts
about one box running one engine build, not about the model. They live in the
deploying node's `observations/`, which is what lets these files be shared
without shipping lies.

**Footprints here are `declared` estimates.** They happen to equal what this
node measured, because these recipes were authored on it. A recipe fetched onto
different hardware starts with the estimate and earns its own observations on
first load.
