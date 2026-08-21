# Sous — design brief

**A control plane for one GPU that everything has to share.**

Sous decides which AI models are running on a single machine with 121.6 GiB of
memory and no way to fit two large ones at once. The interface exists to make
that competition visible before it goes wrong.

Everything below was read out of the running system on 2026-08-21 at v0.9.1.
The figures, page sizes and phase list are real, not illustrative.

---

## 1. What Sous is

One operator, one machine, a private network. Sous runs on an NVIDIA GB10 and
starts and stops model containers on it. There is no multi-tenancy, no team, no
billing, and no mobile use — **the whole audience is one person at a desk,
usually mid-task, usually about to change something they cannot easily undo.**

That shapes everything. This is not a product that needs onboarding or
persuasion. It needs to tell the truth quickly and refuse clearly.

**Who reads this screen.** Someone who already knows what a model is, is
comfortable in a terminal, and opened the dashboard *because the terminal was
worse* — usually to see what is running and how much room is left.

**What they came to do**, in rough order of frequency:

1. Check what is running
2. Start something
3. Stop something to make room
4. Work out why a thing that should be running is not

---

## 2. The one hard idea

If you understand one thing, make it this.

The machine has a **single pool of memory shared by every model**. Most models
want 25–60 GiB of it. Two large ones do not fit, so starting one usually means
stopping another.

Sous therefore answers a question no ordinary dashboard has to: **would this
fit, and if not, what would have to stop?** It reports a *margin* — memory left
over after a hypothetical deployment — rather than a yes or no, because the
operator sometimes needs to overrule it and cannot judge that from a boolean.

The pool bar on `/` renders this as one horizontal bar:

```
├──────── 40.4 in use ────────┼─── 24 reserve ───┼──────── 57.2 free ────────┤
121.6 GiB total · margin 57.2 GiB
```

The **reserve** segment is memory Sous refuses to allocate — a deliberately
conservative 24 GiB with no formula behind it. An earlier theory that overhead
scaled with process count was measured and falsified. The number is honest
guesswork, and the interface should not imply more precision than it has.

---

## 3. Vocabulary

These words are load-bearing. Renaming them in a redesign breaks the mental
model, and the first two are routinely confused.

| Term | Meaning |
|---|---|
| **Recipe** | A model's *configuration* — image, arguments, declared memory footprint, notes. Exists whether or not anything runs. A saved definition. |
| **Deployment** | A recipe that is *actually running*, on a port. The recipe → deployment distinction is the one people miss most. |
| **Phase** | What a deployment is doing right now: `starting`, `ready`, `failed`, `stopping`, `gone`. Derived live, never stored. |
| **Margin** | Memory that would remain after a deployment. Can be **negative** — that is how a refusal explains itself rather than merely happening. |
| **Drift** | Sous has a record but the container is gone — OOM reaper, removed by hand, or crash-looped past its limit. Now folded into the `gone` phase. |
| **Larder** | Downloaded model weights on disk, tens of GiB each. Separate from whether anything runs. The page exists to reclaim space. |
| **Source** | A git repository of recipes that can be fetched. Nothing is ever deployed by a fetch. |

---

## 4. The screens

Eight pages, server-rendered. Byte sizes are the real rendered documents, as a
rough proxy for density.

| Route | Job | Notes | Size |
|---|---|---|---|
| `/` | **Node** — what is running, how much room is left | Pool bar, then a card per deployment. Landing page, most used. | 19.7 kB |
| `/catalog` | **Catalog** — every recipe, deployable or archived | Where a model gets started. Densest page. | 21.3 kB |
| `/model/{id}` | **Model** — config, telemetry and logs for one thing | Deliberately one page, not three tabs: the three questions interleave. | — |
| `/deployments` | **Deployments** — the running set as a list | Overlaps heavily with Node. Candidate for merging. | 14.1 kB |
| `/larder` | **Larder** — weights on disk, what is reclaimable | Destructive actions live here. | 16.1 kB |
| `/sources` | **Sources** — recipe repositories | Rarely visited. Fine as is. | 15.0 kB |
| `/keys` | **API keys** — inference credentials | Newest page. Has a show-once secret moment. | 14.7 kB |
| `/login` | **Sign in** | A real form, deliberately — see constraints. | — |

---

## 5. The system as built

There is an existing design language and it is coherent. It is stamped in the
stylesheet as `macrostructure: Map/Diagram · genre: modern-minimal · theme:
Cobalt`. Treat it as a starting point with a point of view, not a blank slate.

### Colour

Defined in `oklch` throughout, with a full dark theme. The neutrals carry a
slight blue bias toward the accent rather than being neutral grey.

| Token | Value | Role |
|---|---|---|
| `--accent` | `oklch(58% 0.20 256)` | Cobalt. Chrome and interaction only. |
| `--ok` | `oklch(52% 0.13 158)` | Ready. |
| `--gold` | `oklch(58% 0.12 100)` | Starting. |
| `--drift` | `oklch(62% 0.15 62)` | Failed / gone. |
| `--ink` | `oklch(24% 0.02 258)` | Text. |
| `--paper` | `oklch(98.5% 0.004 250)` | Ground. |

Semantic colour is separate from the accent, which is correct and should stay
that way. Cobalt is chrome; green/gold/amber mean something.

### Type

System stacks only — `ui-sans-serif` for prose, `ui-monospace` for every number,
id, port and state. No webfont ships. That is a deliberate constraint (see
below), and the split does real work: **anything the machine said is mono,
anything a person wrote is sans.**

### Phase chips

Shape carries state as well as hue — round dot, square dot, hollow square,
spinner. Colour alone would fail anyone who cannot separate green from amber.

```
● ready      (round dot,   green)
◜ starting   (spinner,     gold)
◜ stopping   (spinner,     muted)
■ failed     (square dot,  amber)
□ gone       (hollow, dashed border)
```

---

## 6. Constraints that are not negotiable

**Go `html/template`, server-rendered.** No React, no build step, no bundler.
Components are HTML partials. A design that needs a component framework cannot
ship.

**No CDN, ever.** The machine sits on a private network and is sometimes wholly
offline. Every asset must be local or inline. This is why there is no webfont —
one may be added only if self-hosted and embedded.

**Core actions work without JavaScript.** Deploy, undeploy, edit and revoke are
plain form posts today. JavaScript may enhance; it may not be required to
operate the thing.

**Operations take minutes, not milliseconds.** Starting a model takes **eight to
ten minutes**. Stopping one takes up to a minute. Any interaction model that
assumes instant feedback is wrong here.

---

## 7. Worth fixing

Real problems, in rough priority order. Each is a symptom the current layout
produced; none is a matter of taste.

**1 — The pool bar contradicts the cards.** The bar still colours segments with
the old binary `running / drifted` flag while the cards beneath it use the
five-state phase. A model can be amber in one and green in the other on the same
screen. *This is a live inconsistency, not a hypothetical.*

**2 — Nothing shows progress through a long wait.** A starting model shows a
spinner for ten minutes with no sense of how far along it is. The underlying
stages are known and logged — downloading, loading weights, compiling, capturing
CUDA graphs — and none of that reaches the screen.

**3 — Recipe and deployment are split across pages.** Catalog lists
configurations, Node and Deployments list running things, and the same model
appears in all three wearing different clothes. Consider whether one object with
states beats three lists.

**4 — Errors arrive as a query string.** Failures redirect with
`?msg=…&err=1` and render as a banner. It works, but a capacity refusal — the
most informative error in the system, carrying a margin and a list of what to
stop — deserves better than a sentence.

**5 — The destructive path looks like the safe one.** Undeploy, delete recipe
and delete weights are all ordinary buttons. Deleting weights can throw away a
40 GiB download that takes 20 minutes to re-fetch.

**6 — Density collapses badly at narrow widths.** The tables and the pool bar
are built for a desktop. It *is* a desktop tool and that is fine — but it should
degrade deliberately rather than by accident.

---

## 8. Do not change

These carry meaning. Changing them silently changes what the interface claims,
which is worse than an ugly screen.

- **Phase is five states, not two.** "Running" was true for the ten minutes a
  model spends loading and unusable. The distinction between *starting* and
  *ready* is the most valuable thing this UI now says.
- **Margin is a number with a sign, not a verdict.** The operator sometimes
  overrules a refusal and cannot judge that from a yes/no.
- **Mono means machine.** Ids, ports, sizes and states are monospace; prose is
  not. It is doing real work.
- **The API key is shown once.** No "reveal" affordance can be designed in — the
  plaintext genuinely does not exist after creation. Only a SHA-256 is stored.
- **Failure states explain themselves.** "OOM-killed", "crash-looping (12
  restarts)", "exited with code 1" — keep the sentence, not just the badge.
- **Shape carries state alongside colour.** Round, square, hollow, spinning. Do
  not reduce the phases to hue.

---

## 9. The ask

Rework the interface around the thing it is actually for: **one scarce resource,
a handful of heavy objects competing for it, and operations slow enough that the
interface has to narrate them.**

The existing visual language is not the problem and does not need replacing. The
information design is where the work is — what belongs on one screen, how a
ten-minute operation reports itself, and how a refusal explains what to do next.
