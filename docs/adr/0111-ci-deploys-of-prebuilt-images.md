# 0111. CI deploys of prebuilt images

- Status: Accepted
- Date: 2026-11-26

## Context

Dockwell can build from Git or deploy the image a pipeline has already pushed. The second path needs a receipt tied to the commit, a way for the pipeline to wait for rollout, and a token that does just that. A pipeline may retry a request whose response it lost, including while Dockwell restarts. See [ADR 0046](0046-builds-from-git.md), [ADR 0071](0071-api-tokens-narrowed-to-projects-and-a-role.md) and [ADR 0098](0098-connecting-github-as-an-app.md).

## Decision

### Correlation and rollout

- `DeployService` accepts a CI provider, repository URL, full commit SHA, Git ref and run URL. URLs are HTTPS without user information, query strings or fragments. These facts come from the pipeline and identify its reported source. Dockwell records the first successful proposal's values with its change.
- The plan keeps the requested image reference and the immutable manifest digest. Accept an image by digest, or resolve its tag on a target server using the app's registry login and the existing image-read call. Store the pinned image in the proposed service spec. The rollout and rollback use the usual revision engine. The deployment record inherits provenance from the change that produced its revision.
- Migration 0061 adds a unique key on service ID, manifest digest and normalized commit SHA. The transaction that records the plan claims the key. A racing request returns the winning change and leaves no duplicate plan or event. A retry receives the original change even if it applied, failed or was discarded. A tag that moves to another digest is a different artifact. Pass a digest for retries that must keep the exact original image.
- The API returns provenance for changes and service deployments. Deployments and Activity show the commit and CI run. GitHub and GitLab have known commit URL formats, and other providers supply a commit URL.
- `dockwell services deploy --wait` applies the plan and follows its terminal result. It prints a link to the change and leaves approval of protected or host-sensitive plans to someone in the panel. It exits successfully on an applied rollout. Failure, rollback or discarding fails the job. A wrapper's CI job timeout limits how long a run waits for approval or for a server.

### Tokens and shipped workflows

- The Deployments sheet uses `TokenService.CreateToken` and the existing passkey step-up. Deploy scope is a read-write credential bound to exactly one service, its existing environment and project, capped at developer. It allows planning a correlated deploy for that service and applying, reading and streaming that deploy's change. The issuer's current role and ordinary host and protected-environment rules still apply. The token name and target are audited using existing token events. Only the creation response carries the secret.
- The GitHub composite action is at `ci/github/action.yml`, addressed by the Dockwell release tag or commit. With a commit pin, pass a matching release tag to choose the CLI. The GitLab input template at `ci/gitlab/dockwell-deploy.yml` is includable from the same release tag and can move unchanged to a GitLab Catalog project. Both invoke the same script from the release source. The script uses a pinned Cosign executable, the exact Dockwell tag's release-workflow certificate identity and issuer, and the signed SHA-256 of the Linux amd64 or arm64 full binary archive. The CI token is read from the runner's environment.
- No agent protocol change is required. Public fields are `DeployServiceRequest.deploy_origin` 6, `Change.deploy_origin` 25 and `ServiceDeployment.deploy_origin` 7. `DeployOrigin` uses fields 1 through 8. `DeployProvider` uses values 0 through 3. `TokenScope` adds value 3, `CreateTokenRequest.deploy_service` is 9, and `Token.deploy_service` and `Token.deploy_environment` are 12 and 13.

### Status in slice 2

A durable outbox should write status transitions in the same transaction as the corresponding change state. An entry needs the change ID, service ID, provider, repository, commit, ref and run URL, the image digest, the panel change URL, and the queued, applying, healthy, failed or rolled-back state with a monotonic transition number. Keep provider installation or credential IDs separately from the pipeline's provenance. Record the provider's remote status ID, an idempotency key for each transition, delivery attempts, next attempt and last error. A worker can then resume after restart and retry delivery without regressing a newer status.

GitHub will need the GitHub App permission upgrade and an installation that actually reaches the repository before Dockwell posts a check run or deployment status. GitLab will need a credential with commit-status access for the matching project. The deploy token supplies provenance. These credentials stay with the integration. Slice 2 can add both adapters without moving slice 1's fields.

## Consequences

The panel and API can trace an external deployment to the exact source and artifact as soon as it is planned. One pipeline call can follow the ordinary health-gated rollout. Provider-facing status delivery follows in slice 2. See [Deploy from CI](../deploy-from-ci.md).
