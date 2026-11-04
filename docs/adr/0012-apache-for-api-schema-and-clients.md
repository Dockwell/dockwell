# 0012. Apache-2.0 for the API schema and its generated clients

- Status: Accepted
- Date: 2026-11-04

## Context

[ADR 0001](0001-license-agpl-and-cla.md) put the core under the AGPL-3.0, and noted that some companies don't allow AGPL dependencies. That matters little for a product people run, and a great deal for code that other people build into their own tools. The PRD's Automate area promises typed SDKs and a CLI generated from one schema ([ADR 0003](0003-one-schema-protobuf-connect.md)). An integration that builds in an AGPL client, such as a deploy step in someone's CI system, an internal portal or a Terraform provider, becomes a combined work under the AGPL once it is distributed, and many legal teams refuse that outright. ADR 0001 left the licence for the schema and the clients to its own record, before the first SDK release (#25).

What is in question:

- `proto/`: the schema. It holds the public API (`dockwell.v1`), the agent's protocol (`dockwell.agent.v1`), the resource specs (`dockwell.resource.v1alpha1`) and the options every method sets.
- `gen/`: the Go code buf generates from it with protoc-gen-go and protoc-gen-connect-go. These are the messages, and the Connect clients and handler interfaces.
- `packages/api/`: the TypeScript package `@dockwell/api`. It holds the messages and service descriptors protoc-gen-es generates, and a hand-written `src/index.ts` that exports them.

The generated code comes from the schema, and the generators copy the comment above each file's `syntax` line into every file they write, so the header in `proto/` is the header in the clients. The libraries the clients run on are permissive already: `google.golang.org/protobuf` is BSD-3-Clause, and Connect and `@bufbuild/protobuf` are Apache-2.0. Today the only copyleft code in a Dockwell client is Dockwell's own.

The options weighed:

1. **Apache-2.0.** Permissive, with an explicit patent grant from every contributor. It is the licence of Connect and the protobuf-es runtime, corporate policies allow it almost everywhere, and Apache-2.0 code can be combined into an AGPL-3.0 work.
2. **MIT.** Shorter and just as permissive, but it has no patent grant, and the libraries next to it are Apache-2.0 anyway.
3. **MPL-2.0.** File-level copyleft: changes to Dockwell's own files stay open, while the rest of an integration can be closed. Some policies still flag it, and people rarely change generated files, so it would protect little.
4. **AGPL-3.0 with a linking exception.** A custom licence that each legal team would have to read, and that most policies treat as AGPL.

The other question is scope. Only the public API needs to reach integrations, so `dockwell.agent.v1` could stay AGPL. That would draw the boundary between packages inside one directory tree, decided file by file, and easy to get wrong. A licence on the schema doesn't stop anyone writing a compatible agent or control plane from the protocol either. What the AGPL protects is the implementation in `internal/`.

## Decision

- These paths are licensed under the Apache License 2.0 (SPDX `Apache-2.0`):
  - `proto/`, the whole API schema, every package in it;
  - `gen/`, the Go code generated from it;
  - `packages/api/`, the TypeScript package `@dockwell/api`, including the hand-written `src/index.ts`. Its `package.json` says `"license": "Apache-2.0"`.
- Everything else stays AGPL-3.0-only, as ADR 0001 decided. That includes the code in `internal/` that serves and calls the API, the panel in `web/`, and the build configuration that drives generation (`buf.yaml`, `buf.gen.yaml` and the Makefile).
- Each of the three directories has a `LICENSE` file with the unmodified Apache-2.0 text, so GitHub, npm and Go tooling find it next to the code. buf empties `gen/` before it writes and can't leave a file there, so `make generate` copies `proto/LICENSE` back into `gen/` afterwards, and `buf generate` on its own deletes it.
- Every source file in the three directories starts with `SPDX-License-Identifier: Apache-2.0`. Generated files get it from the `.proto` file they come from.
- `scripts/check-spdx.sh` enforces the boundary in CI:
  - every source file has exactly one SPDX identifier in its first 5 lines, and it is `Apache-2.0` in the three directories and `AGPL-3.0-only` everywhere else, compared as a whole value;
  - it now checks generated code too, which it used to skip;
  - every `package.json`, read as JSON, declares the licence of its directory;
  - each of the three `LICENSE` files matches a pinned SHA-256 of the text at <https://www.apache.org/licenses/LICENSE-2.0.txt>, and the check names each file that is missing or differs.
- The `dockwell` binary compiles `gen/` in with the AGPL core. The binary as a whole is offered under the AGPL-3.0, and its release archive keeps shipping the AGPL text as `LICENSE`.

### The CLA

The [CLA](../../CLA.md) covers both licences as written, with one weak point for the maintainer to decide on.

- Section 2 lets Dockwell distribute contributions "under the GNU Affero General Public License v3.0 or under other licence terms". Apache-2.0 is one of those terms, so the grant already covers publishing schema contributions under it. Section 3, the patent licence, covers the whole Project, whatever its licence.
- Section 4 commits Dockwell to keeping each contribution to the open-source core available "under the GNU Affero General Public License v3.0, or another licence approved by the Open Source Initiative". Apache-2.0 is approved by the OSI, so publishing schema contributions under it keeps that promise.
- The weak point: by those words, Dockwell could later move a schema contribution from Apache-2.0 to the AGPL alone, and the agreement doesn't say whether `proto/` counts as the "open-source core". Integrators and contributors would want to know that the schema stays permissive. The maintainer decides whether section 4 should also say that contributions to the Apache-2.0 directories stay available under Apache-2.0. That is a change to legal text, so it goes to counsel with the rest of the draft, and this record doesn't change `CLA.md`.

### Schema contributions

- A pull request that changes `proto/`, or the code generated from it, is a contribution under the CLA like any other, and Dockwell publishes it under Apache-2.0.
- Material can move from the AGPL side into the Apache-2.0 directories only when Dockwell can license it under Apache-2.0. That holds for Dockwell's own code and for contributions under the CLA. Code or text taken from third-party AGPL or GPL projects can't go into them.
- A `.proto` file written before this record with an AGPL header fails `check-spdx.sh` until its header says Apache-2.0. After the change, `make generate` updates the generated files to match.

## Consequences

- Anyone can build `@dockwell/api` or the Go packages under `gen/` into their own tools, closed source included, as long as they keep the licence and notices that Apache-2.0 requires.
- Anyone can also write a closed-source agent or control plane that speaks the schema. They could already do that from the protocol. The AGPL still covers Dockwell's implementation, which is where the work is.
- One repository now has two licences. The headers, the `LICENSE` files and the check carry the boundary, so moving a file into or out of the three directories means changing its header, and CI says so.
- The Go packages under `gen/` sit inside the module `github.com/dockwell/dockwell`, whose root `LICENSE` is the AGPL. Importing them compiles only Apache-2.0 code, but `go get` downloads the whole module. pkg.go.dev reads licence files from a package's directory up to the module root, so it lists both licences for those packages. Before the first Go SDK release, decide whether to move `gen/` into a module of its own, such as `github.com/dockwell/dockwell/gen` with its own `go.mod`, so the module people import is Apache-2.0 alone.
- `@dockwell/api` is still private, and publishing it to npm is its own step. Clients generated from `proto/` for more languages later are Apache-2.0 too, by the same rule.
