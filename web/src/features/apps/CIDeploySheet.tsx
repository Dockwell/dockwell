// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type ServiceInfo, StepUpAction, SystemService, TokenScope, TokenService } from "@dockwell/api";
import { Button, Callout, CopyButton, SegmentedControl, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { imageOf, nameOf } from "./app";

type Provider = "github" | "gitlab";
const releaseTag = /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/;
const pseudoVersion = /\d{14}-[0-9a-f]{12}$/;
const secretName = "DOCKWELL_DEPLOY_TOKEN";

export function deployWorkflow(provider: Provider, panel: string, service: string, image: string, tag: string) {
  if (provider === "gitlab") {
    return `include:
  - remote: https://raw.githubusercontent.com/Dockwell/dockwell/${tag}/ci/gitlab/dockwell-deploy.yml
    inputs:
      panel: ${JSON.stringify(panel)}
      token: '$${secretName}'
      service: ${JSON.stringify(service)}
      image: ${JSON.stringify(image)}
      version: ${JSON.stringify(tag)}
`;
  }
  return `name: Deploy to Dockwell

on:
  workflow_dispatch:
    inputs:
      image:
        description: Published image reference or digest
        required: true
        default: ${JSON.stringify(image)}

permissions: {}

jobs:
  deploy:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: Dockwell/dockwell/ci/github@${tag}
        with:
          panel: ${JSON.stringify(panel)}
          token: \${{ secrets.${secretName} }}
          service: ${JSON.stringify(service)}
          image: \${{ inputs.image }}
`;
}

export function CIDeploySheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const build = useQuery(SystemService.method.getVersion, {}).data?.build;
  const version = build?.version ?? "";
  const bundledTag = !build?.modified && releaseTag.test(version) && !pseudoVersion.test(version) ? version : "";
  const [provider, setProvider] = useState<Provider>("github");
  const [release, setRelease] = useState<string>();
  const [made, setMade] = useState<{ secret: string; hint: string }>();
  const [shown, setShown] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const tag = release ?? bundledTag;
  const workflow = deployWorkflow(provider, window.location.origin, service.id, imageOf(service), tag || "vX.Y.Z");

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
          proof,
        }),
      );
      setMade({ secret: result.secret, hint: result.token?.hint ?? "dwt_••••••••" });
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
          onChange={setProvider}
          options={[
            { value: "github", label: "GitHub Actions" },
            { value: "gitlab", label: "GitLab CI" },
          ]}
        />
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
          <pre>
            <code>{workflow}</code>
          </pre>
        </div>
      </div>
    </Sheet>
  );
}
