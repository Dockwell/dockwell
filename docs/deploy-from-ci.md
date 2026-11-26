# Deploy an image from CI

In the app's **Deployments** tab, open **Deploy from CI**. Choose GitHub Actions or GitLab, create a deploy token, and save it as the `DOCKWELL_DEPLOY_TOKEN` secret in your CI provider. The sheet fills in the panel address and service ID. Your runner needs to reach the panel and download Dockwell's signed Linux CLI release.

Use an app that runs a container image. Give its project a registry login if the image is private. Push the image from your build job first, and pass the published reference, ideally `registry.example.com/acme/web@sha256:…`.

## GitHub Actions

Add this step after your build pushes the image. Replace `vX.Y.Z` with a Dockwell release containing the action.

```yaml
- uses: Dockwell/dockwell/ci/github@vX.Y.Z
  with:
    panel: https://panel.example.com
    token: ${{ secrets.DOCKWELL_DEPLOY_TOKEN }}
    service: svc_example
    image: ${{ steps.build.outputs.image }}
```

When you pin the action to a commit, also pass `version: vX.Y.Z` to select a signed CLI release. Give the deploy job a timeout, such as `timeout-minutes: 20`. Provide Bash, curl, jq, tar and sha256sum on your Linux runner.

## GitLab CI

Save the token as a masked CI/CD variable. The template uses the `deploy` stage by default and accepts `stage` and `job` inputs if your pipeline uses other names. Set `DOCKWELL_IMAGE` to the published reference, or expose it from the build job through a dotenv artifact.

```yaml
include:
  - remote: https://raw.githubusercontent.com/Dockwell/dockwell/vX.Y.Z/ci/gitlab/dockwell-deploy.yml
    inputs:
      panel: https://panel.example.com
      token: '$DOCKWELL_DEPLOY_TOKEN'
      service: svc_example
      image: '$DOCKWELL_IMAGE'
      version: vX.Y.Z
```

The included job uses a Debian container and installs curl and jq. Give the job or pipeline a timeout appropriate for your rollout. Use the same release tag in the include URL and `version`.

## Follow the deploy

The job prints the panel link. Deployments and Activity show the source commit and the CI run. For a protected environment, an admin approves the change in the panel while the job waits.

Rerunning a job with the same service, digest and commit follows its original change, including after a control server restart. Pass an immutable image reference so a tag that moves between runs cannot select another artifact. If a rollout fails or rolls back, the job fails. To deploy the artifact from a subsequent commit, rerun the build and pass its published image.

See [ADR 0111](adr/0111-ci-deploys-of-prebuilt-images.md) and [verify a Dockwell release](releasing.md#verify-a-release).
