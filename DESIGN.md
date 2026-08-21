# Design constraints

Full brief — screens, vocabulary, the existing token system, and what is worth
fixing: [`docs/design-brief.md`](docs/design-brief.md).
Rendered version: <https://claude.ai/code/artifact/99ff2b52-0927-43e9-bb5a-baaebfa82df9>

This file holds only the parts that must survive a redesign. Everything here is
a property the interface **claims**, so changing it silently changes what Sous
tells an operator — which is worse than an ugly screen.

## The one hard idea

One GPU, one pool of 121.6 GiB, and most models want 25–60 of it. Two large
models do not fit. Starting one usually means stopping another. Every screen
exists downstream of that.

## Load-bearing, do not change

**Phase is five states, not two.** `starting / ready / failed / stopping / gone`.
"Running" was true for the eight to ten minutes a model spends loading and
unusable, which is exactly when traffic must not be sent to it. The distinction
between *starting* and *ready* is the most valuable thing this UI says.

**Margin is a number with a sign, not a verdict.** The planner returns memory
remaining, which can be negative. An operator sometimes overrules a refusal and
cannot judge that from a boolean.

**Mono means machine.** Ids, ports, sizes, states and anything the runtime said
are monospace; prose a person wrote is not. The split does real work.

**Shape carries state alongside colour.** Round dot, square dot, hollow square,
spinner. Reducing the phases to hue fails anyone who cannot separate green from
amber.

**Failure states explain themselves.** "OOM-killed", "crash-looping (12
restarts)", "exited with code 1". Keep the sentence, not just the badge — a red
chip with no reason sends someone to the logs for something already known.

**The API key is shown once.** No reveal affordance can be designed in: the
plaintext does not exist after creation. Only a SHA-256 is stored.

## Hard constraints

- **Go `html/template`, server-rendered.** No framework, no build step, no
  bundler. Components are HTML partials.
- **No CDN.** The node sits on a private network and is sometimes fully offline.
  Every asset local or inline. A webfont may be added only self-hosted.
- **Core actions work without JavaScript.** Deploy, undeploy, edit and revoke are
  form posts. JS may enhance; it may not be required to operate the thing.
- **Operations take minutes.** Starting a model: 8–10 minutes. Stopping one: up
  to 60 seconds. Any interaction model assuming instant feedback is wrong here.

## Known inconsistency

The pool bar on `/` still colours segments from the old binary `Drifted` flag
while the model cards below use the five-state phase, so the same model can read
amber above and green below. Fix in whichever direction the redesign goes, but
do not leave two state models on one screen.
