// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Build, type BuildLine, BuildService, BuildState, ImageBuild_Builder, Role } from "@dockwell/api";
import { Button, Callout, cx, type Status, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { shownRef } from "../../lib/built";
import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { useSignedInUser } from "../auth/useAuthState";
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

/** A repository as people read it: github.com/acme/web, with no scheme or .git. */
export function shownRepository(url: string): string {
  return url.replace(/^https?:\/\//, "").replace(/\.git$/, "");
}

/** How a build stands, as a status and a word. */
export function useBuildLook() {
  const { t } = useLingui();
  return useCallback(
    (build: Build): { status: Status; word: string } => {
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
export function BuildPart({ build: given, onEnded }: { build: Build; onEnded?: () => void }) {
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
  // Stopping a build takes a developer (ADR 0049), so a viewer of the whole
  // install isn't offered it. What someone holds on one project, and whether
  // the environment is protected, aren't in the sign-in state: they see the
  // button, and the control plane's answer says what they need.
  const mayCancel = useSignedInUser()?.role !== Role.VIEWER;
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
        {live && mayCancel ? (
          <Button size="sm" variant="ghost" icon="x" loading={cancelling} onClick={() => void cancel()}>
            <Trans>Cancel build</Trans>
          </Button>
        ) : null}
      </div>
      {refusal ? (
        <Callout tone="critical" title={t`Couldn't cancel the build`}>
          {refusal}
        </Callout>
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

/**
 * A build's log: each line, with the step it came from, the failing step's lines
 * marked, and where the log left lines out. It follows the end while it runs and
 * whoever reads it hasn't scrolled up.
 */
export function BuildLog({
  lines,
  leftOut,
  live,
  queued = false,
  failedStep,
  error,
  label,
  dropped = 0n,
}: {
  lines: readonly BuildLine[];
  leftOut: bigint;
  live: boolean;
  queued?: boolean;
  failedStep: number;
  error?: string | undefined;
  label: string;
  /** How many lines its server dropped, as the build printed faster than its log could take them. */
  dropped?: bigint;
}) {
  const box = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  useLayoutEffect(() => {
    const el = box.current;
    if (el && following.current) el.scrollTop = el.scrollHeight;
  });
  const { t, i18n } = useLingui();
  if (error)
    return (
      <Callout tone="critical" title={t`Can't read the build's log`}>
        {error}
      </Callout>
    );
  let last = 0n;
  return (
    <div
      ref={box}
      className="build-log"
      role="log"
      aria-label={label}
      aria-live={live ? "polite" : "off"}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region has to take the keyboard
      tabIndex={0}
      onScroll={(event) => {
        const el = event.currentTarget;
        following.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
    >
      {lines.length === 0 ? (
        <p className="build-log-empty">
          {queued
            ? t`Waiting its turn: a server runs one build at a time.`
            : live
              ? t`Waiting for the build's first line`
              : t`The build printed nothing.`}
        </p>
      ) : null}
      {lines.map((line) => {
        const gap = last > 0n && line.number > last + 1n ? line.number - last - 1n : 0n;
        last = line.number;
        const failed = failedStep > 0 && line.step === failedStep;
        const count = i18n.number(Number(gap || leftOut));
        return (
          <div key={line.number.toString()}>
            {gap > 0n ? <div className="build-log-gap">{t`${count} lines left out`}</div> : null}
            <div className={cx("build-log-line", failed && "is-failed", line.text.startsWith("ERROR ") && "is-error")}>
              <span className="build-log-step" aria-hidden="true">
                {line.step > 0 ? `#${line.step}` : ""}
              </span>
              <span className="build-log-text">{line.text}</span>
            </div>
          </div>
        );
      })}
      {dropped > 0n ? (
        <div className="build-log-gap">
          {plural(Number(dropped), {
            one: "# line dropped, as the build printed faster than its log could take it",
            other: "# lines dropped, as the build printed faster than its log could take them",
          })}
        </div>
      ) : null}
      {live ? <div className="build-log-cursor" aria-hidden="true" /> : null}
    </div>
  );
}

/** When a build began, for the revision it made. */
export function buildStarted(build: Build | undefined): Date | undefined {
  return build?.startTime ? timestampDate(build.startTime) : undefined;
}
