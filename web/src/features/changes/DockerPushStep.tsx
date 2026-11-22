// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type DockerImageAction, DockerImageOutcome, DockerService, type StepProgress, StepState } from "@dockwell/api";
import { Button, Callout, Loading, Skeleton, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { splitTag } from "../apps/ImageField";
import { useMay } from "../auth/may";
import { DockerPushSheet } from "../docker/DockerPushSheet";
import { shortImageId } from "../docker/imageIdentity";
import { CopyButton } from "../servers/CopyButton";
import { useChangeSheet } from "./ChangeSheet";

export function DockerPushStep({
  action,
  server,
  live,
}: {
  action: DockerImageAction;
  server: string;
  live?: StepProgress | undefined;
}) {
  const { t } = useLingui();
  const sheets = useChangeSheet();
  const client = createClient(DockerService, useTransport());
  const mayCheck = useMay()(DockerService.method.planDockerMaintenance).allowed;
  const [checking, setChecking] = useState(false);
  const [current, setCurrent] = useState<string>();
  const [failure, setFailure] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const request = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => request.current?.abort(), []);

  const push = action.push;
  const key = push?.sourceKey;
  const reference = push?.destination ?? "";
  const login = action.approvedLogins[0];
  const digest = action.descriptor?.manifestDigest ?? "";
  const outcome = live?.dockerImageReceipts[0]?.outcome;
  const unknown = outcome === DockerImageOutcome.UNKNOWN;
  const finished = outcome === DockerImageOutcome.APPLIED || outcome === DockerImageOutcome.UNCHANGED;
  const [name] = reference.split("@");
  const { repository, tag } = splitTag(name ?? "");
  const repositoryPath =
    login?.host && repository.startsWith(`${login.host}/`) ? repository.slice(login.host.length + 1) : repository;

  async function checkResult() {
    if (!push || !key || checking) return;
    const active = new AbortController();
    request.current = active;
    setChecking(true);
    setCurrent(undefined);
    setFailure("");
    try {
      const result = await client.planDockerMaintenance(
        {
          serverIds: [key.serverId],
          operation: {
            case: "push",
            value: { destination: reference, loginId: push.loginId, credentialProject: push.credentialProject },
          },
        },
        { signal: active.signal },
      );
      if (!active.signal.aborted) setCurrent(result.digestCheck?.current?.manifestDigest ?? "");
    } catch (error) {
      if (!active.signal.aborted) setFailure(sentence(error));
    } finally {
      if (!active.signal.aborted) setChecking(false);
    }
  }

  return (
    <li className="plan-step plan-step-image plan-step-push">
      <span className="plan-step-what">
        {finished ? (
          <StatusBadge
            status="healthy"
            label={outcome === DockerImageOutcome.UNCHANGED ? t`Already published` : t`Verified`}
            labelIsState
          />
        ) : null}
        <span className="plan-step-line">
          <Trans>
            Publish from <code>{server}</code>
          </Trans>
          {key ? (
            <>
              {" · "}
              <code title={key.imageId}>{shortImageId(key.imageId)}</code>
              <CopyButton text={key.imageId} label={t`Copy full image ID`} />
            </>
          ) : null}
        </span>
      </span>
      <dl className="plan-image-evidence plan-push-destination">
        <div>
          <dt>
            <Trans>Registry</Trans>
          </dt>
          <dd>
            <code>{login?.host}</code>
          </dd>
        </div>
        <div>
          <dt>
            <Trans>Registry login</Trans>
          </dt>
          <dd>{login?.name}</dd>
        </div>
        <div>
          <dt>
            <Trans>Repository</Trans>
          </dt>
          <dd>
            <code>{repositoryPath}</code>
          </dd>
        </div>
        <div>
          <dt>
            <Trans>Platform</Trans>
          </dt>
          <dd>
            <Tag>{action.descriptor?.platform}</Tag>
          </dd>
        </div>
        {push?.digestOnly ? (
          <div>
            <dt>
              <Trans>Destination</Trans>
            </dt>
            <dd>
              <Tag>
                <Trans>Publish by digest</Trans>
              </Tag>
            </dd>
          </div>
        ) : tag ? (
          <div>
            <dt>
              <Trans>Tag</Trans>
            </dt>
            <dd>
              <Tag>{tag}</Tag>
            </dd>
          </div>
        ) : null}
        <div className="plan-push-digest">
          <dt>
            <Trans>Manifest digest</Trans>
          </dt>
          <dd>
            <code>{digest}</code>
          </dd>
        </div>
      </dl>
      {live?.state === StepState.RUNNING ? (
        <Loading
          label={t`Publishing image`}
          slow={live.activity === "Waiting for a transfer slot" ? t`Waiting for a transfer slot` : undefined}
        >
          <Skeleton width="70%" inline />
        </Loading>
      ) : null}
      {!live ? (
        <span>
          <Trans>The registry checks write access when the image is published.</Trans>
        </span>
      ) : null}
      {unknown ? (
        <Callout title={t`Registry confirmation missing`} tone="warning">
          <Trans>Check what the destination serves before reviewing another push.</Trans>
        </Callout>
      ) : null}
      {unknown && mayCheck ? (
        <>
          <div className="plan-push-actions">
            <Button
              variant="secondary"
              size="sm"
              icon="refresh"
              loading={checking}
              disabled={checking}
              onClick={() => void checkResult()}
            >
              <Trans>Check result</Trans>
            </Button>
            <Button variant="ghost" size="sm" disabled={checking} onClick={() => setReviewing(true)}>
              <Trans>Review again</Trans>
            </Button>
          </div>
          {checking ? (
            <Loading label={t`Checking the registry`}>
              <Skeleton width="70%" inline />
            </Loading>
          ) : failure ? (
            <Callout title={t`Registry couldn't be checked`} tone="warning">
              {failure}
            </Callout>
          ) : current !== undefined ? (
            <div role="status">
              {current === digest ? (
                <StatusBadge status="healthy" label={t`Registry serves the reviewed digest`} labelIsState />
              ) : current ? (
                <>
                  <Trans>Registry serves</Trans> <code>{current}</code>
                </>
              ) : (
                <Trans>The destination is still empty.</Trans>
              )}
            </div>
          ) : null}
        </>
      ) : live?.state === StepState.FAILED && mayCheck ? (
        <Button variant="ghost" size="sm" onClick={() => setReviewing(true)}>
          {live.reason === "docker_push_conflict" ? <Trans>Choose another tag</Trans> : <Trans>Review again</Trans>}
        </Button>
      ) : null}
      {reviewing && push && key ? (
        <DockerPushSheet
          image={{ serverId: key.serverId, imageId: key.imageId, alias: reference }}
          defaults={push}
          onClose={() => setReviewing(false)}
          onReview={(change) => {
            setReviewing(false);
            sheets.open({ kind: "plan", make: async () => ({ change }) });
          }}
        />
      ) : null}
    </li>
  );
}
