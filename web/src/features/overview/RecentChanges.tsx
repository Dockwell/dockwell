// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, EventKind } from "@dockwell/api";
import { Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useMemo } from "react";

import { EventTimeline } from "../activity/ActivityPage";
import { type EventFilter, useEvents } from "../activity/useEvents";
import { useControlPlaneNow } from "../servers/useFleet";
import "../apps/Apps.css";
import "./Overview.css";

/** How many changes the overview shows; Activity has the rest. */
const SHOWN = 4;

const CHANGES: EventFilter = {
  kinds: [EventKind.CHANGE],
  serverId: "",
  resourceId: "",
  actorId: "",
  actorKind: ActorKind.UNSPECIFIED,
};

/** The newest changes, live, as Activity's timeline draws them, each opening its plan. */
export function RecentChanges() {
  const { t } = useLingui();
  const now = useControlPlaneNow(15_000);
  const { state } = useEvents(CHANGES);
  const events = state.status === "connecting" ? undefined : state.events;
  // What reverted changes were called, so an undo can say what it undid.
  const summaries = useMemo(() => {
    const out = new Map<string, string>();
    for (const e of events ?? []) if (e.change) out.set(e.id, e.change.summary);
    return out;
  }, [events]);
  const shown = useMemo(() => (events ?? []).slice(0, SHOWN), [events]);

  return (
    <Panel
      title={t`Recent changes`}
      actions={
        <Link to="/activity" className="overview-link">
          <Trans>Activity</Trans>
        </Link>
      }
    >
      {events === undefined ? (
        <div aria-busy="true" className="overview-loading">
          <span className="dw-sr">{t`Loading changes`}</span>
          {["72%", "56%"].map((width) => (
            <div key={width} className="apps-skeleton-line" aria-hidden="true">
              <span className="apps-skeleton-bar" style={{ width }} />
              <span className="apps-skeleton-bar is-short" />
            </div>
          ))}
        </div>
      ) : shown.length === 0 ? (
        <p className="overview-quiet">
          <Trans>Nothing has changed yet. Each change you make shows here.</Trans>
        </p>
      ) : (
        <EventTimeline events={shown} summaries={summaries} now={now} framed={false} />
      )}
    </Panel>
  );
}
