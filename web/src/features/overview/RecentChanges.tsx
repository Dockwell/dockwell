// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, ChangeState, type Event, EventKind } from "@dockwell/api";
import { cx, Loading, Panel, SkeletonRows } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo } from "react";

import { EventTimeline, foldFailures } from "../activity/ActivityPage";
import { type EventFilter, useEvents } from "../activity/useEvents";
import { useControlPlaneNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import "../apps/Apps.css";
import "./Overview.css";

/** How many changes the overview shows; Activity has the rest. */
const SHOWN = 4;

const CHANGES: readonly EventKind[] = [EventKind.CHANGE];

/** A plan nobody applied, which is no change: planned and waiting, or discarded (#802). */
const unapplied = (e: Event) => e.change?.state === ChangeState.PLANNED || e.change?.state === ChangeState.DISCARDED;

/**
 * The newest changes, live, as Activity's timeline draws them, each opening its plan.
 * kinds widens what counts, as the fleet's overview has apps' health too.
 */
export function RecentChanges({
  kinds = CHANGES,
  className,
  unboxed = false,
}: {
  kinds?: readonly EventKind[];
  className?: string;
  /** A heading and the timeline with no panel round them, as a phone's fleet overview has them. */
  unboxed?: boolean;
}) {
  const { t } = useLingui();
  const now = useControlPlaneNow(15_000);
  const filter = useMemo<EventFilter>(
    () => ({ kinds: [...kinds], serverId: "", resourceId: "", actorId: "", actorKind: ActorKind.UNSPECIFIED }),
    [kinds],
  );
  const { state } = useEvents(filter);
  const events = state.status === "connecting" ? undefined : state.events;
  // What reverted changes were called, so an undo can say what it undid.
  const summaries = useMemo(() => {
    const out = new Map<string, string>();
    for (const e of events ?? []) if (e.change) out.set(e.id, e.change.summary);
    return out;
  }, [events]);
  // What happened, newest first. Plans still waiting are one line under it, and discarded ones nothing.
  // A failure's rollback is one row with it, as on Activity.
  const { shown, folded } = useMemo(() => {
    const { shown: happened, folded } = foldFailures((events ?? []).filter((e) => !unapplied(e)));
    return { shown: happened.slice(0, SHOWN), folded };
  }, [events]);
  const waiting = useMemo(() => (events ?? []).filter((e) => e.change?.state === ChangeState.PLANNED), [events]);
  const plans = waiting.length;
  const onlyPlan = waiting.length === 1 ? waiting[0]?.change?.id : undefined;
  const pending =
    plans > 0 ? (
      <p className="overview-plans">
        <span>{plans === 1 ? t`1 plan not applied` : t`${plans} plans not applied`}</span>
        <span aria-hidden="true">·</span>
        {onlyPlan ? (
          <Link to="/changes/$changeId" params={{ changeId: onlyPlan }} className="overview-link">
            <Trans>Review</Trans>
          </Link>
        ) : (
          <Link to="/activity" search={{ kind: "changes" }} className="overview-link">
            <Trans>Review</Trans>
          </Link>
        )}
      </p>
    ) : null;

  const link = (
    <Link to="/activity" className="overview-link">
      <Trans>Activity</Trans>
    </Link>
  );
  const body =
    events === undefined ? (
      <Loading label={t`Loading changes`} className="overview-loading">
        <SkeletonRows rows={2} />
      </Loading>
    ) : shown.length === 0 ? (
      <>
        <EmptyState icon="activity" title={<Trans>Nothing has changed yet</Trans>}>
          <p>
            <Trans>Each change you make shows here.</Trans>
          </p>
        </EmptyState>
        {pending}
      </>
    ) : (
      <>
        <EventTimeline events={shown} summaries={summaries} now={now} framed={false} folded={folded} />
        {pending}
      </>
    );
  if (unboxed) {
    return (
      <section className={cx("overview-section", className)} aria-labelledby="overview-changes">
        <div className="overview-section-head">
          <h2 className="dw-text-heading" id="overview-changes">
            <Trans>Recent changes</Trans>
          </h2>
          {link}
        </div>
        {body}
      </section>
    );
  }
  return (
    <Panel className={className} title={t`Recent changes`} actions={link}>
      {body}
    </Panel>
  );
}
