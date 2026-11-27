// SPDX-License-Identifier: AGPL-3.0-only

import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  GitProviderService,
  type ServiceInfo,
  StepUpAction,
  SystemService,
  TokenScope,
  TokenService,
} from "@dockwell/api";
import { Button, Callout, CopyButton, SegmentedControl, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useAllResources } from "../projects/useProjects";
import { nameOf } from "./app";

type Provider = "github" | "gitlab";
const releaseTag = /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/;
const pseudoVersion = /\d{14}-[0-9a-f]{12}$/;
const secretName = "DOCKWELL_DEPLOY_TOKEN";

function normalizeRepoCompare(value: string): string {
  return value
    .trim()
    .replace(/\/+$/, "")
    .replace(/\.git$/i, "")
    .toLowerCase();
}

function canonicalGitHubRepoUrl(url: string, fullName: string): string {
  return url.startsWith("https://") ? url : `https://github.com/${fullName}`;
}

export function deployWorkflow(provider: Provider, panel: string, service: string, tag: string, templateRef = tag) {
  if (provider === "gitlab") {
    return `include:
  - remote: https://raw.githubusercontent.com/Dockwell/dockwell/${templateRef}/ci/gitlab/dockwell-deploy.yml
    inputs:
      panel: ${JSON.stringify(panel)}
      token: '$${secretName}'
      service: ${JSON.stringify(service)}
      image: '$DOCKWELL_IMAGE'
      version: ${JSON.stringify(tag)}
      build-job: build
`;
  }
  return `name: Build and deploy to Dockwell

on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write

jobs:
  deploy:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false
      - name: Build and push
        id: build
        env:
          GHCR_TOKEN: \${{ github.token }}
        run: |
          set -euo pipefail
          image="ghcr.io/\${GITHUB_REPOSITORY,,}"
          printf '%s\\n' "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
          docker buildx build --push -t "$image:$GITHUB_SHA" .
          digest=$(docker buildx imagetools inspect "$image:$GITHUB_SHA" --format '{{.Manifest.Digest}}')
          printf 'image=%s@%s\\n' "$image" "$digest" >> "$GITHUB_OUTPUT"
      - uses: Dockwell/dockwell/ci/github@${tag}
        with:
          panel: ${JSON.stringify(panel)}
          token: \${{ secrets.${secretName} }}
          service: ${JSON.stringify(service)}
          image: \${{ steps.build.outputs.image }}
`;
}

export function CIDeploySheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const may = useMay();
  const build = useQuery(SystemService.method.getVersion, {}).data?.build;
  const resources = useAllResources();
  const envResource = (resources.data ?? []).find((r) => r.id === service.resource?.scope);
  const projectId = envResource?.resource?.scope ?? "";
  const buildRepo = service.spec?.build?.source.case === "git" ? service.spec.build.source.value.url : "";

  const version = build?.version ?? "";
  const bundledTag = !build?.modified && releaseTag.test(version) && !pseudoVersion.test(version) ? version : "";
  const [provider, setProvider] = useState<Provider>("github");
  const [pickedRepo, setPickedRepo] = useState<string>();
  const [customRepo, setCustomRepo] = useState("");
  const [release, setRelease] = useState<string>();
  const [made, setMade] = useState<{ secret: string; hint: string; repository: string }>();
  const [shown, setShown] = useState(false);
  const [expires, setExpires] = useState("never");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const tag = release ?? bundledTag;
  const templateRef = tag === bundledTag && /^[0-9a-f]{40}$/.test(build?.commit ?? "") ? build?.commit : undefined;
  const workflow = deployWorkflow(provider, window.location.origin, service.id, tag || "vX.Y.Z", templateRef);

  const mayGitHub = may(GitProviderService.method.listGitRepositories, projectId ? { project: projectId } : undefined);
  const mayGitLab = may(GitProviderService.method.listGitLabProjects, projectId ? { project: projectId } : undefined);
  const ghRepos = useQuery(
    GitProviderService.method.listGitRepositories,
    { project: projectId },
    { enabled: Boolean(projectId) && provider === "github" && mayGitHub.allowed },
  );
  const glProjects = useQuery(
    GitProviderService.method.listGitLabProjects,
    { project: projectId },
    { enabled: Boolean(projectId) && provider === "gitlab" && mayGitLab.allowed },
  );

  const repoOptions: { value: string; label: string }[] =
    provider === "github"
      ? (ghRepos.data?.repositories ?? []).map((r) => ({
          value: canonicalGitHubRepoUrl(r.url, r.fullName),
          label: r.fullName,
        }))
      : (glProjects.data?.projects ?? []).map((p) => ({ value: p.url, label: p.pathWithNamespace }));

  const defaultRepo = (() => {
    if (buildRepo) {
      const target = normalizeRepoCompare(buildRepo);
      const matched = repoOptions.find((opt) => normalizeRepoCompare(opt.value) === target);
      if (matched) return matched.value;
    }
    return repoOptions[0]?.value ?? "";
  })();

  const hasGitLabConnections = provider === "gitlab" && (glProjects.data?.connections.length ?? 0) > 0;
  const selectedRepo =
    repoOptions.length > 0 ? (pickedRepo ?? defaultRepo) : hasGitLabConnections ? customRepo.trim() : "";

  async function createToken() {
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(TokenService, transport);
      const name = nameOf(service);
      const result = await withStepUpIfAsked(transport, StepUpAction.CREATE_TOKEN, "", (proof) =>
        client.createToken({
          name: t`CI deploy · ${name}`.slice(0, 100),
          scope: TokenScope.DEPLOY,
          deployService: service.id,
          deployRepository: selectedRepo,
          proof,
          ...(expires === "never"
            ? {}
            : { expiration: { case: "ttl", value: durationFromMs(Number(expires) * 86_400_000) } }),
        }),
      );
      setMade({
        secret: result.secret,
        hint: result.token?.hint ?? "dwt_••••••••",
        repository: result.token?.deployRepository ?? "",
      });
    } catch (error) {
      setProblem(explain(error));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Sheet
      open
      title={t`Deploy from CI`}
      onClose={onClose}
      className="app-sheet"
      footer={
        made ? (
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="primary" loading={busy} onClick={() => void createToken()}>
              <Trans>Create deploy token</Trans>
            </Button>
          </>
        )
      }
    >
      <div className="ci-deploy-sheet">
        <SegmentedControl<Provider>
          label={t`CI provider`}
          value={provider}
          onChange={(next) => {
            setProvider(next);
            setPickedRepo(undefined);
          }}
          options={[
            { value: "github", label: "GitHub Actions" },
            { value: "gitlab", label: "GitLab CI" },
          ]}
        />
        <Link className="cp-link" to="/settings/api-tokens">
          <Trans>Manage API tokens</Trans>
        </Link>
        {made ? null : (
          <>
            {repoOptions.length > 0 ? (
              <SelectField
                label={t`Status repository`}
                value={selectedRepo}
                onChange={(event) => setPickedRepo(event.target.value)}
                hint={t`Dockwell reports commit and PR status only to this repository.`}
                options={[...repoOptions, { value: "", label: t`Don't report status` }]}
              />
            ) : hasGitLabConnections ? (
              <TextField
                label={t`Status repository`}
                value={customRepo}
                onChange={(event) => setCustomRepo(event.target.value)}
                placeholder="https://gitlab.example.com/group/project"
                hint={t`Optional. Dockwell reports commit status only to this repository.`}
              />
            ) : (
              <span className="app-muted">
                <Trans>
                  Connect {provider === "github" ? "GitHub" : "GitLab"} in{" "}
                  <Link className="cp-link" to="/settings/integrations">
                    Integrations
                  </Link>{" "}
                  to report deploy status back to commits.
                </Trans>
              </span>
            )}
            <SelectField
              label={t`Token expires`}
              value={expires}
              onChange={(event) => setExpires(event.target.value)}
              options={[
                { value: "never", label: t`Never` },
                { value: "30", label: t`In 30 days` },
                { value: "90", label: t`In 90 days` },
              ]}
            />
          </>
        )}
        {problem ? <Callout tone="critical" title={problem} /> : null}
        {made ? (
          <div className="ci-deploy-field">
            <strong>
              <Trans>Secret</Trans> · <code>{secretName}</code>
            </strong>
            <div className="ci-deploy-row">
              <code className="ci-deploy-secret">{shown ? made.secret : made.hint}</code>
              <Button
                variant="ghost"
                size="sm"
                icon="eye"
                aria-pressed={shown}
                aria-label={shown ? t`Hide the token` : t`Show the whole token`}
                onClick={() => setShown(!shown)}
              />
              <CopyButton text={made.secret} label={t`Copy token`} copiedLabel={t`Token copied`} />
            </div>
            {made.repository ? (
              <span className="app-muted">
                <Trans>Reports deploy status to {made.repository}.</Trans>
              </span>
            ) : null}
          </div>
        ) : null}
        {bundledTag ? null : (
          <TextField
            label={t`Release tag`}
            value={tag}
            placeholder="vX.Y.Z"
            onChange={(e) => setRelease(e.target.value)}
          />
        )}
        <div className="ci-deploy-field">
          <div className="ci-deploy-row">
            <strong>
              <Trans>Workflow</Trans>
            </strong>
            <CopyButton
              text={workflow}
              label={t`Copy workflow`}
              copiedLabel={t`Workflow copied`}
              disabled={!releaseTag.test(tag)}
            />
          </div>
          {provider === "gitlab" ? (
            <span className="app-muted">
              <Trans>The build job exports DOCKWELL_IMAGE as a dotenv artifact.</Trans>
            </span>
          ) : null}
          <pre>
            <code>{workflow}</code>
          </pre>
        </div>
      </div>
    </Sheet>
  );
}
