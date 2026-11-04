# 0015. REST paths and an OpenAPI document from the schema

- Status: Accepted
- Date: 2026-11-04

## Context

[ADR 0003](0003-one-schema-protobuf-connect.md) chose one Protobuf schema, served with Connect, and said resource-style REST paths would come from `google.api.http` annotations, transcoded by Vanguard, with an OpenAPI document generated from the same schema. The PRD wants REST among the surfaces that one schema generates, and scripts, CI systems and integrations need an API they can call with `curl` or any OpenAPI tool.

That left these questions open:

- What the paths look like, and what keeps new methods consistent.
- What REST does for streaming methods. `google.api.http` can't express bidirectional streams, and REST has no standard way to carry a stream of messages. `ServerService.WatchServers` is a server stream today.
- How REST fits the guard from [ADR 0011](0011-claim-and-sign-in.md). REST accepts requests that Connect's content types rule out, such as a form a page on another site posts, so the CSRF checks have to hold for them too.
- Which generator writes the OpenAPI document, where it lives, and how CI keeps it current and valid.

## Decision

### Paths

- Every unary method of `dockwell.v1` has exactly one `google.api.http` rule, and every path starts with `/v1/`. `/v1/` is reserved for REST: the panel has no pages under it, and the guard treats it as API.
- The paths follow Google's API design guide:
  - Get and List read a resource or a collection with GET ([AIP-131](https://google.aip.dev/131), [AIP-132](https://google.aip.dev/132)): `GET /v1/servers`, `GET /v1/servers/{id}`.
  - Create posts to the collection ([AIP-133](https://google.aip.dev/133)): `POST /v1/tokens`.
  - Delete uses DELETE when the resource goes away ([AIP-135](https://google.aip.dev/135)): `DELETE /v1/passkeys/{id}`.
    - Since [ADR 0022](0022-secrets-encrypted-at-rest.md#revealing-a-value-and-changing-how-you-sign-in-a-step-up), that example is a custom method: removing a passkey takes a step-up's proof, which a DELETE could carry only in its URL, where logs keep it. So it is `POST /v1/passkeys/{id}:remove`, with the proof in its body. A delete that takes nothing but the ID still uses DELETE; no method has one now.
  - Everything else is a custom method, a POST to `resource:verb` or `collection:verb` with the whole request as the body ([AIP-136](https://google.aip.dev/136)): `POST /v1/sessions/{id}:revoke`, `POST /v1/sessions:signInWithCode`, `POST /v1/claim:begin`.
  - Things there is one of are singletons, which have no Create or Delete ([AIP-156](https://google.aip.dev/156)): `/v1/authState`, `/v1/totp`, `/v1/version`.
- Path variables bind the request's `id` field. The schema names resources by ID rather than by [AIP-122](https://google.aip.dev/122) resource names, and renaming the fields would break every client.
- Bodies and responses are the protobuf JSON mapping: lowerCamelCase field names, enums by name, 64-bit integers as strings, timestamps in RFC 3339. Errors are a `google.rpc.Status` in JSON with the HTTP status for its code, and the reason under `Dockwell-Error`, as for every protocol.
  - The guard's own refusals are too: `origin` is `PERMISSION_DENIED` with 403, and `misdirected` is `INVALID_ARGUMENT` with 421, as no code means another host and the client has to fix the request.
  - The one exception is Vanguard's 415 for a body that isn't JSON, which is plain text.
- A POST with no body is a request with no fields, whatever its content type and whether or not it is chunked, so `curl -X POST` and `curl -X POST -d ''` both work. Only POSTs take a body.
- `TestEveryMethodHasARESTPath` in `internal/control` enforces the rules:
  - each unary method has one rule, under `/v1/`, and no two share a verb and path;
  - a GET is a read with no side effects (`EFFECT_READ` and `NO_SIDE_EFFECTS`), and every such read is a GET;
  - GETs and DELETEs have no body, and POSTs take the whole request;
  - a DELETE is destructive;
  - streaming methods have no rule.

### Streams have no REST path

- Vanguard serves a server stream over REST only when the response is `google.api.HttpBody`, and answers every other call with 415. An `HttpBody` stream of JSON lines would work, but the schema and the OpenAPI document would then describe opaque bytes instead of `WatchServersResponse`, and the stream would need its own framing that no client library knows.
- So `WatchServers`, and any later server, client or bidirectional stream, has no `google.api.http` rule, and the test above fails if one gets one. Over REST:
  - poll `GET /v1/servers` or `GET /v1/servers/{id}`, which carry the same `Server` messages;
  - or open the stream with Connect, gRPC or gRPC-Web, which serve it at `/dockwell.v1.ServerService/WatchServers` with the same credentials. Connect works over HTTP/1.1 with JSON (`Content-Type: application/connect+json`, with each message behind a 5-byte envelope), so it needs no gRPC tooling.
- The schema says so on `ServerService` and `WatchServers`, so the OpenAPI document and the generated clients say it too.
- Interactive bidirectional streams, such as the terminal, run over a WebSocket, as ADR 0003 decided.
- If REST clients need pushed updates later, a server-sent events path that is transcoded to a Connect call, and so passes the same interceptor, is the way to add them. That is its own decision.

### Serving

- `connectrpc.com/vanguard` v0.4.0 serves the REST paths. v0.5.0 needs Connect v2, and the rest of Dockwell is on Connect v1.
- Vanguard turns each REST call into a Connect call to the same handler that serves the procedure path. The guard, the interceptor, the rate limits, the audit log and the reasons in `Dockwell-Error` therefore apply to REST without a second copy. RPC logs name the protocol a call arrived with, `rest` included.
- Procedure paths for Connect, gRPC and gRPC-Web go to the handlers directly, as before. Only `/v1/` goes through Vanguard.
- A path under `/v1/` that no method has gets 404 with a `google.rpc.Status`, which points to the OpenAPI document.

### Cross-site requests

- A browser sends some requests to another site without asking first: GETs, and POSTs of forms or plain text. Connect's procedure paths never accept those content types, but REST paths are meant to be simple, so the guard can't lean on that.
- The guard applies its origin checks to `/v1/` exactly as to procedure paths. A request whose `Origin` isn't the panel's, or whose `Sec-Fetch-Site` says another site, is refused with the reason `origin`, before Vanguard reads it.
- The interceptor runs on the call Vanguard makes, so a write that comes with the session cookie must still carry the panel's `Origin` or `Sec-Fetch-Site: same-origin`.
- GETs reach only reads with no side effects, which the test above enforces. A cookie's GET with no fetch metadata is let through, as for a URL typed into the address bar, and only reads.
- Vanguard also refuses request bodies that aren't JSON with 415. That is a second barrier, not the one relied on.
- A request with an API token skips the origin checks; [ADR 0016](0016-scoped-api-tokens.md) says why.

### The OpenAPI document

- `protoc-gen-openapi` from `github.com/google/gnostic` v0.7.1, pinned in `tools/go.mod`, writes it during `make generate` to `gen/openapi/openapi.yaml`. It is OpenAPI 3.0.3, which more tools read than 3.1, and it describes the REST paths alone, from their `google.api.http` rules, with JSON names, enums as strings and `google.rpc.Status` as the error response. `protoc-gen-connect-openapi` was the alternative; it centres on Connect's own paths and writes OpenAPI 3.1.
- The document comes from the schema, so it is Apache-2.0 like the rest of `gen/` ([ADR 0012](0012-apache-for-api-schema-and-clients.md)).
- The control plane serves it at `GET /v1/openapi.yaml`, without a session: it is the same for every install of a version, and the schema is public anyway. `go:embed` can't reach `gen/`, so `make generate` also copies it to `internal/control/apidoc/openapi.yaml`, and a Go test fails when the two differ.
- `make check-generated` covers both copies, so CI fails when the document is stale.
- `make check-openapi` validates it with `cmd/validate` from `github.com/getkin/kin-openapi` v0.151.0, also pinned in `tools/go.mod`. It runs in `make lint` and in CI's API schema job.

### The `google.api` annotations

- `google/api/annotations.proto` comes from `buf.build/googleapis/googleapis`, pinned in `buf.yaml` to a commit at least 14 days old and locked in `buf.lock`.
- The Go code imports those files' generated code from `google.golang.org/genproto/googleapis/api`, which Vanguard also uses, so managed mode leaves their Go package alone.
- The TypeScript client needs them generated beside its own files, in `packages/api/src/gen/google/`. They keep Google's Apache-2.0 notice, and `make generate` puts `SPDX-License-Identifier: Apache-2.0` above it, as every source file in `packages/api/` starts with ([ADR 0012](0012-apache-for-api-schema-and-clients.md)). `scripts/check-spdx.sh` checks for both.

## Consequences

- Anything that speaks HTTP and JSON can use the API, such as `curl -H "Authorization: Bearer $TOKEN" https://panel.acme.dev/v1/servers`. Any OpenAPI tool can generate a client from `/v1/openapi.yaml`.
- Every new unary method needs a `google.api.http` rule that follows the rules above, or a test fails. Reviewers check that the verb and path fit the method.
- REST paths are part of the public API. `buf breaking` doesn't compare `google.api.http` options, so a changed path isn't caught by it; treat one like a renamed field, and don't.
- `buf lint`, `buf breaking` and `make generate` now fetch the googleapis module from the Buf Schema Registry the first time, and cache it. CI needs network access to `buf.build`, as it already does for Go modules and npm.
- REST clients can't receive live updates. They poll, or switch to Connect for the stream.
- `/v1/` belongs to the API, so a panel route must never start with it.
