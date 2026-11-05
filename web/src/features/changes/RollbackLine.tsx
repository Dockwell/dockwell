// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  type FieldChange,
  FieldOp,
  type Plan,
  RollbackState,
  type RollbackStatus,
} from "@dockwell/api";
import { cx, Glyph, type GlyphShape, STATUS } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import { keepTrying } from "../../lib/retry";
import { useRunsNow } from "../apps/Health";
import { type AppHealth, runsAsFarAsAnyoneKnows, useHealth } from "../apps/health";
import { MachineWords } from "./FailureNotice";
import { followChange } from "./follow";
import { changing, failedStep, onlyCreates, runs, startsStopped, wentOffline } from "./plan";
import "./ChangeSheet.css";

const BACKOFF = { minMs: 500, maxMs: 8_000 };

/** True once a rollback won't move again. */
function settled(status: RollbackStatus | undefined): boolean {
  return status?.state === RollbackState.APPLIED || status?.state === RollbackState.FAILED;
}

/**
 * How a change's rollback stands, from the API. A rollback runs on after the
 * change it rolls back has failed, so the change as it was read is soon out of
 * date: this follows the rollback, and reads the change again each time it
 * moves on. What the API says of it is the only thing anything here goes by.
 */
export function useRollbackStatus(change: Change | undefined, onSettled?: () => void): RollbackStatus | undefined {
  const transport = useTransport();
  const id = change?.id;
  const given = change?.rollbackStatus;
  const [read, setRead] = useState<{ of: string; status: RollbackStatus }>();
  const fresh = read && read.of === id ? read.status : undefined;
  // What has settled stays settled, whichever read came last.
  const status = fresh && !settled(given) ? fresh : given;
  const rollback = status?.id;
  const moving = status !== undefined && !settled(status);

  useEffect(() => {
    if (!id || !rollback || !moving) return;
    const abort = new AbortController();
    const client = createClient(ChangeService, transport);
    const readAgain = () =>
      keepTrying(abort.signal, BACKOFF, async () => {
        const next = (await client.getChange({ id }, { signal: abort.signal })).change?.rollbackStatus;
        if (next) setRead({ of: id, status: next });
        return "done";
      });
    let seen = "";
    void followChange(transport, rollback, abort.signal, (next) => {
      const moved = `${next.state} ${next.steps.map((s) => s.state).join()}`;
      if (moved === seen) return;
      seen = moved;
      void readAgain();
    });
    return () => abort.abort();
  }, [transport, id, rollback, moving]);

  const told = useRef(onSettled);
  told.current = onSettled;
  const over = settled(status);
  useEffect(() => {
    if (over) told.current?.();
  }, [over]);

  return status;
}

/** What a rollback is about, for saying plainly what runs now. */
export interface RollbackSubject {
  /** The one resource it comes down to, when it does. */
  name?: string | undefined;
  /** Its ID, which how it runs goes by. */
  id?: string | undefined;
  /** The image that resource ran before, when the failed change was to move it. */
  previous?: string | undefined;
  /** The image the failed change was to run in its place. */
  next?: string | undefined;
  /** True when what it is about serves something, which is down while it is away. */
  serves: boolean;
  /** True for a first deploy: nothing ran before, so there is nothing to put back. */
  first: boolean;
  /** True when what it set out to do rolled, so what ran went on serving until the new one was healthy. */
  rolled: boolean;
}

/** The image a change moved a resource from and to, under the name a plan or a revision gives it. */
function imageChange(changes: readonly FieldChange[] | undefined): { previous?: string; next?: string } {
  const image = changes?.find((c) => c.op === FieldOp.MODIFIED && (c.path === "image" || c.path.endsWith(".image")));
  return image ? { previous: image.from, next: image.to } : {};
}

/**
 * What a rollback is about, from the plan of the change it rolls back. A brief
 * plan has no resources, so a page that knows which app it is about says so.
 */
export function rollbackSubject(
  plan: Plan | undefined,
  known?: { id?: string; name: string; changes: readonly FieldChange[] },
): RollbackSubject {
  const first = onlyCreates(plan);
  const steps = (plan?.servers ?? [])
    .flatMap((server) => server.steps)
    .filter((step) => step.resource?.kind === "service" || step.resource?.kind === "container");
  const rolled = steps.length > 0 && steps.every((step) => step.rollout?.rolling && !startsStopped(step));
  if (known) return { name: known.name, id: known.id, ...imageChange(known.changes), serves: true, first, rolled };
  const all = changing(plan);
  const serving = all.filter(runs);
  const only = serving.length === 1 ? serving[0] : all.length === 1 ? all[0] : undefined;
  return { name: only?.resource?.name, id: only?.id, ...imageChange(only?.changes), serves: serving.length > 0, first, rolled };
}

/** How a rollback that has ended looks: as what runs now, where that's been read, and otherwise as nothing at all. */
function lookOf(subject: RollbackSubject, health: AppHealth | undefined): Pick<Line, "tone" | "shape"> {
  if (subject.serves && health && runsAsFarAsAnyoneKnows(health)) {
    return { tone: STATUS[health.status].tone, shape: STATUS[health.status].shape };
  }
  return { tone: "idle", shape: "ring" };
}

/** How a rollback reads: a fact that says what runs now, then what follows from it. */
interface Line {
  tone: "healthy" | "cobalt" | "warning" | "critical" | "idle" | "ink";
  shape: GlyphShape;
  title: ReactNode;
  text?: ReactNode;
}

function useLine() {
  return useCallback(
    (
      status: RollbackStatus | undefined,
      subject: RollbackSubject,
      offline: boolean,
      record: boolean,
      /** How it runs now, where that's been read, which is all that can say it runs. */
      health: AppHealth | undefined,
      runsNow: string,
    ): Line | undefined => {
      const { name, previous, next, serves, first, rolled } = subject;
      const servers = status?.waitingFor.join(", ") ?? "";
      switch (status?.state) {
        case RollbackState.APPLIED: {
          // A first deploy that failed has left nothing, and had nothing to come back to.
          if (first) return undefined;
          if (name ? status.restored.includes(name) : status.steps > 0) {
            // "Runs" is what was so as this rollback finished, for whoever watched it. On
            // the record, days later, it is only what Dockwell did then.
            const title = record ? (
              !name ? (
                <Trans>Dockwell put things back as they were</Trans>
              ) : serves && previous ? (
                <Trans>
                  Dockwell put <code>{previous}</code> back on <code>{name}</code>
                </Trans>
              ) : (
                <Trans>
                  Dockwell put <code>{name}</code> back as it was
                </Trans>
              )
            ) : !name ? (
              serves ? (
                <Trans>What ran before runs again</Trans>
              ) : (
                <Trans>Everything is back as it was</Trans>
              )
            ) : !serves ? (
              <Trans>
                <code>{name}</code> is back as it was
              </Trans>
            ) : previous ? (
              <Trans>
                <code>{name}</code> runs <code>{previous}</code> again
              </Trans>
            ) : (
              <Trans>
                <code>{name}</code> runs as it did before
              </Trans>
            );
            const did = offline ? (
              <Trans>Dockwell put it back once it could reach the server.</Trans>
            ) : !serves || rolled ? (
              <Trans>Dockwell put it back after this failed.</Trans>
            ) : name ? (
              <Trans>Dockwell put it back after this failed, so it was down while that happened.</Trans>
            ) : (
              <Trans>Dockwell put it back after this failed, so some of it was down while that happened.</Trans>
            );
            return {
              ...lookOf(subject, health),
              title,
              text: runsNow ? (
                <>
                  {did} {runsNow}
                </>
              ) : (
                did
              ),
            };
          }
          // No step means its spec is as it was. That says nothing of whether it runs, which
          // nothing here has read: someone may have stopped it, or it may have crashed.
          const title = !name ? (
            record ? (
              <Trans>Everything stayed as it was</Trans>
            ) : (
              <Trans>Everything stays as it was</Trans>
            )
          ) : serves && previous ? (
            record ? (
              <Trans>
                <code>{name}</code> stayed on <code>{previous}</code>
              </Trans>
            ) : (
              <Trans>
                <code>{name}</code> stays on <code>{previous}</code>
              </Trans>
            )
          ) : record ? (
            <Trans>
              <code>{name}</code> stayed as it was
            </Trans>
          ) : (
            <Trans>
              <code>{name}</code> stays as it was
            </Trans>
          );
          const did =
            status.steps > 0 && status.restored.length === 0 ? (
              <Trans>It never stopped. Dockwell removed what this made after it failed.</Trans>
            ) : status.steps > 0 ? (
              <Trans>This stopped before it reached that, and Dockwell put back the rest.</Trans>
            ) : (
              <Trans>This stopped before it changed anything on the server.</Trans>
            );
          return {
            ...lookOf(subject, health),
            title,
            text: runsNow ? (
              <>
                {did} {runsNow}
              </>
            ) : (
              did
            ),
          };
        }
        case RollbackState.APPLYING:
          return {
            tone: "cobalt",
            shape: "spinner",
            title: first ? (
              <Trans>Dockwell is clearing this away</Trans>
            ) : name && serves && previous ? (
              <Trans>
                Dockwell is putting <code>{previous}</code> back
              </Trans>
            ) : name ? (
              <Trans>
                Dockwell is putting <code>{name}</code> back as it was
              </Trans>
            ) : (
              <Trans>Dockwell is putting things back as they were</Trans>
            ),
            text:
              first || !serves ? undefined : name ? (
                <Trans>
                  <code>{name}</code> may be down until that finishes.
                </Trans>
              ) : (
                <Trans>Some of this may be down until that finishes.</Trans>
              ),
          };
        case RollbackState.WAITING: {
          const after =
            (status.waitingFor.length ?? 0) > 1 ? (
              <Trans>
                Once <code>{servers}</code> reconnect, Dockwell puts things back as they were
              </Trans>
            ) : first ? (
              <Trans>
                Once <code>{servers}</code> reconnects, Dockwell clears this away
              </Trans>
            ) : name && serves && previous ? (
              <Trans>
                Once <code>{servers}</code> reconnects, Dockwell puts <code>{previous}</code> back
              </Trans>
            ) : name ? (
              <Trans>
                Once <code>{servers}</code> reconnects, Dockwell puts <code>{name}</code> back as it was
              </Trans>
            ) : (
              <Trans>
                Once <code>{servers}</code> reconnects, Dockwell puts things back as they were
              </Trans>
            );
          // A server lost mid-step leaves the one thing nobody can say. What comes next follows from that.
          if (offline) {
            return {
              tone: "warning",
              shape: "triangle",
              title: next ? (
                <Trans>
                  Dockwell can't tell whether <code>{next}</code> started
                </Trans>
              ) : (
                <Trans>Dockwell can't tell whether this ran</Trans>
              ),
              text: (
                <>
                  {after}. <Trans>Check that the server and its agent are up.</Trans>
                </>
              ),
            };
          }
          return {
            tone: "warning",
            shape: "triangle",
            title: after,
            text: (
              <Trans>It can't tell what is running there until then. Check that the server and its agent are up.</Trans>
            ),
          };
        }
        case RollbackState.FAILED:
          return {
            tone: "critical",
            shape: "square",
            title: first ? (
              <Trans>Clearing this away failed too</Trans>
            ) : name ? (
              <Trans>
                Putting <code>{name}</code> back failed too
              </Trans>
            ) : (
              <Trans>Putting things back failed too</Trans>
            ),
            text: first ? (
              <Trans>
                Parts of it may still be on the server. Check what is there, then try again, or remove them by hand.
              </Trans>
            ) : name && serves && health?.read === "read" ? (
              runsNow ? (
                <>
                  {runsNow} <Trans>Deploy what should run, or fix it on the server.</Trans>
                </>
              ) : (
                <Trans>
                  Something of <code>{name}</code> runs, and Dockwell can't say which version. Check it, then deploy
                  what should run, or fix it on the server.
                </Trans>
              )
            ) : name && serves ? (
              <Trans>
                <code>{name}</code> may not be running, or may be running the new version. Check it, then deploy what
                should run, or fix it on the server.
              </Trans>
            ) : (
              <Trans>
                The server may be left between the two. Check it, then apply what should be there, or fix it by hand.
              </Trans>
            ),
          };
        default:
          return undefined;
      }
    },
    [],
  );
}

/**
 * What a failed change's rollback did: whether what ran before runs now, which
 * is the first thing anyone wants to know, so it comes before why. It follows the
 * rollback while it runs, and it never says more than the API does.
 */
export function RollbackLine({
  change,
  subject,
  onSettled,
  record = true,
}: {
  change: Change;
  subject: RollbackSubject;
  /** Hears once the rollback is over, when what the pages read has moved on. */
  onSettled?: () => void;
  /** Read later, not as it happened: what it says of what runs may no longer be so. */
  record?: boolean;
}) {
  const { t } = useLingui();
  const line = useLine();
  const status = useRollbackStatus(change, onSettled);
  const offline = wentOffline(change);
  const view = useHealth();
  const health = subject.id ? view.of({ id: subject.id }) : undefined;
  const runsNow = useRunsNow(health, subject.name ?? "");
  // A rollback ended with what it put back in place. Only the server says that it runs, and it's what anyone reads this for.
  const runs = health !== undefined && runsAsFarAsAnyoneKnows(health);
  // Whoever saw it move watched it end, so what it says of what runs is so as they read it.
  const watched = useRef(false);
  if (status && !settled(status)) watched.current = true;
  // Made, and no word of how it went, it says only that. A first deploy that came back has nothing to say.
  const reads: Line | undefined =
    line(status, subject, offline, (record && !watched.current) || (subject.serves && !runs), health, runsNow) ??
    (change.rolledBackBy && status === undefined
      ? {
          tone: "idle",
          shape: "ring",
          title: <Trans>Dockwell made a rollback of this, and can't say yet how it went.</Trans>,
        }
      : undefined);
  if (!reads) return null;
  const failed = status?.state === RollbackState.FAILED;
  // A failure says why in its own notice, but a server that went away has only this to say it.
  const said = failed ? status?.detail : offline ? failedStep(change)?.detail : "";
  return (
    <div className={cx("plan-rollback", `dw-tone-${reads.tone}`)} role={failed ? "alert" : "status"}>
      <span className="plan-rollback-mark">
        <Glyph shape={reads.shape} size={10} />
      </span>
      <div className="plan-rollback-body">
        <div className="plan-rollback-title">{reads.title}</div>
        {failed && status?.error ? (
          <div className="plan-rollback-said" lang="en">
            <MachineWords>{status.error}</MachineWords>
          </div>
        ) : null}
        {reads.text ? <div className="plan-rollback-text">{reads.text}</div> : null}
        {said ? (
          <details className="plan-details">
            <summary>{t`Details`}</summary>
            <pre className="plan-details-text">{said}</pre>
          </details>
        ) : null}
      </div>
    </div>
  );
}

/** How a rollback stands, in a word or two, for a list. */
export function useRollbackWord() {
  const { t } = useLingui();
  return useCallback(
    (change: Change): string => {
      if (!change.rolledBackBy) return "";
      switch (change.rollbackStatus?.state) {
        case RollbackState.APPLIED:
          return t`Rolled back`;
        case RollbackState.APPLYING:
          return t`Rolling back`;
        case RollbackState.WAITING:
          return t`Rollback waits`;
        case RollbackState.FAILED:
          return t`Rollback failed`;
        default:
          return t`Rollback made`;
      }
    },
    [t],
  );
}
