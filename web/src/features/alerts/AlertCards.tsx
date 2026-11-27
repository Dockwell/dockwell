// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  type Alert,
  type AlertRuleInfo,
  AlertService,
  AlertState,
  AlertRule_Condition as Condition,
  Connection,
  type Server,
  AlertRule_Severity as Severity,
} from "@dockwell/api";
import { Button, Callout, cx, Icon, type IconName, Sparkline, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatClock, formatDateTime, formatDay, sameDay } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useActorWords } from "../activity/actor";
import { alertTitle } from "../activity/describe";
import { useSignedInUser } from "../auth/useAuthState";
import { trend } from "../servers/history";
import { useControlPlaneNow, useFleet } from "../servers/useFleet";
import { AlertActions } from "./AlertActions";
import { useAlertKind } from "./alertKind";

const MARK: Partial<Record<Condition, IconName>> = {
  [Condition.SERVICE_DOWN]: "service",
  [Condition.CHECK_DOWN]: "activity",
  [Condition.SERVER_UNREACHABLE]: "server",
  [Condition.DISK_FULL]: "server",
  [Condition.MEMORY_HIGH]: "server",
  [Condition.CERTIFICATE_EXPIRING]: "link",
  [Condition.CERTIFICATE_FAILED]: "link",
  [Condition.BACKUP_FAILED]: "archive",
  [Condition.BACKUP_LATE]: "archive",
  [Condition.CHANGE_FAILED]: "activity",
  [Condition.RESTORE_DRILL_FAILED]: "archive",
  [Condition.RESTORE_DRILL_LATE]: "archive",
  [Condition.UPDATE_AVAILABLE]: "arrow-up",
  [Condition.CATALOG_SOURCE_STUCK]: "branch",
  [Condition.DOMAIN_UNPUBLISHED]: "globe",
};

function ruleTitle(alert: Alert, rules: readonly AlertRuleInfo[] | undefined): string | undefined {
  return rules?.find((rule) => rule.id === alert.ruleId)?.title;
}

const ms = (a: { seconds: bigint } | undefined) => (a ? Number(a.seconds) * 1000 : undefined);

/** Critical before warning, then the newest first, as Alerts has them: "Most severe first". */
export function bySeverity(alerts: readonly Alert[]): Alert[] {
  const rank = (a: Alert) => (a.severity === Severity.CRITICAL ? 0 : 1);
  return [...alerts].sort((a, b) => rank(a) - rank(b) || (ms(b.openTime) ?? 0) - (ms(a.openTime) ?? 0));
}

/**
 * Says you're on an alert, or takes that back, and reads the alerts again. It's an
 * audited action, as a mute is, with no plan (ADR 0044): it holds nothing back and
 * sends nothing. What the control plane refuses is kept for the card to say.
 */
function useAcknowledge() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(AlertService, transport), [transport]);
  // By alert, as two cards can be asked at once, and each keeps its own refusal until it's closed or asked again.
  const [busy, setBusy] = useState<ReadonlySet<string>>(new Set());
  const [failed, setFailed] = useState<ReadonlyMap<string, unknown>>(new Map());
  const mark = (id: string, on: boolean) =>
    setBusy((was) => {
      const next = new Set(was);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  const fail = (id: string, error?: unknown) =>
    setFailed((was) => {
      const next = new Map(was);
      if (error === undefined) next.delete(id);
      else next.set(id, error);
      return next;
    });
  const run = async (id: string, on: boolean) => {
    mark(id, true);
    fail(id);
    try {
      if (on) await client.acknowledgeAlert({ id });
      else await client.unacknowledgeAlert({ id });
    } catch (error) {
      fail(id, error);
    } finally {
      mark(id, false);
      // Refused or not, the alert may no longer be as the card says: it may have resolved, or someone took it over.
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: AlertService, cardinality: undefined }),
      });
    }
  };
  return { busy, failed, run, dismiss: (id: string) => fail(id) };
}

type Acknowledge = ReturnType<typeof useAcknowledge>;

/**
 * What's open, each as a card, as Alerts draws them: what's wrong and how much it matters,
 * since when, what it means, and what was sent. A server's memory has its trend beside it,
 * from the minutes the control plane keeps. Each says who's on it, and offers to take it.
 */
export function OpenAlerts({
  alerts,
  rules,
}: {
  alerts: readonly Alert[];
  rules: readonly AlertRuleInfo[] | undefined;
}) {
  const { t } = useLingui();
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? [] : fleet.servers;
  const now = useControlPlaneNow(30_000);
  const acknowledge = useAcknowledge();
  return (
    <section className="alerts-main" aria-labelledby="alerts-open">
      <div className="alerts-heading">
        <h2 id="alerts-open" className="dw-text-heading">
          {t({ message: "Open", context: "alerts that haven't resolved" })}
        </h2>
        {alerts.length > 1 ? (
          <span className="alerts-muted">
            <Trans>Most severe first</Trans>
          </span>
        ) : null}
      </div>
      {alerts.length === 0 ? (
        <p className="alerts-muted alerts-note">
          <Trans>Nothing is open. What fires shows here until it resolves.</Trans>
        </p>
      ) : (
        <ul className="alerts-cards">
          {bySeverity(alerts).map((alert) => (
            <li key={alert.id}>
              <AlertCard
                alert={alert}
                rule={ruleTitle(alert, rules)}
                server={servers.find((s) => s.id === alert.serverId)}
                now={now}
                acknowledge={acknowledge}
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function AlertCard({
  alert,
  rule,
  server,
  now,
  acknowledge,
}: {
  alert: Alert;
  rule: string | undefined;
  server: Server | undefined;
  now: number;
  acknowledge: Acknowledge;
}) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const me = useSignedInUser();
  const actorWords = useActorWords();
  // Who's on it, if anyone: you, or someone else, named and marked as Activity does, so a
  // token reads as a token whatever its maker called it. Your own token isn't you.
  const by = alert.acknowledgeTime ? alert.acknowledgedBy : undefined;
  const mine = by !== undefined && me !== undefined && by.kind === ActorKind.USER && by.id === me.id;
  const who = actorWords(by);
  const name = who.name;
  const ago = alert.acknowledgeTime ? words.ago(timestampDate(alert.acknowledgeTime), now) : "";
  const busy = acknowledge.busy.has(alert.id);
  const failed = acknowledge.failed.get(alert.id);
  const critical = alert.severity === Severity.CRITICAL;
  const title = alertTitle(alert, i18n);
  const kind = useAlertKind()(alert.condition);
  const opened = ms(alert.openTime);
  const lasted = opened === undefined ? "" : words.span(Math.max(0, now - opened));
  const since = alert.openTime ? formatClock(timestampDate(alert.openTime), i18n.locale) : "";
  const sent = alert.sentTime ? formatClock(timestampDate(alert.sentTime), i18n.locale) : "";
  return (
    <article className={cx("dw-panel", "alerts-card", critical ? "is-critical" : "is-warning")}>
      <span className="alerts-card-mark" aria-hidden="true">
        <Icon name={MARK[alert.condition] ?? "service"} size={20} />
      </span>
      <div className="alerts-card-head">
        <h3 className="alerts-card-title">
          {/* Its own page: what changed around it, what was sent, and how it ends. */}
          <Link to="/alerts/$alertId" params={{ alertId: alert.id }}>
            {title}
          </Link>
        </h3>
        {critical ? (
          <StatusBadge status="down" label={t`Critical`} variant="pill" labelIsState />
        ) : (
          <StatusBadge status="degraded" label={t`Warning`} variant="pill" labelIsState />
        )}
      </div>
      <div className="alerts-card-when">
        {lasted ? (
          <span className="alerts-card-lasted">
            <strong>{lasted}</strong> <span className="alerts-muted">{t`since ${since}`}</span>
          </span>
        ) : null}
        <MemoryTrend alert={alert} server={server} />
      </div>
      {/* What to do, as buttons: the detail's sentence of what to check is what these do (#794). */}
      <AlertActions alert={alert} />
      <div className="alerts-card-foot">
        {/* A status, so a change of who has it is said to someone who can't see it change. */}
        <span role="status" className={cx("alerts-card-seen", by === undefined && "alerts-muted")}>
          {by === undefined ? (
            <Trans>Not acknowledged</Trans>
          ) : (
            <>
              {who.icon ? (
                <span className="alerts-card-by" aria-hidden="true">
                  <Icon name={who.icon} size={12} />
                </span>
              ) : null}
              <span title={who.full === who.name ? undefined : who.full}>
                {mine ? t`Acknowledged by you` : t`Acknowledged by ${name}`}
              </span>
              <span className="alerts-muted"> · {ago}</span>
            </>
          )}
        </span>
        <span className="alerts-card-spacer" />
        <span className="alerts-muted">
          {rule || kind}
          {" · "}
          <Sending alert={alert} sent={sent} />
        </span>
        {mine ? (
          <Button
            size="sm"
            variant="ghost"
            loading={busy}
            aria-label={t`Take back your acknowledgement of: ${title}`}
            onClick={() => void acknowledge.run(alert.id, false)}
          >
            <Trans>Take back</Trans>
          </Button>
        ) : (
          <Button
            size="sm"
            icon="check"
            loading={busy}
            aria-label={by === undefined ? t`Acknowledge: ${title}` : t`Take over from ${name}: ${title}`}
            onClick={() => void acknowledge.run(alert.id, true)}
          >
            {by === undefined ? <Trans>Acknowledge</Trans> : <Trans>Take over</Trans>}
          </Button>
        )}
      </div>
      {failed ? (
        <Callout
          tone="critical"
          title={t`That didn't take`}
          className="alerts-card-failed"
          actions={
            <Button size="sm" variant="ghost" onClick={() => acknowledge.dismiss(alert.id)}>
              <Trans>Close</Trans>
            </Button>
          }
        >
          {sentence(failed)}
        </Callout>
      ) : null}
    </article>
  );
}

/**
 * Whether what an alert sent reached anyone: held by a mute, nothing sent, sent at
 * a time, or failed to send where no channel took its newest message (#794).
 */
export function Sending({ alert, sent }: { alert: Alert; sent: string }) {
  const { t } = useLingui();
  if (alert.muted) return <>{t`Held by a mute`}</>;
  if (alert.undelivered) return <span className="alerts-undelivered">{t`Failed to send`}</span>;
  return <>{sent ? t`Sent ${sent}` : t`Nothing sent`}</>;
}

/** A server's memory over the minutes the control plane keeps, for an alert that's about it, while it reports. */
function MemoryTrend({ alert, server }: { alert: Alert; server: Server | undefined }) {
  const { t } = useLingui();
  if (alert.condition !== Condition.MEMORY_HIGH || !server || server.connection !== Connection.CONNECTED) return null;
  const { samples } = trend(server.history.length > 0 ? server.history : server.stats ? [server.stats] : []);
  const share = samples
    .filter((s) => s.memoryTotalBytes > 0n)
    .map((s) => (Number(s.memoryUsedBytes) / Number(s.memoryTotalBytes)) * 100);
  const last = share.at(-1);
  if (share.length < 2 || last === undefined) return null;
  const percent = Math.round(last);
  const name = server.name;
  return (
    <>
      <Sparkline
        data={share}
        width={124}
        height={24}
        min={Math.min(40, ...share)}
        max={100}
        tone="warning"
        area
        label={t`Memory on ${name}, now ${percent}%`}
      />
      <span className="alerts-card-caption">{t`Memory, now ${percent}%`}</span>
    </>
  );
}

/** What resolved, newest first, beside what's open: what it was, when it ended and how long it lasted. */
export function ResolvedAlerts({
  alerts,
  rules,
}: {
  alerts: readonly Alert[];
  rules: readonly AlertRuleInfo[] | undefined;
}) {
  const { i18n } = useLingui();
  const kind = useAlertKind();
  const words = useTimeWords();
  // The list's own clock, so a row resolved before midnight turns to its day as the day turns.
  const now = useControlPlaneNow(60_000);
  if (alerts.length === 0) return null;
  const rows = alerts;
  return (
    <aside className="alerts-side" aria-labelledby="alerts-resolved">
      <div className="alerts-heading">
        <h2 id="alerts-resolved" className="dw-text-heading">
          <Trans>Resolved</Trans>
        </h2>
        <span className="alerts-muted">{alerts.length}</span>
        <span className="alerts-card-spacer" />
        <Link to="/activity" className="alerts-history">
          <Trans>History</Trans>
        </Link>
      </div>
      <ul className="dw-panel alerts-resolved">
        {rows.map((alert) => {
          const from = ms(alert.openTime);
          const to = ms(alert.resolveTime);
          const lasted = from !== undefined && to !== undefined ? words.span(Math.max(0, to - from)) : "";
          return (
            <li key={alert.id} className="alerts-resolved-row">
              <span className="alerts-resolved-mark" aria-hidden="true">
                <Icon name="check" size={14} />
              </span>
              <Link to="/alerts/$alertId" params={{ alertId: alert.id }} className="alerts-resolved-title">
                {alertTitle(alert, i18n)}
              </Link>
              <ResolvedAt time={alert.resolveTime} now={now} />
              <span className="alerts-resolved-meta">
                {[lasted, ruleTitle(alert, rules) || kind(alert.condition)].filter(Boolean).join(" · ")}
              </span>
            </li>
          );
        })}
      </ul>
    </aside>
  );
}

/**
 * When an alert resolved, as Alerts' list has it: the time for today, the day before that, so it takes a
 * narrow column's corner rather than half its row; the whole of it on hover.
 */
function ResolvedAt({ time, now }: { time: Timestamp | undefined; now: number }) {
  const { i18n } = useLingui();
  if (!time) return <span className="alerts-resolved-at" />;
  const at = timestampDate(time);
  const today = sameDay(at, new Date(now));
  return (
    <span className="alerts-resolved-at" title={formatDateTime(at, i18n.locale)}>
      {today ? formatClock(at, i18n.locale) : formatDay(at, i18n.locale)}
    </span>
  );
}

/**
 * Taking an open alert, on its own page as on its card: Acknowledge, Take over from whoever has it, or Take back
 * your own, with why where it didn't take. The page's head has it, as Incident's has its actions.
 */
export function AlertAcknowledge({ alert }: { alert: Alert }) {
  const { t } = useLingui();
  const acknowledge = useAcknowledge();
  const me = useSignedInUser();
  const actorWords = useActorWords();
  if (alert.state !== AlertState.FIRING) return null;
  const by = alert.acknowledgeTime ? alert.acknowledgedBy : undefined;
  const mine = by !== undefined && me !== undefined && by.kind === ActorKind.USER && by.id === me.id;
  const name = actorWords(by).name;
  const busy = acknowledge.busy.has(alert.id);
  const failed = acknowledge.failed.get(alert.id);
  return (
    <span className="alerts-page-acknowledge">
      {failed ? (
        <span role="alert" className="alerts-page-failed">
          {t`That didn't take: ${sentence(failed)}`}
        </span>
      ) : null}
      {mine ? (
        <Button variant="ghost" loading={busy} onClick={() => void acknowledge.run(alert.id, false)}>
          <Trans>Take back</Trans>
        </Button>
      ) : (
        <Button
          icon="check"
          loading={busy}
          title={by === undefined ? undefined : t`Take over from ${name}`}
          onClick={() => void acknowledge.run(alert.id, true)}
        >
          {by === undefined ? <Trans>Acknowledge</Trans> : <Trans>Take over</Trans>}
        </Button>
      )}
    </span>
  );
}
