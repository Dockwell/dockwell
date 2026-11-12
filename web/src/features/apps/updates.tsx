// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type AvailableUpdate,
  CatalogService,
  type ServiceInfo,
  ServiceService,
  type ServiceUpdate,
  ServiceUpdateService,
  UpdateCheck,
  UpdateKind,
  UpdateProblem,
  UpdateSource,
} from "@dockwell/api";
import { Button, cx, Icon, Panel, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { installIn, useInstalls } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAgoNow } from "../servers/useFleet";
import { environmentOf } from "./app";
import { DeploySheet } from "./DeploySheet";
import { readServicesAgain } from "./useServices";
import "./Updates.css";

/** What an update offers first: the first newer version a person hasn't skipped (ADR 0079). */
export function offered(update: ServiceUpdate | undefined): AvailableUpdate | undefined {
  return update?.available.find((a) => !a.skipped);
}

/** Until when a person set an app's updates aside, while that holds. */
export function snoozedUntil(update: ServiceUpdate | undefined, now: number): Date | undefined {
  const until = update?.snoozeTime ? timestampDate(update.snoozeTime) : undefined;
  return until && until.getTime() > now ? until : undefined;
}

/** Whether an app's update counts: something newer that isn't skipped, while it isn't snoozed. */
export function counts(update: ServiceUpdate | undefined, now: number): boolean {
  return update?.check === UpdateCheck.AVAILABLE && offered(update) !== undefined && !snoozedUntil(update, now);
}

/** The apps whose updates count. */
export function withUpdates(services: readonly ServiceInfo[], now: number): ServiceInfo[] {
  return services.filter((s) => counts(s.update, now));
}

/** The words for a kind of update, as the API says them. */
export function useKindWord(): (kind: UpdateKind) => string {
  const { t } = useLingui();
  return (kind) => {
    switch (kind) {
      case UpdateKind.PATCH:
        return t({ message: "patch", context: "update kind" });
      case UpdateKind.MINOR:
        return t({ message: "minor", context: "update kind" });
      case UpdateKind.MAJOR:
        return t({ message: "major", context: "update kind" });
      case UpdateKind.DIGEST:
        return t({ message: "new build", context: "update kind" });
      case UpdateKind.RELEASE:
        return t({ message: "catalog release", context: "update kind" });
      case UpdateKind.COMMIT:
        return t({ message: "new commits", context: "update kind" });
      default:
        return "";
    }
  };
}

/** The step an update takes, as a person reads it: 16.3 → 16.4, or new commits. */
function stepOf(a: AvailableUpdate): string {
  if (a.kind === UpdateKind.DIGEST) return a.toVersion;
  return `${a.fromVersion} → ${a.toVersion}`;
}

/**
 * On a row of Apps: a quiet mark that an update is out, with its step in the
 * tooltip. Nothing while there's none that counts.
 */
export function UpdateMark({ service, now, className }: { service: ServiceInfo; now: number; className?: string }) {
  const { t } = useLingui();
  const kindWord = useKindWord();
  const a = offered(service.update);
  if (!a || !counts(service.update, now)) return null;
  const label = a.kind === UpdateKind.DIGEST ? t`New build` : a.toVersion;
  const kind = kindWord(a.kind);
  return (
    <span
      className={cx("update-mark", a.kind === UpdateKind.MAJOR && "is-major", className)}
      title={t`Update available: ${stepOf(a)}, ${kind}`}
    >
      <Icon name="arrow-up" size={12} />
      <span className="update-mark-to">{label}</span>
    </span>
  );
}

/** On a stack's row: how many of its apps have an update. */
export function StackUpdates({ services, now }: { services: readonly ServiceInfo[]; now: number }) {
  const count = withUpdates(services, now).length;
  if (count === 0) return null;
  return (
    <span className="update-mark is-count">
      <Icon name="arrow-up" size={12} />
      {plural(count, { one: "# update", other: "# updates" })}
    </span>
  );
}

/** Calls of ServiceUpdateService for one app, which read the list again once they've answered. */
function useUpdateCalls(service: ServiceInfo) {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState<"" | "check" | "skip" | "snooze">("");
  const [failed, setFailed] = useState("");
  const run = async (what: "check" | "skip" | "snooze", call: () => Promise<unknown>) => {
    setBusy(what);
    setFailed("");
    try {
      await call();
      await readServicesAgain(queryClient);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy("");
    }
  };
  const client = () => createClient(ServiceUpdateService, transport);
  return {
    busy,
    failed,
    check: () => run("check", () => client().checkServiceUpdates({ id: service.id })),
    skip: (version: string, unskip = false) =>
      run("skip", () => client().skipServiceUpdate({ id: service.id, version, unskip })),
    snooze: (duration: string) => run("snooze", () => client().snoozeServiceUpdates({ id: service.id, duration })),
  };
}

/** What skipping names an update by. */
function targetOf(a: AvailableUpdate): string {
  if (a.kind === UpdateKind.DIGEST) return a.toDigest;
  if (a.kind === UpdateKind.COMMIT) return a.toCommit;
  return a.toVersion;
}

/**
 * On an app's page: what's newer than what it runs, with Update, its notes and
 * when it came out, and skip and snooze one step away. Up to date, or unable to
 * say, it's a line with Check now. Nothing before the first check (ADR 0079).
 */
export function UpdateCard({ service }: { service: ServiceInfo }) {
  const { t, i18n } = useLingui();
  const update = service.update;
  const time = useTimeWords();
  const kindWord = useKindWord();
  const checked = update?.checkTime ? timestampDate(update.checkTime) : undefined;
  const now = useAgoNow(checked);
  const calls = useUpdateCalls(service);
  const may = useMay();
  const here = { environment: environmentOf(service) };
  const mayCheck = may(ServiceUpdateService.method.checkServiceUpdates, here);
  const maySet = may(ServiceUpdateService.method.skipServiceUpdate, here);
  const maySnooze = may(ServiceUpdateService.method.snoozeServiceUpdates, here);
  const act = useUpdateAction(service);
  if (!update || update.check === UpdateCheck.UNSPECIFIED) return null;

  const until = snoozedUntil(update, now);
  const first = offered(update);
  const rest = update.available.filter((a) => a !== first && !a.skipped);
  const skipped = update.available.filter((a) => a.skipped);
  const checkedWords = checked ? t`Checked ${time.ago(checked, now)}` : "";
  const checkNow =
    update.source === UpdateSource.CATALOG ? null : (
      <Button
        variant="ghost"
        size="sm"
        icon="refresh"
        onClick={() => void calls.check()}
        loading={calls.busy === "check"}
        {...mayCheck.off}
      >
        <Trans>Check now</Trans>
      </Button>
    );
  const failedLine = calls.failed ? (
    <p className="update-failed" role="alert">
      {calls.failed}
    </p>
  ) : null;

  if (!first) {
    // Up to date, or it couldn't say: a line, with the way to ask again.
    return (
      <Panel title={t`Updates`} className="update-card is-quiet" actions={checkNow}>
        <p className="update-state">
          <UpdateState update={update} />
          {checkedWords ? <span className="update-when"> · {checkedWords}</span> : null}
        </p>
        {skipped.length > 0 ? (
          <SkippedLine skipped={skipped} onUnskip={(a) => void calls.skip(targetOf(a), true)} off={maySet.off} />
        ) : null}
        {failedLine}
      </Panel>
    );
  }

  const released = first.releaseTime ? timestampDate(first.releaseTime) : undefined;
  const found = first.foundTime ? timestampDate(first.foundTime) : undefined;
  const when = released
    ? t`out ${time.span(Math.max(0, now - released.getTime()))}`
    : found
      ? t`found ${time.ago(found, now)}`
      : "";
  return (
    <Panel
      title={until ? t`Updates snoozed` : t`Update available`}
      className={cx("update-card", until && "is-snoozed")}
      actions={checkNow}
    >
      <div className="update-offer">
        <div className="update-offer-line">
          <code className="update-step">{stepOf(first)}</code>
          <Tag className={cx("update-kind", first.kind === UpdateKind.MAJOR && "is-major")}>{kindWord(first.kind)}</Tag>
          {when ? <span className="update-when">{when}</span> : null}
          {first.notesUrl ? (
            <a className="update-notes" href={first.notesUrl} target="_blank" rel="noreferrer">
              {first.kind === UpdateKind.COMMIT ? (
                <Trans>What changed</Trans>
              ) : (
                <Trans context="release notes">Notes</Trans>
              )}
            </a>
          ) : null}
        </div>
        {until ? (
          <Button size="sm" onClick={() => void calls.snooze("")} loading={calls.busy === "snooze"} {...maySnooze.off}>
            <Trans>Show again</Trans>
          </Button>
        ) : (
          <Button variant="primary" size="sm" icon="arrow-up" onClick={() => act.run(first)} {...act.offer.off}>
            {act.label(first)}
          </Button>
        )}
      </div>
      {until ? (
        <p className="update-when">
          <Trans>Snoozed until {formatDateTime(until, i18n.locale)}</Trans>
        </p>
      ) : null}
      {rest.map((a) => (
        <div className="update-offer is-also" key={`${a.kind}:${targetOf(a)}`}>
          <div className="update-offer-line">
            <span className="update-also">
              <Trans>Also</Trans>
            </span>
            <code className="update-step">{a.toVersion}</code>
            <Tag className={cx("update-kind", a.kind === UpdateKind.MAJOR && "is-major")}>{kindWord(a.kind)}</Tag>
          </div>
          {until ? null : (
            <Button size="sm" variant="ghost" onClick={() => act.run(a)} {...act.offer.off}>
              {act.label(a)}
            </Button>
          )}
        </div>
      ))}
      <details className="update-more">
        <summary>
          <Trans>Not now</Trans>
        </summary>
        <div className="update-more-actions">
          <Button
            size="sm"
            onClick={() => void calls.skip(targetOf(first))}
            loading={calls.busy === "skip"}
            {...maySet.off}
          >
            <Trans>Skip {first.toVersion}</Trans>
          </Button>
          {until ? null : (
            <>
              <Button size="sm" variant="ghost" onClick={() => void calls.snooze("1d")} {...maySnooze.off}>
                <Trans>Snooze a day</Trans>
              </Button>
              <Button size="sm" variant="ghost" onClick={() => void calls.snooze("7d")} {...maySnooze.off}>
                <Trans>Snooze a week</Trans>
              </Button>
              <Button size="sm" variant="ghost" onClick={() => void calls.snooze("30d")} {...maySnooze.off}>
                <Trans>Snooze a month</Trans>
              </Button>
            </>
          )}
        </div>
        {skipped.length > 0 ? (
          <SkippedLine skipped={skipped} onUnskip={(a) => void calls.skip(targetOf(a), true)} off={maySet.off} />
        ) : null}
      </details>
      {checkedWords ? <p className="update-when">{checkedWords}</p> : null}
      {failedLine}
      {act.sheet}
    </Panel>
  );
}

/** What was skipped, each with the way to show it again. */
function SkippedLine({
  skipped,
  onUnskip,
  off,
}: {
  skipped: readonly AvailableUpdate[];
  onUnskip: (a: AvailableUpdate) => void;
  off: { disabled?: boolean; title?: string };
}) {
  return (
    <p className="update-skipped">
      <Trans>Skipped:</Trans>{" "}
      {skipped.map((a) => (
        <Button key={`${a.kind}:${targetOf(a)}`} size="sm" variant="ghost" onClick={() => onUnskip(a)} {...off}>
          <Trans>Show {a.toVersion} again</Trans>
        </Button>
      ))}
    </p>
  );
}

/** How an app's check stands, in words, when it offers nothing. */
function UpdateState({ update }: { update: ServiceUpdate }): ReactNode {
  switch (update.check) {
    case UpdateCheck.UP_TO_DATE:
    case UpdateCheck.AVAILABLE:
      return (
        <span className="update-ok">
          <Icon name="check" size={14} />
          <Trans>Up to date</Trans>
        </span>
      );
    default:
      return <ProblemWords problem={update.problem} />;
  }
}

function ProblemWords({ problem }: { problem: UpdateProblem }): ReactNode {
  switch (problem) {
    case UpdateProblem.PRIVATE_OR_NOT_FOUND:
      return <Trans>Can't check: the image is private, or not on its registry</Trans>;
    case UpdateProblem.UNREACHABLE:
      return <Trans>Can't reach its registry from its server; checked again later</Trans>;
    case UpdateProblem.RATE_LIMITED:
      return <Trans>Its registry asked Dockwell to wait; checked again later</Trans>;
    case UpdateProblem.SERVER_OFFLINE:
      return <Trans>Its server is offline; checked when it's back</Trans>;
    case UpdateProblem.AGENT_OUTDATED:
      return <Trans>Its server's agent is too old to check; update the agent</Trans>;
    case UpdateProblem.NOT_RUNNING:
      return <Trans>Not running yet, so there's nothing to compare</Trans>;
    case UpdateProblem.LOCAL_IMAGE:
      return <Trans>Built on its server, so no registry has newer versions</Trans>;
    default:
      return <Trans>Not checked yet</Trans>;
  }
}

/**
 * What Update does for an app: a catalog install's upgrade plan, a build of an
 * app's newest commit, or another image in the deploy sheet, filled in.
 */
function useUpdateAction(service: ServiceInfo) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay();
  const environment = environmentOf(service);
  const install = installIn(useInstalls().data?.installs ?? [], environment);
  const [deploying, setDeploying] = useState("");
  const source = service.update?.source;
  const offer =
    source === UpdateSource.CATALOG && install
      ? may(CatalogService.method.upgradeCatalogInstall, { project: install.projectId })
      : may(ServiceService.method.deployService, { environment });
  const run = (a: AvailableUpdate) => {
    if (source === UpdateSource.CATALOG) {
      if (!install) return;
      sheet.open({
        kind: "plan",
        make: () => createClient(CatalogService, transport).upgradeCatalogInstall({ id: install.projectId }),
      });
      return;
    }
    if (a.kind === UpdateKind.COMMIT) {
      sheet.open({
        kind: "plan",
        make: () => createClient(ServiceService, transport).deployService({ id: service.id }),
      });
      return;
    }
    setDeploying(a.toImage);
  };
  const label = (a: AvailableUpdate) => {
    if (a.kind === UpdateKind.RELEASE) return t`Review the change`;
    if (a.kind === UpdateKind.DIGEST) return t`Pull the new build`;
    if (a.kind === UpdateKind.COMMIT) return t`Deploy the new commits`;
    return t`Update to ${a.toVersion}`;
  };
  const shown =
    deploying !== "" ? <DeploySheet service={service} image={deploying} onClose={() => setDeploying("")} /> : null;
  return { run, label, offer, sheet: shown };
}
