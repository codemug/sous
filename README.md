# Sous

A recipe manager for local inference. One node, one GPU pool, one thing that
owns it.

Sous stores **recipes** — complete, portable answers to "how do I serve this
model" — checks whether one fits in the memory pool before starting it, deploys
it through the Docker Engine API on a port allocated at deploy time, and makes
failure legible when it happens anyway.

Built for a single NVIDIA GB10 (DGX Spark class) with 121.6 GiB of unified
memory and no discrete VRAM, which is the constraint that shapes every design
decision here.

## Why it exists

Managing models on a box like this by hand goes wrong in ways that are not
obvious and not documented:

- **`--gpu-memory-utilization` is a startup gate, not a budget.** It is checked
  against *free* memory, so a model that fits perfectly refuses to start
  because another one is resident.
- **KV cost per token differs 20× between models.** 88.3 KiB/token for a
  35B MoE, 273 for a "smaller" 27B dense hybrid, ~4.3 for a Mamba model.
  "Give it more KV" means something different every time.
- **Page cache is not counted as free.** Loading 30 GB of weights leaves 30 GB
  of stale cache behind, and the next model sizes its cache against a lie.
- **Two models profiling memory concurrently is a documented crash.**

Sous encodes all of that as rules the machine follows rather than facts a human
has to remember.

## Concepts

| Term | Meaning |
|---|---|
| **recipe** | Portable: model, image, kind, flags, and the reasoning in `notes` |
| **source** | A git repo of recipes, mirrored read-only |
| **overlay** | Your edits to a source recipe, as a sparse patch |
| **deployment** | A recipe plus what this node granted it: port, container |
| **larder** | Downloaded weights on disk |
| **observation** | What the boot log actually reported — node-local, never in a recipe |

A recipe declares a `kind`, which changes its shape rather than just labelling
it:

- `vllm` — image plus serve flags
- `transformers` — a build context and an explicit entrypoint
- `container` — a third-party image, used as published

That matters because not everything is a language model. A CPU-only TTS service
costs zero GPU, and a capacity model that assumes every entry has weights *and*
a KV cache is wrong for it.

## Design decisions worth knowing

**No ports in a recipe.** Not host, not container. Placement is decided at
deploy time against the real host, with an actual bind test — because a process
outside Sous can hold a port, and checking only your own records cannot see
that.

**Measured values never live in a recipe.** `24.87 GiB of weights` and
`136 KiB/token` are facts about *one box running one engine build*, not about
the model. They live in `observations/`, which is also what lets recipes be
distributed without shipping lies.

**Docker Engine API, not Compose.** Every Compose trap this was built against
is a property of Compose semantics rather than of containers: a service
archived behind `profiles:` keeps running, teardown fails when the file uses
`${VAR:?}` guards, and `up -d` will not rebuild after a Dockerfile change. Sous
still writes a compose file per deployment into `exports/`, never used to
deploy, purely so a broken Sous can be worked around by hand.

**Capacity returns a margin, never a boolean.** A 2 GiB pass and a 30 GiB pass
call for different decisions.

**Force distinguishes policy from safety.** It overrides a judgement you may
disagree with — deleting rollback weights — and never overrides a guard that
protects something in use.

## Running it

```
sous -listen 100.x.y.z:8090 -models /path/to/model/cache -data /var/lib/sous
```

`-listen` is required and refuses `0.0.0.0`. Sous generates container
configuration and runs it, which makes it root-equivalent on its node by
construction; the network boundary is the mitigation, so binding everything
would remove it.

## Layout

```
internal/recipe      the portable schema and its validation
internal/catalog     recipes, sources, overlays, effective resolution
internal/capacity    does it fit, and by how much
internal/ports       deploy-time allocation, verified by binding
internal/engine      recipe -> container spec, and the Docker client
internal/deploy      the ordering rules: serialise, drop caches, stop-then-start
internal/observe     boot log -> measured truth
internal/larder      weights on disk, reconciled and reclaimable
internal/sources     read-only git mirrors
internal/overlay     sparse patches and a field-level three-way merge
internal/httpapi     JSON API and the server-rendered UI
```

## Tests

```
go test ./...
```

The interesting ones assert behaviour that is expensive to relearn: that a
redeploy stops the old container *before* starting the new one, that page cache
is dropped before anything starts, that `352,000 tokens` does not parse as 352,
and that `PIECEWISE` stays distinguishable from `FULL_AND_PIECEWISE`.
