// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, isValidElement, type ReactNode, useId, useState } from "react";

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
  | "variable"
  | "database"
  | "job"
  | "volume"
  | "network"
  | "backup"
  | "package"
  | "server"
  | "firewall"
  | "route"
  /** Anything else: a kind this package doesn't know yet, with its word in kindLabel. */
  | "resource";

/**
 * A value before or after the change. Secrets are never shown, only marked as
 * hidden. Text is a machine value, in mono; an element holds words too, with
 * its machine values in code, such as "web-data at /data".
 */
export type PlanValue = string | number | ReactNode | { secret: true };

export interface PlanChange {
  /** Names the row, and tells it from the others: the field that changes, such as "image" or "ports[0]". */
  path: string;
  /** What the field means, in words, such as "port". The path is shown when there is none. */
  label?: ReactNode;
  /** Absent for a new field. */
  from?: PlanValue;
  /** Absent for a removed field. */
  to?: PlanValue;
  /**
   * How the new value reads, when green would say the wrong thing: critical
   * for what is as good as root or loses data, warning for what reaches the
   * server and stops short of that, change for a field that changed and doesn't
   * say from what.
   */
  tone?: "critical" | "warning" | "change";
  /** Says the value is hidden, beside what names it, such as a variable's name. */
  hidden?: boolean;
}

export interface PlanResource {
  id: string;
  action: PlanAction;
  kind: PlanKind;
  /** Replaces the kind word after the name. */
  kindLabel?: string;
  name: string;
  /** Replaces the default action word on the right, such as "Deploy". */
  note?: string;
  changes?: readonly PlanChange[];
  /** Whether the changes start open. Destroyed resources start collapsed, everything else open. */
  expanded?: boolean;
  /** It changes only if a pull finds a newer image, so it counts apart, as what may come to nothing. */
  ifNewer?: boolean;
}

export interface PlanDiffProps {
  resources: readonly PlanResource[];
  /** Servers affected, rollout, expected downtime: "One replica at a time · no downtime". It takes a row of its own, under the counts. */
  impact?: ReactNode;
  /**
   * Says what it did, once it has: the counts and the actions in the past
   * tense, and no zeros, which are a promise only while they are to come.
   */
  done?: boolean;
  /** Leaves out the counts, for a plan that came to nothing. */
  noSummary?: boolean;
  /**
   * What comes along with the resources that matter, on one folded row, such as
   * "Also creates project blog, a network and 2 volumes". It counts like the rest.
   */
  also?: { label: ReactNode; action: PlanAction; resources: readonly PlanResource[] } | undefined;
  className?: string;
}

type DiffTone = "add" | "change" | "remove";

const ACTIONS: Readonly<Record<PlanAction, { symbol: string; tone: DiffTone }>> = {
  create: { symbol: "+", tone: "add" },
  update: { symbol: "~", tone: "change" },
  replace: { symbol: "±", tone: "change" },
  destroy: { symbol: "−", tone: "remove" },
};

/** The fold's own place among what the reader toggled, which no resource's ID can be. */
const ALSO = " also";

const KIND_ICON: Readonly<Record<PlanKind, IconName>> = {
  project: "folder",
  environment: "layers",
  app: "service",
  service: "service",
  container: "service",
  domain: "globe",
  secret: "key",
  variable: "settings",
  database: "database",
  job: "clock",
  volume: "archive",
  network: "network",
  backup: "archive",
  package: "layers",
  server: "server",
  firewall: "shield",
  route: "globe",
  resource: "layers",
};

/**
 * Shows exactly what a change will do before it runs: what is created,
 * changed, replaced or destroyed, and the impact. The summary counts
 * replacements apart from changes, so a plan that replaces a database never
 * reads as a plain change. Old values are struck through as well as
 * coloured, so the diff reads in grayscale.
 */
export function PlanDiff({ resources, impact, done = false, noSummary = false, also, className }: PlanDiffProps) {
  const strings = useUIStrings();
  const baseId = useId();
  // Only what the reader toggled is stored, so resources added later start the default way.
  const [toggled, setToggled] = useState<Readonly<Record<string, boolean>>>({});
  const counts: Record<PlanAction, number> = { create: 0, update: 0, replace: 0, destroy: 0 };
  let ifNewer = 0;
  for (const resource of [...resources, ...(also?.resources ?? [])]) {
    // Once it has happened it did or it didn't, and the counts say what it did.
    if (resource.ifNewer && !done) ifNewer += 1;
    else counts[resource.action] += 1;
  }
  // A replace removes what is there now, so it gets its own count rather than hiding among the changes.
  // Only what the plan does is counted: a zero says nothing, and takes a phone's line to say it.
  const words = done ? strings.planDiff.summaryDone : strings.planDiff.summary;
  const actionWords = done ? strings.planDiff.actionDone : strings.planDiff.action;
  const summary = [
    { key: "create", count: counts.create, tone: "add", symbol: ACTIONS.create.symbol },
    { key: "change", count: counts.update, tone: "change", symbol: ACTIONS.update.symbol },
    { key: "replace", count: counts.replace, tone: "change", symbol: ACTIONS.replace.symbol },
    { key: "destroy", count: counts.destroy, tone: "remove", symbol: ACTIONS.destroy.symbol },
    { key: "ifNewer", count: ifNewer, tone: "change", symbol: ACTIONS.update.symbol },
  ].filter((part) => part.count > 0);

  const row = (resource: PlanResource, at: string) => {
    const action = ACTIONS[resource.action];
    const changes = resource.changes ?? [];
    const open = toggled[resource.id] ?? resource.expanded ?? resource.action !== "destroy";
    const changesId = `${baseId}-${at}`;
    const head = (
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
        <span className="dw-plan-act">{resource.note ?? actionWords[resource.action]}</span>
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
            onClick={() => setToggled((was) => ({ ...was, [resource.id]: !open }))}
          >
            {head}
            <Icon name="chevron-down" size={14} className={cx("dw-plan-chev", open && "is-open")} />
          </button>
        ) : (
          <div className="dw-plan-row">
            {head}
            <span />
          </div>
        )}
        {changes.length > 0 ? (
          <dl id={changesId} className="dw-plan-changes" hidden={!open}>
            {changes.map((change) => (
              <Fragment key={change.path}>
                <dt
                  className={cx(
                    change.label ? "dw-plan-label" : "dw-plan-path",
                    change.tone === "critical" && "is-critical",
                    change.tone === "warning" && "is-warning",
                  )}
                >
                  {change.label ?? change.path}
                </dt>
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
                        className={change.tone ? `dw-diff-${change.tone}` : "dw-diff-to"}
                        hiddenLabel={strings.planDiff.hidden}
                      />
                    </>
                  ) : null}
                  {change.hidden ? <Hidden label={strings.planDiff.hidden} /> : null}
                </dd>
              </Fragment>
            ))}
          </dl>
        ) : null}
      </li>
    );
  };

  const alsoAction = also ? ACTIONS[also.action] : undefined;
  const alsoOpen = toggled[ALSO] ?? false;
  const alsoId = `${baseId}-also`;

  return (
    <div className={cx("dw-plan", className)}>
      {noSummary ? null : (
        <div className="dw-plan-summary">
          <div className="dw-plan-counts">
            {summary.map((part) => (
              <span key={part.key} className="dw-plan-count">
                <span className={`dw-plan-sym is-${part.tone}`} aria-hidden="true">
                  {part.symbol}
                </span>
                <b>{part.count}</b>{" "}
                {part.key === "ifNewer"
                  ? strings.planDiff.summary.ifNewer
                  : words[part.key as keyof typeof strings.planDiff.summaryDone]}
              </span>
            ))}
          </div>
          {hasContent(impact) ? (
            <div className="dw-plan-impact">
              <Icon name="activity" size={14} />
              <span>{impact}</span>
            </div>
          ) : null}
        </div>
      )}
      <ul className="dw-plan-list">
        {resources.map((resource, i) => row(resource, String(i)))}
        {also && alsoAction ? (
          <li className={cx("dw-plan-res", "dw-plan-also", `is-${alsoAction.tone}`)}>
            <button
              type="button"
              className="dw-plan-row"
              aria-expanded={alsoOpen}
              aria-controls={alsoId}
              onClick={() => setToggled((was) => ({ ...was, [ALSO]: !alsoOpen }))}
            >
              <span className={`dw-plan-sym is-${alsoAction.tone}`} aria-hidden="true">
                {alsoAction.symbol}
              </span>
              <Icon name="layers" size={16} className="dw-plan-kicon" />
              <span className="dw-plan-name">{also.label}</span> <span className="dw-plan-act" />
              <Icon name="chevron-down" size={14} className={cx("dw-plan-chev", alsoOpen && "is-open")} />
            </button>
            <ul id={alsoId} className="dw-plan-list dw-plan-also-list" hidden={!alsoOpen}>
              {also.resources.map((resource, i) => row(resource, `also-${i}`))}
            </ul>
          </li>
        ) : null}
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
  if (isSecret(value)) return <Hidden label={hiddenLabel} />;
  if (isValidElement(value)) return <span className={cx(className, "is-words")}>{value}</span>;
  return <span className={className}>{value as ReactNode}</span>;
}

function isSecret(value: PlanValue): value is { secret: true } {
  return typeof value === "object" && value !== null && "secret" in value;
}

function Hidden({ label }: { label: string }) {
  return (
    <span className="dw-diff-secret">
      <Icon name="lock" size={12} />
      {label}
    </span>
  );
}
