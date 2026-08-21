# Sous UI — wiring notes

For the backend implementer. Prototype: `Sous.dc.html`. Source read at `codemug/sous@main` (f79dba9).

The prototype is a browser mock, not a design for a client-side app. Everything in it is reachable with `html/template`, form posts and one optional stream. These notes say what to add, where, and what must not move.

Read alongside `DESIGN.md`. Where the two disagree, DESIGN.md wins — none of this is worth breaking a claim the interface already makes.

## 1 · What does not change

No new dependency, no build step, no CDN asset, no webfont. The prototype uses `ui-sans-serif` and `ui-monospace` only, and the same `oklch` token set already in `layout.html`. Two tokens are used that the stylesheet defines but `tokens.css` omits: `--gold` and `--gold-wash`. Add them there for parity.

Deploy, undeploy, edit, delete and revoke stay plain form posts. Every screen renders correctly on first paint with JavaScript disabled; the stream in §7 only replaces text the server already rendered once.

## 2 · Routes

| Route | Change |
|---|---|
| `/` | Node. Pool bar rebuilt from phase (§4); model cards gain the stage stepper (§5). |
| `/models` | New name for `/catalog`. One list of recipes carrying phase. `/catalog` 301s here. |
| `/model/{id}` | Unchanged in structure. Stepper added while starting. |
| `/model/{id}/plan` | **New, GET, no side effects.** The pre-flight plan (§6). |
| `/deployments` | **Retired.** 301 to `/`; delete `deployments.html` and its handler. |
| `/larder` `/keys` `/sources` `/login` | Same jobs, card layout, typed confirmation on the destructive paths (§8). |
| `/events` | **New, optional.** Server-sent status (§7). |

Nav in `layout.html` drops Deployments and renames Catalog to Models. The rail keeps its permanent node readout and gains a 4px pool bar built from the same segments as the page.

## 3 · One recipe, one phase

Catalog and Node stop being two lists of two kinds of object. Both render the same view model; Node filters it to residents.

```go
// httpapi: one type behind /, /models and /model/{id}
type ModelView struct {
    Recipe      recipe.Recipe
    Phase       deploy.Phase     // starting|ready|failed|stopping|gone, or ""
    Port        int
    UptimeSec   float64
    DeclaredGiB float64
    Observed    *observe.Observation
    Detail      string           // the failure sentence, verbatim
    Progress    *deploy.Progress // non-nil only while starting
}
```

A recipe with no deployment has an empty `Phase`; the template renders that as the library chip. Archived is a recipe flag, not a phase, and stays that way.

**Resident means holding memory.** Define it once and use it for the bar, the counts, the rail and the planner: `Phase` in `{starting, ready, stopping}`. `failed` and `gone` are records whose container is not there, so they hold nothing and must not appear in any memory total.

## 4 · The pool bar

This is the live inconsistency in DESIGN.md. `node.html` currently colours each segment from `.Drifted`:

```html
<div class="seg {{if .Drifted}}seg-drift{{else}}seg-run{{end}}">   <!-- delete -->
```

Replace with a class per phase — `seg-ready`, `seg-starting`, `seg-stopping` — drawn from the same field the chip below uses, and drop `Drifted` from the view model entirely so the two state models cannot diverge again.

Three rules the prototype encodes:

- **Starting segments are hatched, not solid.** The memory is claimed and the model cannot serve. A solid green segment says something false for eight to ten minutes.
- **Failed and gone are not segments.** They render below the bar under "Records without a container", with the released figure and the reason sentence. That is also where the operator clears the record.
- **Segment width is the declared footprint** unless an observation exists, in which case it is the observation. Same rule the planner uses, or the picture and the arithmetic disagree.

Under the bar: a 0 / 30.4 / 60.8 / 91.2 / 121.6 ruler, then phase counts. The reserve keeps its hatch and its sentence — the calibration note from `capacity/plan.go` is worth printing on the page, because a number with no formula behind it should say so.

## 5 · Start progress

The one genuinely new derivation. Stages are already in the boot log; none of this is stored, for the same reason `Phase` is not.

```go
// internal/deploy — derived on read, like Phase
type Stage struct {
    Name    string  // "load weights"
    State   string  // pending | skipped | running | done
    Seconds float64 // elapsed for running, duration for done
    Note    string  // "35.02 GiB into device memory"
}
type Progress struct {
    Stages     []Stage
    ElapsedSec float64
    LastRunSec float64 // Observation.LoadSeconds, for "last start took 7m 41s"
}
```

Detection, from lines `observe` already reads or nearly reads:

| Stage | Becomes done when |
|---|---|
| pull image | Image present before create — `skipped`, with the digest as the note. |
| download weights | Larder holds the model repo — `skipped`, with the size. Otherwise running until the HF cache stops growing. |
| load weights | `Model loading took X GiB memory and Y seconds` — the existing `reLoading`. Starts at `Starting to load model`. |
| compile | `torch.compile takes N s in total`. The attention-backend line gives the note. |
| capture CUDA graphs | `Profiling CUDA graph memory: PIECEWISE=…` — the existing `rePiece`. |
| answers /health | `Prober.Ready` returns true. This is the transition to `ready`, not a stage of its own. |

Per-stage elapsed comes from the log line timestamps; fall back to container `StartedAt` and wall clock when a line is missing. Scan with `--since` the last read so a ten-minute boot is not re-parsed on every refresh, and keep the 4 MiB scanner buffer already in `ParseBootLog` — multimodal profiling lines exceed 64 KiB.

**No percentage and no ETA.** A stage list with elapsed times and the last measured start is honest; a bar filling at a guessed rate is not. A non-vLLM kind gets no stepper at all rather than an invented one.

## 6 · The plan, and how a refusal explains itself

A capacity refusal stops being a query-string banner. `GET /model/{id}/plan` renders `capacity.Result` as a page: the projected bar with the incoming model as its own segment, the margin with its sign, and `MustFree` as a list with a Stop button beside each entry.

```go
resident := residents(states)          // phase in {starting, ready, stopping}
res := planner.Plan(resident, capacity.Entry{ID: id, GiB: declared})
// res.MarginGiB, res.MustFree, res.Warning go straight into the template
```

When the projection overflows, scale the bar to `max(pool, committed+reserve)` and draw a dashed rule at the 121.6 mark, so the overflow is visible as overflow rather than as a clipped bar. Segments that `MustFree` names are hatched in `--drift`.

The plan is live: each Stop is an ordinary `POST /api/undeploy/{id}` that returns to the plan page, which recomputes. No client state is required for that — the stream in §7 only makes it smoother.

**`POST /api/deploy/{id}` changes shape.** Re-run the plan server-side (the client's plan may be stale by minutes). If it fits, deploy. If it does not, respond `409` and render the plan page — not a redirect with `?msg=…&err=1`. A margin, a list of what to stop and a force path do not fit in a sentence.

Force stays available, because the reserve is deliberately conservative and the operator sometimes knows better. It requires `force=<id>` typed by hand (§8) and its own copy explaining that the OOM reaper chooses its victim, which may not be the incoming model.

## 7 · Live updates

One endpoint, one payload, one script. `GET /events` as `text/event-stream`, emitting the `status.go` JSON plus `Progress` per deployment:

```
event: status
data: {"committed_gib":87.07,"margin_gib":10.53,"models":[
        {"id":"qwen36","phase":"starting","port":8110,"elapsed_sec":252,
         "stage":"compile","stage_sec":63}]}
```

- **Tick at 3s, not 1s.** `probeTTL` is 3s; a faster stream turns one dashboard into a health-check storm across every port.
- **Patch, do not re-render.** The script updates text content and the segment widths/classes by `data-id`. It never builds markup — a template change must not require a script change.
- **Optimistic stop.** On submit of an undeploy form, set that card to `stopping` immediately. The server already agrees within one tick: `UndeployAsync` marks it before returning, and `stopMaxAge` bounds a lie to five minutes if Sous is killed mid-stop.
- **Degrade to nothing.** No `EventSource`, no stream: the page is already correct as served. Put a `<noscript>` meta refresh at 15s in the head so a JS-off browser still tracks a boot.

The rail's "streaming" dot reflects the connection, and should go grey when the stream drops. An operator who cannot tell a frozen page from an idle node will re-click a deploy.

## 8 · Destructive actions

Four paths carry a typed confirmation: delete recipe, delete weights, revoke key, force deploy. The field is `confirm` and it must equal the object's id or name.

**Validate server-side.** The prototype disables the button until the text matches, but that is a courtesy; a mismatched or absent `confirm` must fail with `409` and a re-render, because these are form posts and a form post can arrive from anywhere.

State the cost in the confirmation, from real data rather than adjectives: the weights delete says the GiB and the re-fetch minutes for that repo; the recipe delete says the measurements are unrecoverable from the image; the revoke says the holder gets 401 on the next request. A set in use by a running container is not deletable at all, and its card offers no armed state.

## 9 · Partials worth defining

The prototype repeats five shapes. As `{{define}}` blocks in `layout.html`, each takes exactly one value:

```
{{template "phase-chip"    .Phase}}     dot shape + hue + label
{{template "pool-bar"      .Node}}      segments, ruler, legend, warning
{{template "stage-stepper" .Progress}}  used on / and /model/{id}
{{template "model-card"    .}}          ModelView; one shape everywhere
{{template "confirm-typed" .}}          {Action, ID, Label, Cost}
```

Card layout is a wrapping grid — `repeat(auto-fit, minmax(21rem, 1fr))` — so narrow widths reflow instead of collapsing a table. The pool bar and the stepper are the two things still built for a desktop; they need an explicit narrow treatment before anyone opens this on a phone, and that is deliberately out of scope here.

## 10 · Open questions

1. **Larder ownership.** The cards show which recipe each weights set belongs to and whether a container is using it. Does `internal/larder` already map a cache directory to a recipe id, or does that mapping need building from `Recipe.Model`?
2. **Key scope.** The key cards show a scope ("asr, kokoro, qwen38"). If `apikey` has no per-model scoping, that line should be dropped rather than faked.
3. **Re-fetch estimates.** The delete confirmation quotes minutes. Is there a recorded download rate to derive them from, or should it quote size only?
4. **Clearing a gone record.** The prototype offers it on the Node card. Confirm there is a store call for removing a record without touching a container.

---

Prototype screens: Node, Models, plan, model detail, larder, keys, sources, sign in. Figures in it are the real ones from `recipes/` and the boot logs, so the layouts are sized against true strings rather than placeholders.
