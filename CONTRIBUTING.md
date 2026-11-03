# Contributing to Dockwell

Thanks for your interest. Dockwell is at the very start of its first phase, so the shape of the project is still being set. This guide says how to help now, and it grows as the code does.

## Where help is most useful now

- **Feedback on the plan.** Read the [product requirements](docs/product/prd.md) and the [design](design/README.md), and open an issue where something is wrong, missing or unclear. This matters most if you run servers today with 1Panel, Coolify, Dokploy or by hand.
- **Your migration story.** What would you need to move your servers to Dockwell? Importers are planned for 1Panel, Coolify, Dokploy and plain Compose.
- **Issues labelled `good first issue`**, once the first code lands.

## Before you write code

- Pick an existing issue, or open one first for anything bigger than a small fix, so nobody's work is wasted.
- Changes to the architecture, the API schema, the data model, or a dependency with a wide reach need a decision record in [`docs/adr/`](docs/adr/). The template is in its README.
- Every pull request targets `main`, stays small enough to review in one sitting, and says what it changes and how you tested it.

## How Dockwell writes

The product has one voice, set out in the design system's [content fundamentals](design/system/README.md#content-fundamentals). The short version, for UI text, CLI output, errors and docs:

- Sentence case everywhere. No emoji, no exclamation marks, no filler ("simply", "just", "successfully").
- Address the reader as "you"; the product never says "I" or "we".
- Lead with the fact, then the consequence, then the fix. Errors say how to fix the problem and never apologize.
- Numbers carry units with a space: `1.8 GB`, `41 s`.
- Say *server* and *control server* in the product, and *agent* for the binary in docs and the CLI. Never *master* or *slave*.

## Contributor License Agreement

Before we merge your first pull request, you sign the [Contributor License Agreement](CLA.md) once. Dockwell's core is licensed under the AGPL-3.0, and the paid Organization edition and Dockwell Cloud are built on that same core. The CLA lets the project ship your contribution there too. Your contribution always stays available under the AGPL in the open-source core, and you keep the copyright.

The CLA is still a draft, and the bot that collects signatures isn't set up yet. Until it is, we can review outside pull requests but can't merge them.

## Development setup

Build and test instructions arrive with the first code (milestone M0).

## Code of conduct and security

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md). Report security problems privately as described in [SECURITY.md](SECURITY.md), never in a public issue.
