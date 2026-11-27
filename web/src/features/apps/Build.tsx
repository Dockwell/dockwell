// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Build,
  type BuildLine,
  BuildService,
  BuildState,
  ImageBuild_Builder,
  type ServiceInfo,
} from "@dockwell/api";
import { Button, Callout, type Status, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useCallback, useEffect, useRef, useState } from "react";

import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { BuildLog } from "./BuildLog";
import { BuildMemoryNotice, isBuildMemoryFailure } from "./BuildMemoryNotice";
import { useDetectionWords } from "./detection";
import "./Apps.css";

/** A build that waits or runs, which its server goes on telling of. */
export function buildUnderWay(build: Build | undefined): boolean {
  return build?.state === BuildState.QUEUED || build?.state === BuildState.BUILDING;
}

/** A commit in the 7 digits people read one in. */
export function shortCommit(commit: string): string {
  return commit.slice(0, 7);
}

/** A repository as people read it: github.com/acme/web, with no scheme, SSH user, or .git. */
export function shownRepository(url: string): string {
  const trimmed = url.trim();
  const scp = /^[\w.-]+@([^:/]+):(.+)$/.exec(trimmed);
  if (scp) return `${scp[1]}/${scp[2]}`.replace(/\/+$/, "").replace(/\.git$/, "");
  return trimmed
    .replace(/^https?:\/\//, "")
    .replace(/^ssh:\/\/(?:[^/@]+@)?/, "")
    .replace(/\/+$/, "")
    .replace(/\.git$/, "");
}

/** How a build stands, as a status and a word. */
export function useBuildLook() {
  const { t } = useLingui();
  return useCallback(
    (build: Build): { status: Status; word: string } => {
      if (buildUnderWay(build) && build.memoryAvailableMib !== undefined) {
        return { status: "unknown", word: t`Waiting for memory to build` };
      }
      switch (build.state) {
        case BuildState.QUEUED:
          return { status: "unknown", word: t`Queued` };
        case BuildState.BUILDING:
          return { status: "deploying", word: t`Building` };
        case BuildState.DONE:
          return { status: "healthy", word: t`Built` };
        case BuildState.FAILED:
          return { status: "down", word: t`Failed` };
        case BuildState.CANCELLED:
          return { status: "stopped", word: t`Cancelled` };
        case BuildState.LOST:
          return { status: "unknown", word: t`Lost` };
        default:
          return { status: "unknown", word: t`Unknown` };
      }
    },
    [t],
  );
}

/** What the panel has of a build it follows or read. */
interface Followed {
  id: string;
  build?: Build;
  lines: BuildLine[];
  leftOut: bigint;
  error?: unknown;
}

/** The most lines the panel holds of a build: what the control plane keeps. */
const MAX_LINES = 10_000;

/**
 * A build as it stands and what its log keeps: followed while it waits or runs,
 * and read once when it has ended. onEnded hears when a build that was under way
 * ends, for what shows its revision to read again.
 */
export function useBuildFollow(build: Build | undefined, onEnded?: () => void) {
  const transport = useTransport();
  const [state, setState] = useState<Followed>({ id: "", lines: [], leftOut: 0n });
  const ended = useRef(onEnded);
  ended.current = onEnded;
  const id = build?.id ?? "";
  const live = buildUnderWay(build);
  useEffect(() => {
    if (id === "") return;
    const abort = new AbortController();
    const client = createClient(BuildService, transport);
    setState({ id, lines: [], leftOut: 0n });
    void (async () => {
      try {
        if (!live) {
          const res = await client.readBuildLog({ id }, { signal: abort.signal });
          setState((was) => ({ ...was, id, lines: res.lines, leftOut: res.leftOut }));
          return;
        }
        for await (const m of client.watchBuild({ id }, { signal: abort.signal })) {
          setState((was) => ({
            id,
            build: m.build ?? was.build,
            lines: m.lines.length === 0 ? was.lines : [...was.lines, ...m.lines].slice(-MAX_LINES),
            leftOut: m.leftOut > 0n ? m.leftOut : was.leftOut,
          }));
          if (m.build && !buildUnderWay(m.build)) ended.current?.();
        }
      } catch (err) {
        if (!abort.signal.aborted) setState((was) => ({ ...was, error: err }));
      }
    })();
    return () => abort.abort();
  }, [id, live, transport]);
  const current: Followed = state.id === id ? state : { id, lines: [], leftOut: 0n };
  return { build: current.build ?? build, lines: current.lines, leftOut: current.leftOut, error: current.error };
}

/** How long a build took, or has taken so far. */
function buildTook(build: Build, now: number): number | undefined {
  if (!build.startTime) return undefined;
  const end = build.endTime ? timestampMs(build.endTime) : now;
  return Math.max(end - timestampMs(build.startTime), 0);
}

/**
 * A revision's build: how it stands, how long it took, how much came from the
 * cache, where it ran and what it made, then its log, live while it runs
 * (App-Deployments). A build under way can be cancelled from here.
 */
export function BuildPart({
  build: given,
  service,
  onEnded,
}: {
  build: Build;
  service: ServiceInfo;
  onEnded?: () => void;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const look = useBuildLook();
  const detected = useDetectionWords();
  const transport = useTransport();
  const { build: shown, lines, leftOut, error } = useBuildFollow(given, onEnded);
  const build = shown ?? given;
  const [cancelling, setCancelling] = useState(false);
  const [refusal, setRefusal] = useState<string>();
  const [now, setNow] = useState(() => Date.now());
  const live = buildUnderWay(build);
  // Stopping a build is a developer's where its environment is open, and an admin's
  // where it is protected, as applying is: off, with why, for anyone else (ADR 0059).
  const mayCancel = useMay()(BuildService.method.cancelBuild, { environment: build.resource?.scope ?? "" });
  useEffect(() => {
    if (!live) return;
    const tick = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(tick);
  }, [live]);
  const took = buildTook(build, now);
  const hit = build.steps > 0 ? Math.round((build.cachedSteps / build.steps) * 100) : undefined;
  const cancel = async () => {
    setCancelling(true);
    setRefusal(undefined);
    try {
      await createClient(BuildService, transport).cancelBuild({ id: build.id });
    } catch (err) {
      setRefusal(sentence(err));
    } finally {
      setCancelling(false);
    }
  };
  const status = look(build);
  const step = build.step > 0 && build.steps > 0 ? `${build.step}/${build.steps}` : "";
  return (
    <div className="deploy-part build-part">
      <div className="build-head">
        <h3 className="dw-text-label">
          <Trans>Build</Trans>
        </h3>
        <StatusBadge status={status.status} label={status.word} labelIsState />
        {live && build.stepName ? (
          <span className="app-muted build-step">
            <Trans>
              Step {step}, <code>{build.stepName}</code>
            </Trans>
          </span>
        ) : null}
        <span className="build-spacer" />
        {live ? (
          <Button
            size="sm"
            variant="ghost"
            icon="x"
            loading={cancelling}
            onClick={() => void cancel()}
            {...mayCancel.off}
          >
            <Trans>Cancel build</Trans>
          </Button>
        ) : null}
      </div>
      {refusal ? (
        <Callout tone="critical" title={t`Couldn't cancel the build`}>
          {refusal}
        </Callout>
      ) : null}
      {isBuildMemoryFailure(build.errorReason) ? (
        <BuildMemoryNotice
          reason={build.errorReason}
          available={build.memoryAvailableMib}
          limit={build.memoryLimitMib}
          serverId={build.serverId}
          service={service}
        />
      ) : null}
      {build.state === BuildState.CANCELLED && build.cancelledBy ? (
        <p className="app-note">
          <Trans>Cancelled by {build.cancelledBy}. Nothing was deployed, and what ran before runs on.</Trans>
        </p>
      ) : null}
      {build.state === BuildState.LOST ? (
        <p className="app-note">
          <Trans>{build.serverName} went away during the build, so how it ended isn't known.</Trans>
        </p>
      ) : null}
      <dl className="build-facts">
        <div>
          <dt>{t`Duration`}</dt>
          <dd>{took === undefined ? "–" : time.bound(took)}</dd>
        </div>
        <div>
          <dt>{t`Cache hit`}</dt>
          <dd>{hit === undefined ? "–" : `${hit}%`}</dd>
        </div>
        <div>
          <dt>{t`Built on`}</dt>
          <dd>
            <code className="build-facts-code">{build.serverName || "–"}</code>
          </dd>
        </div>
        <div className="build-facts-wide">
          <dt>{t`Commit`}</dt>
          <dd>
            <code>{shortCommit(build.commit)}</code>
            {build.commitSubject ? <span className="build-subject"> {build.commitSubject}</span> : null}
            {build.commitAuthor ? <span className="app-muted"> · {build.commitAuthor}</span> : null}
          </dd>
        </div>
        {build.detection ? (
          <div className="build-facts-wide">
            <dt>{t`Built with`}</dt>
            <dd>
              {detected.headline(build.detection)}
              {build.detection.builder === ImageBuild_Builder.RAILPACK && !build.detection.error ? (
                <>
                  {/* A full stop between what it is and what it runs, which read as one otherwise. */}.{" "}
                  <span className="app-muted build-detected">{detected.steps(build.detection)}</span>
                </>
              ) : null}
            </dd>
          </div>
        ) : null}
        <div className="build-facts-wide">
          <dt>{t`Source`}</dt>
          <dd>
            {build.ref ? (
              <Trans>
                <code>{shownRepository(build.url)}</code> on <code>{shownRef(build.ref)}</code>
              </Trans>
            ) : (
              <code>{shownRepository(build.url)}</code>
            )}
          </dd>
        </div>
      </dl>
      <BuildLog
        lines={lines}
        leftOut={leftOut}
        dropped={build.logLinesDropped}
        live={live}
        queued={build.state === BuildState.QUEUED}
        failedStep={build.state === BuildState.FAILED ? build.step : 0}
        error={error ? sentence(error) : undefined}
        label={t`Build log`}
      />
    </div>
  );
}

/** When a build began, for the revision it made. */
export function buildStarted(build: Build | undefined): Date | undefined {
  return build?.startTime ? timestampDate(build.startTime) : undefined;
}
