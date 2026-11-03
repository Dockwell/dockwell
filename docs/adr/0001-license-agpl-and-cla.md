# 0001. AGPL-3.0 core, contributions under a CLA

- Status: Accepted
- Date: 2026-11-03

## Context

The PRD's model has three editions: a free, open-source Community edition; an Organization edition whose extra modules are source-available; and Dockwell Cloud, a hosted control plane. The rule is to paywall the organization, never the machine. The PRD asked for a choice between AGPL-3.0 for the core with source-available Organization modules, or Apache-2.0 throughout.

Apache-2.0 would let anyone resell a hosted Dockwell without giving changes back. The AGPL keeps self-hosting free and requires anyone running a modified version as a service to publish their changes.

An open core under the AGPL creates a second problem. The paid editions combine the core with modules that are not AGPL. Community contributions received only under the AGPL could not be shipped in those editions.

## Decision

- The core is licensed under the GNU Affero General Public License v3.0 only (SPDX `AGPL-3.0-only`). The full text is in [`LICENSE`](../../LICENSE).
- Organization modules will live in their own top-level directory under a separate source-available licence. That licence is chosen in its own record when the first module lands.
- Contributors sign a Contributor License Agreement ([`CLA.md`](../../CLA.md)), adapted from the Apache ICLA. It grants Dockwell the right to distribute contributions under other terms too. In return, it commits Dockwell to keeping every contribution in the core available under the AGPL or another OSI-approved licence.

## Consequences

- Self-hosters run Dockwell freely, and a hosted resale of a modified Dockwell has to publish its source.
- The CLA adds friction for first-time contributors. It needs a legal entity, a review by counsel and a CLA bot before any outside pull request can be merged. Until then the CLA is a draft, and outside pull requests can be reviewed but not merged.
- Some companies don't allow AGPL dependencies. That matters little for a product people run, but it would matter for libraries others embed. The API schema and the generated client SDKs probably need a permissive licence, such as Apache-2.0, so integrations aren't bound by the AGPL. Decide that in its own record before the first SDK release.
- Every source file carries an `SPDX-License-Identifier` header, checked in CI.
