# Deploy an image from CI

In the app's **Deployments** tab, open **Deploy from CI**. Choose GitHub Actions or GitLab, create a deploy token, and save it as the `DOCKWELL_DEPLOY_TOKEN` secret in your CI provider. The sheet fills in the panel address and service ID. Your runner needs to reach the panel and download Dockwell's signed Linux CLI release.

Use an app that runs a container image. Give its project a registry login if the image is private. Push the image from your build job first, and pass the published reference, ideally `registry.example.com/acme/web@sha256:…`.

## GitHub Actions

The panel gives you a workflow that builds on pushes to `main`, publishes to GitHub Container Registry, and deploys the build's digest. For an existing build, export a digest-pinned reference from its `build` step and add the step below. Replace `vX.Y.Z` with a Dockwell release containing the action.

```yaml
- uses: Dockwell/dockwell/ci/github@vX.Y.Z
  timeout-minutes: 35
  with:
    panel: https://panel.example.com
    token: ${{ secrets.DOCKWELL_DEPLOY_TOKEN }}
    service: svc_example
    image: ${{ steps.build.outputs.image }}
```

When you pin the action to a commit, also pass `version: vX.Y.Z` to select a signed CLI release. Allow time for the CLI’s 30-minute deploy deadline and for the download and verification. Provide Bash, curl, jq, tar and sha256sum on your Linux runner.

## GitLab CI

Save the token as a masked CI/CD variable. Include the template in the pipeline that builds and pushes your image. The build job exports `DOCKWELL_IMAGE` as a dotenv artifact with the registry reference at its digest. The deploy job runs on pushes to the default branch after that job.

```yaml
include:
  - remote: https://raw.githubusercontent.com/Dockwell/dockwell/vX.Y.Z/ci/gitlab/dockwell-deploy.yml
    inputs:
      panel: https://panel.example.com
      token: '$DOCKWELL_DEPLOY_TOKEN'
      service: svc_example
      image: '$DOCKWELL_IMAGE'
      version: vX.Y.Z
      build-job: build
```

The included job uses Debian, installs curl and jq, and verifies the deploy script against the signed release checksums before running it. The panel can pin the include URL to the release's commit. The `version` input always names the release tag. Use `build-job`, `stage`, and `job` to match your pipeline's names.

## Follow the deploy

The job prints the panel link. Deployments and Activity show the source commit and the CI run. For a protected environment, an admin approves the change in the panel while the job waits.

Rerunning a job with the same service, digest and commit follows an active change or the successful rollout that is still current. A failed, discarded, stale or superseded change gives way to a new plan. Rerunning an older build deploys its older digest under the same approval rules as any deploy. Pass the immutable reference so a tag moving between runs cannot select another artifact.

`dockwell services deploy --wait` reconnects to the change after a dropped stream. With `--deploy-origin`, it also retries transient failures while asking for the plan. Set `--timeout` to a positive duration such as `30m`. The default is 30 minutes, including proposal, approval and rollout.

| Exit code | Outcome |
| --- | --- |
| 0 | Healthy |
| 1 | Failed |
| 4 | Still waiting for approval at the deadline |
| 5 | Timed out |
| 6 | Rolled back |
| 7 | Discarded |

See [ADR 0111](adr/0111-ci-deploys-of-prebuilt-images.md) and [verify a Dockwell release](releasing.md#verify-a-release).
