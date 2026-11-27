// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Build,
  type BuildLine,
  BuildState,
  type DockerImageAction,
  DockerImageOutcome,
  DockerService,
  type StepProgress,
  StepState,
} from "@dockwell/api";
import { Button, Callout, Loading, SkeletonFacts, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { BuildLog } from "../apps/BuildLog";
import { deployKeyNameOf, gitlabConnectionNameOf } from "../apps/gitHost";
import { useMay } from "../auth/may";
import { shortImageId } from "../docker/imageIdentity";
import { CopyButton } from "../servers/CopyButton";

export function DockerBuildStep({
  action,
  changeId,
  serverId,
  live,
}: {
  action: DockerImageAction;
  changeId: string;
  serverId: string;
  live: StepProgress | undefined;
}) {
  const { t } = useLingui();
  const may = useMay();
  const mayWatch = may(DockerService.method.watchDockerImageBuild).allowed;
  const mayCancel = may(DockerService.method.cancelDockerImageBuild).allowed;
  const transport = useTransport();
  const client = useMemo(() => createClient(DockerService, transport), [transport]);
  const [open, setOpen] = useState(false);
  const [build, setBuild] = useState<Build>();
  const [lines, setLines] = useState<BuildLine[]>([]);
  const [retry, setRetry] = useState(0);
  const [failure, setFailure] = useState("");
  const [cancelFailure, setCancelFailure] = useState("");
  const [cancelling, setCancelling] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const cursor = useRef(0n);
  const b = action.build;
  const receipt = live?.dockerImageReceipts[0];
  const built = receipt?.imageKey?.imageId;
  const state = build?.state;
  const active = state === BuildState.QUEUED || state === BuildState.BUILDING;
  const stepRunning = live?.state === StepState.RUNNING;
  const stepEnded = live?.state === StepState.DONE || live?.state === StepState.FAILED;
  const underway = !stepEnded && (active || (stepRunning && state === undefined));
  const canStop = mayCancel && underway && !publishing;
  const logLabel = t`Build log`;
  const logOpen = mayWatch && open && !!changeId;

  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry reopens the stream at its saved cursor.
  useEffect(() => {
    if (!logOpen) return;
    const abort = new AbortController();
    void (async () => {
      try {
        setFailure("");
        for await (const item of client.watchDockerImageBuild(
          { serverId, changeId, afterLine: cursor.current },
          { signal: abort.signal },
        )) {
          if (abort.signal.aborted) return;
          if (item.build) setBuild(item.build);
          const seen = cursor.current;
          const added = item.lines.filter((line) => line.number > seen);
          const last = added.at(-1);
          if (last) {
            cursor.current = last.number;
            setLines((old) => {
              const all = [...old, ...added];
              return all.length > 10_000 ? [...all.slice(0, 1_000), ...all.slice(-9_000)] : all;
            });
          }
        }
      } catch (err) {
        if (!abort.signal.aborted) setFailure(sentence(err));
      }
    })();
    return () => abort.abort();
  }, [client, changeId, serverId, logOpen, retry]);

  const cancel = async () => {
    setCancelling(true);
    setCancelFailure("");
    try {
      await client.cancelDockerImageBuild({ serverId, changeId });
    } catch (err) {
      setCancelling(false);
      if (reasonOf(err) === "build_publishing") setPublishing(true);
      else setCancelFailure(sentence(err));
    }
  };

  const bases = [
    ...new Map(action.baseDescriptors.map((base) => [`${base.repository}@${base.manifestDigest}`, base])).keys(),
  ];
  const source = b?.git?.repository ?? (b?.folderPath || t`Uploaded context`);
  const badge =
    state === BuildState.CANCELLED || receipt?.outcome === DockerImageOutcome.CANCELLED
      ? { status: "stopped" as const, label: t`Cancelled` }
      : state === BuildState.LOST || receipt?.outcome === DockerImageOutcome.UNKNOWN
        ? { status: "unknown" as const, label: t`Result unknown` }
        : state === BuildState.FAILED || live?.state === StepState.FAILED
          ? { status: "down" as const, label: t`Build failed` }
          : state === BuildState.DONE || built
            ? { status: "healthy" as const, label: t`Built` }
            : state === BuildState.QUEUED
              ? { status: "deploying" as const, label: t`Queued` }
              : underway
                ? { status: "deploying" as const, label: t`Building` }
                : undefined;

  return (
    <li className="plan-step plan-archive-step">
      <div className="plan-archive-identity">
        <strong>
          <Trans>Build image</Trans>
        </strong>
        {cancelling && underway ? (
          <StatusBadge status="deploying" label={t`Cancelling`} labelIsState />
        ) : publishing && underway ? (
          <StatusBadge status="deploying" label={t`Adding image to Docker`} labelIsState />
        ) : badge ? (
          <StatusBadge {...badge} labelIsState />
        ) : null}
      </div>
      <dl className="plan-build-inputs">
        <dt>
          <Trans>Source</Trans>
        </dt>
        <dd>
          <code>{source}</code>
          {action.gitCommit ? (
            <>
              <code title={action.gitCommit}>{action.gitCommit.slice(0, 12)}</code>
              <CopyButton label={t`Copy commit`} text={action.gitCommit} />
            </>
          ) : null}
          {b?.git?.gitLogin ? (
            <Tag>
              {(() => {
                const key = deployKeyNameOf(b.git.gitLogin);
                if (key) return t`with deploy key ${key}`;
                const gl = gitlabConnectionNameOf(b.git.gitLogin);
                if (gl) return t`through GitLab connection ${gl}`;
                return t`through GitHub App ${b.git.gitLogin}`;
              })()}
            </Tag>
          ) : null}
        </dd>
        <dt>
          <Trans>Dockerfile</Trans>
        </dt>
        <dd>
          <code>{b?.dockerfilePath}</code>
          {b?.dockerfileOverrideArtifactId ? <Tag>{t`Override`}</Tag> : null}
        </dd>
        <dt>
          <Trans>Output</Trans>
        </dt>
        <dd>
          <code>{b?.output}</code>
          {b?.platform ? <Tag>{b.platform}</Tag> : null}
        </dd>
        {action.approvedLogins.length ? (
          <>
            <dt>
              <Trans>Registry logins</Trans>
            </dt>
            <dd>
              {action.approvedLogins.map((login) => (
                <Tag key={login.id}>
                  {login.name} · {login.host}
                </Tag>
              ))}
            </dd>
          </>
        ) : null}
        {built ? (
          <>
            <dt>
              <Trans>Image ID</Trans>
            </dt>
            <dd>
              <code title={built}>{shortImageId(built)}</code>
              <CopyButton label={t`Copy full image ID`} text={built} />
            </dd>
          </>
        ) : null}
      </dl>
      {bases.length ? (
        <details className="plan-build-bases">
          <summary>
            <Trans>Pinned base images</Trans>
          </summary>
          <ul>
            {bases.map((base) => (
              <li key={base}>
                <code>{base}</code>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {live && changeId ? (
        <div className="plan-archive-download">
          {mayWatch ? (
            <Button
              variant="secondary"
              size="sm"
              icon="terminal"
              aria-expanded={logOpen}
              onClick={() => setOpen(!open)}
            >
              {logLabel}
            </Button>
          ) : null}
          {canStop ? (
            <Button variant="ghost" size="sm" icon="x" disabled={cancelling} onClick={() => void cancel()}>
              <Trans>Cancel build</Trans>
            </Button>
          ) : null}
        </div>
      ) : null}
      {cancelFailure ? (
        <Callout tone="warning" title={t`Build couldn't be cancelled`}>
          {cancelFailure}
        </Callout>
      ) : null}
      {publishing && underway ? (
        <Callout tone="info" title={t`Docker is already adding the image`}>
          <Trans>The result will appear here when it finishes.</Trans>
        </Callout>
      ) : null}
      {logOpen ? (
        failure ? (
          <>
            <Callout tone="critical" title={t`Can't read the build's log`}>
              {failure}
            </Callout>
            <Button variant="ghost" size="sm" icon="refresh" onClick={() => setRetry(retry + 1)}>
              <Trans>Retry</Trans>
            </Button>
          </>
        ) : (!build || active) && lines.length === 0 ? (
          <Loading label={t`Reading the build log`}>
            <SkeletonFacts layout="rows" count={3} />
          </Loading>
        ) : (
          <BuildLog
            lines={lines}
            leftOut={build?.logLinesLeftOut ?? 0n}
            live={active}
            failedStep={state === BuildState.FAILED ? (build?.step ?? 0) : 0}
            focusFailure={state === BuildState.FAILED}
            label={logLabel}
            dropped={build?.logLinesDropped ?? 0n}
          />
        )
      ) : null}
    </li>
  );
}
