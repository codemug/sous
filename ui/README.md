# The prototype

`Sous.dc.html` is the designer's finished prototype, kept as the reference the
current UI was built from. It is a **browser mock**, not shippable code — the
bindings are client-side and `support.js` is the mock's own runtime. Nothing
here is served.

It is in the repo because the reasoning behind the interface is easier to check
against the thing it was drawn from than against a description of it. When the
UI and this disagree, this is what was intended — but `DESIGN.md` outranks both,
because a few of its rules are claims the interface makes rather than choices
about how it looks.

Figures in the prototype are real ones from `recipes/` and the boot logs, so the
layouts are sized against true strings rather than placeholders.

## What was built differently, and why

- **Key scope** was shown per-model when the backend had none. Rather than drop
  the line or fake it, per-model scoping was built — see `internal/apikey`.
- **Re-fetch estimates** in the delete confirmation quoted minutes. No download
  rate is recorded anywhere, so the confirmation quotes size only.
- **The rail's 4px pool bar** and a narrow-width treatment for the bar and the
  stepper are not done. The notes put the second explicitly out of scope.
