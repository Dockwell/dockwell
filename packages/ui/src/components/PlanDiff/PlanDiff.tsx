// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, type ReactNode, useId, useState } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./PlanDiff.css";

export type PlanAction = "create" | "update" | "replace" | "destroy";

export type PlanKind =
  | "project"
  | "environment"
  | "app"
  | "service"
  | "container"
  | "domain"
  | "secret"
  | "database"
  | "job"
  | "volume"
  | "backup"
  | "package"
  | "server"
  | "firewall";

/** A value before or after the change. Secrets are never shown, only marked as hidden. */
export type PlanValue = string | number | { secret: true };

export interface PlanChange {
  /** The field that changes, such as "image" or "env.DATABASE_URL". */
  path: string;
  /** Absent for a new field. */
  from?: PlanValue;
  /** Absent for a removed field. */
  to?: PlanValue;
}

export interface PlanResource {
  id: string;
  action: PlanAction;
  kind: PlanKind;
  /** Replaces the kind word after the name. */
  kindLabel?: string;
  name: string;
  /** Replaces the default action word on the right, such as "Rolling update". */
  note?: string;
  changes?: readonly PlanChange[];
  /** Whether the changes start open. Destroyed resources start collapsed, everything else open. */
  expanded?: boolean;
}

export interface PlanDiffProps {
  resources: readonly PlanResource[];
  /** Servers affected, rollout, expected downtime: "One replica at a time · no downtime". */
  impact?: ReactNode;
  className?: string;
}

type DiffTone = "add" | "change" | "remove";

const ACTIONS: Readonly<Record<PlanAction, { symbol: string; tone: DiffTone }>> = {
  create: { symbol: "+", tone: "add" },
  update: { symbol: "~", tone: "change" },
  replace: { symbol: "±", tone: "change" },
  destroy: { symbol: "−", tone: "remove" },
};

const KIND_ICON: Readonly<Record<PlanKind, IconName>> = {
  project: "folder",
  environment: "layers",
  app: "service",
  service: "service",
  container: "service",
  domain: "globe",
  secret: "key",
  database: "database",
  job: "clock",
  volume: "archive",
  backup: "archive",
  package: "layers",
  server: "server",
  firewall: "shield",
};

/**
 * Shows exactly what a change will do before it runs: what is created,
 * changed, replaced or destroyed, and the impact. The summary counts
 * replacements apart from changes, so a plan that replaces a database never
 * reads as a plain change. Old values are struck through as well as
 * coloured, so the diff reads in grayscale.
 */
export function PlanDiff({ resources, impact, className }: PlanDiffProps) {
  const strings = useUIStrings();
  const baseId = useId();
  // Only what the reader toggled is stored, so resources added later start the default way.
  const [toggled, setToggled] = useState<Readonly<Record<string, boolean>>>({});
  const counts: Record<PlanAction, number> = { create: 0, update: 0, replace: 0, destroy: 0 };
  for (const resource of resources) counts[resource.action] += 1;
  // A replace removes what is there now, so it gets its own count rather than hiding among the changes.
  // It shows only when the plan has one; the other three always show, so a zero reads as a promise.
  const summary = [
    { key: "create", count: counts.create, tone: "add", symbol: ACTIONS.create.symbol },
    { key: "change", count: counts.update, tone: "change", symbol: ACTIONS.update.symbol },
    ...(counts.replace > 0
      ? [{ key: "replace", count: counts.replace, tone: "change", symbol: ACTIONS.replace.symbol } as const]
      : []),
    { key: "destroy", count: counts.destroy, tone: "remove", symbol: ACTIONS.destroy.symbol },
  ] as const;

  return (
    <div className={cx("dw-plan", className)}>
      <div className="dw-plan-summary">
        {summary.map((part) => (
          <span key={part.key} className={cx("dw-plan-count", part.count === 0 && "is-zero")}>
            <span className={`dw-plan-sym is-${part.tone}`} aria-hidden="true">
              {part.symbol}
            </span>
            <b>{part.count}</b> {strings.planDiff.summary[part.key]}
          </span>
        ))}
        {hasContent(impact) ? (
          <span className="dw-plan-impact">
            <Icon name="activity" size={14} />
            {impact}
          </span>
        ) : null}
      </div>
      <ul className="dw-plan-list">
        {resources.map((resource, i) => {
          const action = ACTIONS[resource.action];
          const changes = resource.changes ?? [];
          const open = toggled[resource.id] ?? resource.expanded ?? resource.action !== "destroy";
          const changesId = `${baseId}-${i}`;
          const row = (
            <>
              <span className={`dw-plan-sym is-${action.tone}`} aria-hidden="true">
                {action.symbol}
              </span>
              <Icon name={KIND_ICON[resource.kind]} size={16} className="dw-plan-kicon" />
              {/* The spaces keep the parts apart when read as one name; grid and flex layout ignore them. */}
              <span className="dw-plan-name">
                <code>{resource.name}</code>{" "}
                <span className="dw-plan-kind">{resource.kindLabel ?? strings.planDiff.kind[resource.kind]}</span>
              </span>{" "}
              <span className="dw-plan-act">{resource.note ?? strings.planDiff.action[resource.action]}</span>
            </>
          );
          return (
            <li key={resource.id} className={cx("dw-plan-res", `is-${action.tone}`)}>
              {changes.length > 0 ? (
                <button
                  type="button"
                  className="dw-plan-row"
                  aria-expanded={open}
                  aria-controls={changesId}
                  onClick={() => setToggled({ ...toggled, [resource.id]: !open })}
                >
                  {row}
                  <Icon name="chevron-down" size={14} className={cx("dw-plan-chev", open && "is-open")} />
                </button>
              ) : (
                <div className="dw-plan-row">
                  {row}
                  <span />
                </div>
              )}
              {changes.length > 0 ? (
                <dl id={changesId} className="dw-plan-changes" hidden={!open}>
                  {changes.map((change) => (
                    <Fragment key={change.path}>
                      <dt className="dw-plan-path">{change.path}</dt>
                      <dd className="dw-plan-val">
                        {change.from !== undefined ? (
                          <>
                            <span className="dw-sr">{strings.planDiff.from} </span>
                            <PlanValueView
                              value={change.from}
                              className="dw-diff-from"
                              hiddenLabel={strings.planDiff.hidden}
                            />
                          </>
                        ) : null}
                        {change.from !== undefined && change.to !== undefined ? (
                          <Icon name="arrow-right" size={12} className="dw-plan-arrow" />
                        ) : null}
                        {change.to !== undefined ? (
                          <>
                            <span className="dw-sr">{strings.planDiff.to} </span>
                            <PlanValueView
                              value={change.to}
                              className="dw-diff-to"
                              hiddenLabel={strings.planDiff.hidden}
                            />
                          </>
                        ) : null}
                      </dd>
                    </Fragment>
                  ))}
                </dl>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function PlanValueView({
  value,
  className,
  hiddenLabel,
}: {
  value: PlanValue;
  className: string;
  hiddenLabel: string;
}) {
  if (typeof value === "object") {
    return (
      <span className="dw-diff-secret">
        <Icon name="lock" size={12} />
        {hiddenLabel}
      </span>
    );
  }
  return <span className={className}>{String(value)}</span>;
}
