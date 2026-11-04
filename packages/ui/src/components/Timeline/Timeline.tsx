// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import type { Status } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import { StatusBadge } from "../StatusBadge/StatusBadge";
import "./Timeline.css";

export type TimelineKind = "deploy" | "config" | "alert" | "rollback" | "server" | "plan" | "backup";

export interface TimelineEvent {
  id: string | number;
  kind: TimelineKind;
  /** Replaces the kind's mark, for events the kinds don't name, such as a sign-in. */
  icon?: IconName;
  /** Past tense for finished work ("Deployed api-gateway"), present progressive for work in flight ("Deploying web"). */
  title: ReactNode;
  /** Only for an alert or an outcome worth noticing. Routine events stay neutral. */
  tone?: "healthy" | "warning" | "critical" | "cobalt";
  status?: Status;
  /** The word beside the status, where it says more than the status's own, such as "Live". */
  statusLabel?: string;
  revision?: string;
  actor?: string;
  meta?: ReactNode;
  /** As shown: "3 min ago", "12 Oct". */
  time: string;
  /** Machine-readable time for the time element. */
  datetime?: string;
  /** Offers Undo, which opens a plan for the reverse change. */
  undoable?: boolean;
  /** A specific verb instead of Undo, such as "Roll back". */
  undoLabel?: string;
}

export interface TimelineProps {
  events: readonly TimelineEvent[];
  onUndo?: (event: TimelineEvent) => void;
  className?: string;
}

const KIND_ICON: Readonly<Record<TimelineKind, IconName>> = {
  deploy: "deploy",
  config: "settings",
  alert: "alert",
  rollback: "undo",
  server: "server",
  plan: "commit",
  backup: "archive",
};

/** One line of time that correlates deploys, config changes, alerts, servers and backups, with undo where it is possible. */
export function Timeline({ events, onUndo, className }: TimelineProps) {
  const baseId = useId();
  return (
    <ol className={cx("dw-tl", className)}>
      {events.map((event, i) => (
        <TimelineItem key={event.id} event={event} titleId={`${baseId}-${i}`} onUndo={onUndo} />
      ))}
    </ol>
  );
}

function TimelineItem({
  event,
  titleId,
  onUndo,
}: {
  event: TimelineEvent;
  titleId: string;
  onUndo: ((event: TimelineEvent) => void) | undefined;
}) {
  const strings = useUIStrings();
  return (
    <li className="dw-tl-item">
      <span className={cx("dw-tl-mark", event.tone && `dw-tone-${event.tone}`)}>
        <Icon name={event.icon ?? KIND_ICON[event.kind]} size={14} />
      </span>
      <div className="dw-tl-main">
        <div className="dw-tl-title" id={titleId}>
          {event.title}
        </div>
        <div className="dw-tl-meta">
          {event.status ? (
            <StatusBadge
              status={event.status}
              {...(event.statusLabel ? { label: event.statusLabel, labelIsState: true } : {})}
            />
          ) : null}
          {event.revision ? <code className="dw-tl-rev">{event.revision}</code> : null}
          {event.actor ? <span>{event.actor}</span> : null}
          {hasContent(event.meta) ? <span>{event.meta}</span> : null}
        </div>
      </div>
      <div className="dw-tl-side">
        <time className="dw-tl-time" dateTime={event.datetime}>
          {event.time}
        </time>
        {event.undoable && onUndo ? (
          <Button size="sm" variant="ghost" icon="undo" aria-describedby={titleId} onClick={() => onUndo(event)}>
            {event.undoLabel ?? strings.undo}
          </Button>
        ) : null}
      </div>
    </li>
  );
}
