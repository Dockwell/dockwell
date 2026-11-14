// SPDX-License-Identifier: AGPL-3.0-only

import { type Timestamp, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ActorKind,
  type Alert,
  type AlertDelivery,
  AlertState,
  AlertRule_Condition as Condition,
  type Event,
  EventKind,
  EventService,
  AlertRule_Severity as Severity,
} from "@dockwell/api";
import { Callout, Icon, Panel, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery as useAnyQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { formatClock, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { EventTimeline } from "../activity/ActivityPage";
import { useActorWords } from "../activity/actor";
import { alertTitle, happenedAt } from "../activity/describe";
import { useControlPlaneNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ReadFailed } from "../shell/ReadFailed";
import { AlertAcknowledge } from "./AlertCards";
import { useAlerts, useDeliveries, useRules } from "./useAlerts";
import "./Alerts.css";

/** How long before an alert fired its page looks for what changed. */
export const BEFORE_MS = 60 * 60_000;
/** How many pages of events, of 100, it reads back to reach that far. */
const PAGES = 5;

const ms = (t: { seconds: bigint } | undefined) => (t ? Number(t.seconds) * 1000 : undefined);

/**
 * The events around an alert, newest first: the changes and health of what it's about, and the alert's
 * own. Events have no read by time, so this pages back from now until it's past an hour before the
 * alert fired, or five pages, and says when that wasn't far enough.
 */
function useEventsAround(alert: Alert | undefined) {
  const transport = useTransport();
  const client = useMemo(() => createClient(EventService, transport), [transport]);
  const from = (ms(alert?.openTime) ?? 0) - BEFORE_MS;
  // An app's alert is read by the app; anything else by its server.
  const about = alert?.condition === Condition.SERVICE_DOWN && alert.subjectId ? alert.subjectId : "";
  const serverId = about ? "" : (alert?.serverId ?? "");
  return useAnyQuery({
    queryKey: ["alert-events", alert?.id, about, serverId, alert?.state],
    enabled: alert !== undefined,
    queryFn: async ({ signal }) => {
      const events: Event[] = [];
      let pageToken = "";
      let reached = false;
      for (let page = 0; page < PAGES && !reached; page++) {
        const res = await client.listEvents(
          {
            pageSize: 100,
            pageToken,
            kinds: [EventKind.CHANGE, EventKind.HEALTH, EventKind.ALERT],
            resourceId: about,
            serverId,
            actorId: "",
            actorKind: ActorKind.UNSPECIFIED,
          },
          { signal },
        );
        events.push(...res.events);
        pageToken = res.nextPageToken;
        const oldest = res.events.at(-1);
        reached = pageToken === "" || (oldest !== undefined && happenedAt(oldest).getTime() < from);
      }
      return { events, reached };
    },
  });
}

/**
 * One alert on a page of its own, the first slice of the incident view: what fired and what its rule
 * saw, what changed on what it's about shortly before and while it was open, what happened to the alert
 * itself, what was sent, who has it and how it ended. It reads only what the API already gives, and
 * changes nothing.
 */
export function AlertPage({ alertId }: { alertId: string }) {
  const { t, i18n } = useLingui();
  const alerts = useAlerts();
  const alert = alerts.data?.alerts.find((a) => a.id === alertId);
  const title = alert ? alertTitle(alert, i18n) : t`Alert`;
  const back = (
    <Link to="/alerts" className="alerts-back">
      <Icon name="arrow-left" size={16} />
      <Trans>Alerts</Trans>
    </Link>
  );
  return (
    <Page
      title={title}
      breadcrumb={back}
      // A phone's header has the arrow alone, as every page's has.
      backLink={
        <Link to="/alerts" className="mobile-header-button" aria-label={t`Back to alerts`}>
          <Icon name="arrow-left" size={20} />
        </Link>
      }
      badge={alert ? <State alert={alert} /> : undefined}
      // Taking it, from its page as from its card, so "Nobody yet" comes with the way to change it.
      actions={alert ? <AlertAcknowledge alert={alert} /> : undefined}
      lead={alert ? <Lead alert={alert} /> : undefined}
      className="alerts alert-page"
    >
      {alerts.error ? (
        <ReadFailed error={alerts.error} title={t`Can't load the alerts`} />
      ) : !alerts.data ? null : !alert ? (
        // There's no read of one alert by its ID, so one that resolved long ago isn't in what the list holds.
        <EmptyState icon="alert" title={t`This alert isn't among the newest`}>
          <Trans>
            Alerts lists what's open and the newest that resolved. This one resolved before those, or was never there.
            Activity keeps what fired and resolved.
          </Trans>
        </EmptyState>
      ) : (
        <Body alert={alert} />
      )}
    </Page>
  );
}

function State({ alert }: { alert: Alert }) {
  const { t } = useLingui();
  if (alert.state !== AlertState.FIRING) return <StatusBadge status="healthy" label={t`Resolved`} labelIsState />;
  return alert.severity === Severity.CRITICAL ? (
    <StatusBadge status="down" label={t`Critical`} variant="pill" labelIsState />
  ) : (
    <StatusBadge status="degraded" label={t`Warning`} variant="pill" labelIsState />
  );
}

/** Where it is and what it's about, each a link to its own page. */
function Lead({ alert }: { alert: Alert }) {
  const parts: ReactNode[] = [];
  if (alert.condition === Condition.SERVICE_DOWN && alert.subjectId) {
    parts.push(
      <Link key="app" to="/apps/$serviceId" params={{ serviceId: alert.subjectId }}>
        {alert.subjectName || alert.subjectId}
      </Link>,
    );
  }
  if (alert.serverId) {
    parts.push(
      <Link key="server" to="/servers/$serverId" params={{ serverId: alert.serverId }}>
        {alert.serverName || alert.serverId}
      </Link>,
    );
  }
  parts.push(<code key="rule">{alert.ruleName}</code>);
  return (
    <>
      {parts.map((part, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a fixed, short list
        <span key={i}>
          {i > 0 ? " · " : null}
          {part}
        </span>
      ))}
    </>
  );
}

function Body({ alert }: { alert: Alert }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const actorWords = useActorWords();
  const now = useControlPlaneNow(30_000);
  const rule = useRules().data?.rules.find((r) => r.id === alert.ruleId);
  const around = useEventsAround(alert);
  const deliveries = useDeliveries();

  const opened = ms(alert.openTime) ?? 0;
  const resolved = ms(alert.resolveTime);
  const open = alert.state === AlertState.FIRING;
  const lasted = words.span(Math.max(0, (resolved ?? now) - opened));
  const when = (time: Timestamp | undefined) => (time ? formatDateTime(timestampDate(time), i18n.locale) : "");

  const events = around.data?.events ?? [];
  const at = (e: Event) => happenedAt(e).getTime();
  const others = events.filter((e) => e.kind !== EventKind.ALERT);
  const before = others.filter((e) => at(e) >= opened - BEFORE_MS && at(e) < opened);
  const during = others.filter((e) => at(e) >= opened && at(e) <= (resolved ?? Number.POSITIVE_INFINITY));
  const own = events.filter((e) => e.kind === EventKind.ALERT && e.alert?.id === alert.id);
  // Where Activity has the rest of it: what the alert is about, as this page reads it.
  const about = alert.condition === Condition.SERVICE_DOWN && alert.subjectId ? alert.subjectId : "";
  const further = about ? { resource: about } : alert.serverId ? { server: alert.serverId } : {};
  const summaries = useMemo(() => {
    const out = new Map<string, string>();
    for (const e of around.data?.events ?? []) if (e.change) out.set(e.id, e.change.summary);
    return out;
  }, [around.data]);

  const sent = messagesAbout(alert, deliveries.data?.deliveries ?? []);
  const by = alert.acknowledgeTime ? actorWords(alert.acknowledgedBy) : undefined;
  const firedAt = alert.openTime ? formatClock(timestampDate(alert.openTime), i18n.locale) : "";

  const facts: [string, ReactNode][] = [
    [t`Fired`, when(alert.openTime)],
    [open ? t`Open for` : t`Lasted`, lasted],
  ];
  if (!open) facts.push([t`Resolved`, when(alert.resolveTime)]);
  facts.push([
    t`Rule`,
    <span key="rule">
      {rule?.title ? <>{rule.title} · </> : null}
      <code>{rule?.expression || alert.ruleName}</code>
    </span>,
  ]);
  facts.push([
    t`Acknowledged`,
    by ? (
      <span key="by" title={by.full === by.name ? undefined : by.full}>
        {by.name} · {when(alert.acknowledgeTime)}
      </span>
    ) : open ? (
      t`Nobody yet`
    ) : (
      t`Nobody did`
    ),
  ]);
  facts.push([
    t`Sending`,
    alert.muted ? t`Held by a mute` : alert.sentTime ? t`Sent ${when(alert.sentTime)}` : t`Nothing sent`,
  ]);

  return (
    <div className="alerts-columns">
      <div className="alerts-main alert-main">
        {/* How it ended comes first once it has: nothing here needs doing any more. */}
        {!open ? <Callout tone="info">{t`It resolved ${when(alert.resolveTime)}, after ${lasted}.`}</Callout> : null}
        <Panel title={t`What the rule saw`}>
          {alert.detail ? <p className="alert-detail">{alert.detail}</p> : null}
          <dl className="alert-facts">
            {facts.map(([term, value]) => (
              <div key={term}>
                <dt>{term}</dt>
                <dd>{value}</dd>
              </div>
            ))}
          </dl>
        </Panel>

        {around.error ? (
          <ReadFailed error={around.error} title={t`Can't load what happened around it`} />
        ) : (
          <>
            <Panel
              title={t`What changed before it fired`}
              actions={<span className="alerts-muted">{t`The hour before ${firedAt}`}</span>}
            >
              {!around.data ? null : before.length > 0 ? (
                <EventTimeline events={before} summaries={summaries} now={now} framed={false} />
              ) : (
                <p className="alerts-muted alerts-note">
                  {around.data.reached ? (
                    <Trans>Nothing changed here in the hour before, and no check changed its answer.</Trans>
                  ) : (
                    <>
                      <Trans>It's further back than this page reads.</Trans>{" "}
                      <Link to="/activity" search={further}>
                        <Trans>See it on Activity</Trans>
                      </Link>
                    </>
                  )}
                </p>
              )}
            </Panel>
            {during.length > 0 ? (
              <Panel title={open ? t`What has changed since` : t`What changed while it was open`}>
                <EventTimeline events={during} summaries={summaries} now={now} framed={false} />
              </Panel>
            ) : null}
          </>
        )}
      </div>

      <aside className="alerts-side">
        <Panel
          title={t`This alert`}
          actions={
            <Link to="/activity" className="alerts-history">
              <Trans>Activity</Trans>
            </Link>
          }
        >
          {own.length > 0 ? (
            <EventTimeline events={own} summaries={summaries} now={now} framed={false} />
          ) : (
            <p className="alerts-muted alerts-note">
              {around.data && !around.data.reached ? (
                <>
                  <Trans>Its events are further back than this page reads.</Trans>{" "}
                  <Link to="/activity" search={{ ...further, kind: "alerts" }}>
                    <Trans>See them on Activity</Trans>
                  </Link>
                </>
              ) : (
                <Trans>Nothing more has happened to it.</Trans>
              )}
            </p>
          )}
        </Panel>

        <Panel
          title={t`Messages about it`}
          actions={
            <span className="alerts-muted">
              {sent.length > 0 ? plural(sent.length, { one: "# sent", other: "# sent" }) : null}
            </span>
          }
        >
          {sent.length === 0 ? (
            <p className="alerts-muted alerts-note">
              {alert.muted ? (
                <Trans>A mute holds what it would send.</Trans>
              ) : (
                <Trans>None among the newest messages. Channels has what was sent lately.</Trans>
              )}
            </p>
          ) : (
            <ul className="alert-sent">
              {sent.map((d) => (
                <li key={`${d.channel}-${String(d.time?.seconds)}-${d.about}`}>
                  <span className="alert-sent-to">
                    <StatusBadge
                      status={d.ok ? "healthy" : "down"}
                      label={d.channel}
                      meta={d.ok ? t`Delivered` : t`Failed`}
                    />
                  </span>
                  <span className="alerts-muted">
                    {when(d.time)}
                    {d.said ? <> · {d.said}</> : null}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </aside>
    </div>
  );
}

/**
 * The messages that were about an alert, among the newest the control plane lists. A message doesn't
 * name its alert, so they're matched by what they said and when: the alert's own title, firing or
 * resolved, from when it fired until shortly after it resolved.
 */
export function messagesAbout(alert: Alert, deliveries: readonly AlertDelivery[]): AlertDelivery[] {
  const from = ms(alert.openTime);
  if (from === undefined || alert.title === "") return [];
  const until = (ms(alert.resolveTime) ?? Number.POSITIVE_INFINITY) + 15 * 60_000;
  return deliveries.filter((d) => {
    const at = ms(d.time);
    if (at === undefined || at < from - 1000 || at > until) return false;
    return d.about === alert.title || d.about.endsWith(`: ${alert.title}`);
  });
}
