# StatusBadge

Shows the state of a server, app, deploy or job as a shape, a word and a colour together.

**You provide** `status` (healthy, degraded, down, deploying, stopped). Optionally a `label` to name the subject instead of the state, and `meta` for duration ("for 6 min").

- The five shapes are fixed: filled circle healthy, triangle degraded, square down, spinning arc deploying, hollow ring stopped. Never recolour a shape or reuse a shape for another meaning.
- `dot` sits in tables, lists and timelines. `pill` is for headers and cards where the status is the headline.
- Never show colour without the shape or the word. Colour-blind users and grayscale screenshots must still read the state.
- Deploying is the only animated state; it stops under reduced motion.
