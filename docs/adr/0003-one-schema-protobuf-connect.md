# 0003. One API schema in Protobuf, served with Connect

- Status: Accepted
- Date: 2026-11-03

## Context

Principle 1 of the PRD: the UI, API, CLI, Git config and MCP server are views of one model, and anything doable in one is doable in all. The PRD wants one schema that generates REST, typed SDKs, the CLI and the MCP server. Agents talk to the control plane over gRPC with mTLS, and the browser needs live updates with under 1 s of latency from server to browser.

## Decision

- The API is defined in Protocol Buffers under `proto/dockwell/v1/` and managed with `buf`. CI runs `buf lint` and `buf breaking` against `main`.
- It is served with Connect (`connectrpc.com/connect`). The same handlers speak gRPC to agents and the CLI, and the Connect protocol (JSON over HTTP) to the browser and to `curl`.
- Resource-style REST paths come from `google.api.http` annotations, transcoded by Vanguard (`connectrpc.com/vanguard`). An OpenAPI document is generated from the same schema.
- Generated from the schema: Go server interfaces, the TypeScript client (`protobuf-es` and `connect-es`), the CLI's commands and the MCP server's tools. Custom method options mark what the generators need, for example that a method only proposes a plan, or that it is destructive and needs approval.
- Live state uses server-streaming RPCs. The interactive terminal is the one bidirectional exception, and it runs over a WebSocket.

## Consequences

- The schema is the public API. The web panel uses only the generated client, so nothing is UI-only.
- Breaking changes are caught in review by `buf breaking`, and versioning is explicit (`v1`, then `v2` side by side).
- One more toolchain (`buf`) runs in CI, and contributors learn Protobuf conventions.
- The CLI and MCP generators are ours to build. They are small, but they are code we own.
