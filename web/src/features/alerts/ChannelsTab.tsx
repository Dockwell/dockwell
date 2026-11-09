// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import { type AlertChannelInfo, type AlertDelivery, AlertService, AlertRule_Severity as Severity } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatClock, formatDateTime } from "../../lib/format";
import { EmptyState } from "../shell/EmptyState";
import { AlertsPage } from "./AlertsPage";
import { ChannelSheet } from "./ChannelSheet";
import { CHANNEL_KIND, channelStatus, isChannelType, routing, useChannels, useDeliveries, useRules } from "./useAlerts";

/** When something was sent: the time today, or the date and time before. */
function useWhen() {
  const { i18n } = useLingui();
  return (at: Date | undefined) => {
    if (!at) return "";
    const today = new Date();
    return at.toDateString() === today.toDateString() ? formatClock(at, i18n.locale) : formatDateTime(at, i18n.locale);
  };
}

/** A channel's mark and name, with where it sends under them. */
function ChannelName({ channel }: { channel: AlertChannelInfo }) {
  const { i18n } = useLingui();
  const kind = isChannelType(channel.type) ? CHANNEL_KIND[channel.type] : CHANNEL_KIND.webhook;
  return (
    <div className="alerts-channel">
      <span className="alerts-channel-mark" aria-hidden="true">
        <Icon name={kind.icon} size={16} />
      </span>
      <div className="dw-cell">
        <span className="dw-cell-title">{channel.name}</span>
        <span className="alerts-channel-sub">
          {i18n._(kind.label)}
          {channel.target ? (
            <>
              {" · "}
              <code>{channel.target}</code>
            </>
          ) : null}
        </span>
      </div>
    </div>
  );
}

/** Sends a test through a channel by its name, and says how it went in a line. */
function useSendTest() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(AlertService, transport), [transport]);
  const [sending, setSending] = useState<string>();
  const [last, setLast] = useState<{ name: string; delivery?: AlertDelivery; error?: unknown }>();
  const send = async (name: string) => {
    setSending(name);
    setLast(undefined);
    try {
      const res = await client.testAlertChannel({ name });
      setLast({ name, delivery: res.delivery });
    } catch (error) {
      setLast({ name, error });
    } finally {
      setSending(undefined);
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: AlertService, cardinality: undefined }),
      });
    }
  };
  return { sending, last, send, clear: () => setLast(undefined) };
}

/**
 * The Channels tab: where alerts go, how each last went, what was sent lately,
 * and which channels each severity reaches, with a test for each.
 */
export function ChannelsTab() {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const channels = useChannels();
  const deliveries = useDeliveries();
  const when = useWhen();
  const test = useSendTest();
  const [editing, setEditing] = useState<AlertChannelInfo | "new">();
  const list = channels.data?.channels ?? [];

  const add = (
    <Button variant="primary" icon="plus" onClick={() => setEditing("new")}>
      <Trans>Add channel</Trans>
    </Button>
  );

  const sendButton = (c: AlertChannelInfo) => (
    <Button
      size="sm"
      disabled={test.sending !== undefined}
      aria-label={t`Send a test to ${c.name}`}
      onClick={() => void test.send(c.name)}
    >
      {test.sending === c.name ? <Trans>Sending…</Trans> : <Trans>Send test</Trans>}
    </Button>
  );

  const columns: TableColumn<AlertChannelInfo>[] = [
    { key: "name", header: t`Channel`, render: (c) => <ChannelName channel={c} /> },
    {
      key: "state",
      header: t`Status`,
      width: 148,
      render: (c) => {
        const s = channelStatus(c);
        return <StatusBadge status={s.status} label={i18n._(s.word)} labelIsState />;
      },
    },
    {
      key: "used",
      header: t`Used by`,
      width: 120,
      render: (c) =>
        c.rules > 0 ? (
          plural(c.rules, { one: "# rule", other: "# rules" })
        ) : (
          <span className="alerts-muted">{t`No rules`}</span>
        ),
    },
    {
      key: "last",
      header: t`Last sent`,
      width: 220,
      render: (c) =>
        c.last ? (
          <div className="dw-cell">
            <span>{when(c.last.time ? timestampDate(c.last.time) : undefined)}</span>
            <span className="alerts-channel-sub">{c.last.ok ? c.last.about : c.last.said}</span>
          </div>
        ) : (
          <span className="alerts-muted">{t`Nothing yet`}</span>
        ),
    },
    {
      key: "test",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 140,
      align: "right",
      render: sendButton,
    },
  ];

  const failing = list.filter((c) => c.last && !c.last.ok).length;
  const newest = list
    .map((c) => (c.last?.time ? timestampDate(c.last.time) : undefined))
    .filter((d): d is Date => d !== undefined)
    .sort((a, b) => b.getTime() - a.getTime())[0];
  const summary = [
    plural(list.length, { one: "# channel", other: "# channels" }),
    failing > 0 ? plural(failing, { one: "# failing", other: "# failing" }) : t`none failing`,
    ...(newest ? [t`last sent ${when(newest)}`] : []),
  ].join(" · ");

  let body: ReactNode;
  if (channels.error) {
    body = (
      <Callout tone="critical" title={t`Can't load the channels`}>
        {sentence(channels.error)}
      </Callout>
    );
  } else if (!channels.data) {
    body = null;
  } else if (list.length === 0) {
    body = (
      <EmptyState icon="bell" title={t`Nowhere to send alerts yet`} action={add}>
        <Trans>
          Dockwell watches your servers and apps from the first start, and puts what it finds on Activity. Add a
          channel, such as email, Slack or ntfy on your phone, and every rule sends to it.
        </Trans>
      </EmptyState>
    );
  } else {
    body = (
      <div className="alerts-columns">
        <section className="alerts-main">
          <div className="alerts-heading">
            <h2 className="dw-text-heading">
              <Trans>Channels</Trans>
            </h2>
            <span className="alerts-muted">{summary}</span>
          </div>
          {test.last ? <TestResult result={test.last} onClose={test.clear} /> : null}
          {phone ? (
            <ul className="alerts-phone-list">
              {list.map((c) => {
                const s = channelStatus(c);
                return (
                  <li key={c.id} className="alerts-phone-row">
                    <button type="button" className="alerts-phone-open" onClick={() => setEditing(c)}>
                      <ChannelName channel={c} />
                      <StatusBadge status={s.status} label={i18n._(s.word)} labelIsState />
                    </button>
                    {sendButton(c)}
                  </li>
                );
              })}
            </ul>
          ) : (
            <Table columns={columns} rows={list} caption={t`Alert channels`} onRowClick={(c) => setEditing(c)} />
          )}
          <Deliveries deliveries={deliveries.data?.deliveries ?? []} />
        </section>
        <aside className="alerts-side">
          <Routing />
        </aside>
      </div>
    );
  }

  return (
    <AlertsPage
      actions={list.length > 0 ? add : undefined}
      phoneAction={
        list.length > 0 ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Add channel`}
            onClick={() => setEditing("new")}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {body}
      {editing ? (
        <ChannelSheet channel={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} />
      ) : null}
    </AlertsPage>
  );
}

/** How a test from the list went, until it's closed or another is sent. */
function TestResult({
  result,
  onClose,
}: {
  result: { name: string; delivery?: AlertDelivery; error?: unknown };
  onClose: () => void;
}) {
  const { t } = useLingui();
  const { name } = result;
  const said = result.delivery?.said ?? "";
  const close = (
    <Button
      size="sm"
      variant="ghost"
      icon="x"
      aria-label={t`Close`}
      onClick={onClose}
      className="alerts-result-close"
    />
  );
  if (result.error) {
    return (
      <Callout tone="critical" title={t`Dockwell didn't send a test to ${name}`}>
        {sentence(result.error)}
        {close}
      </Callout>
    );
  }
  return result.delivery?.ok ? (
    <Callout tone="healthy" title={t`The test reached ${name}`}>
      {t`It answered ${said}.`}
      {close}
    </Callout>
  ) : (
    <Callout tone="critical" title={t`The test didn't reach ${name}`}>
      {t`It said: ${said}. Open the channel to check what it holds.`}
      {close}
    </Callout>
  );
}

/** A message, and every channel it went to at once. */
interface Sent {
  id: string;
  at: Date | undefined;
  about: string;
  to: string[];
  failed: string[];
}

/** Deliveries of one message to several channels, which go within a second of each other, as one row. */
function group(deliveries: readonly AlertDelivery[]): Sent[] {
  const out: Sent[] = [];
  for (const d of deliveries) {
    const at = d.time ? timestampDate(d.time) : undefined;
    const prev = out.at(-1);
    if (prev && prev.about === d.about && prev.at && at && Math.abs(prev.at.getTime() - at.getTime()) < 1000) {
      prev.to.push(d.channel);
      if (!d.ok) prev.failed.push(d.channel);
      continue;
    }
    out.push({
      id: `${at?.getTime() ?? 0}-${out.length}`,
      at,
      about: d.about,
      to: [d.channel],
      failed: d.ok ? [] : [d.channel],
    });
  }
  return out;
}

/** What was sent lately, newest first, and whether each arrived. */
function Deliveries({ deliveries }: { deliveries: readonly AlertDelivery[] }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const when = useWhen();
  const rows = group(deliveries).slice(0, 20);
  const list = (names: string[]) =>
    new Intl.ListFormat(i18n.locale, { style: "short", type: "conjunction" }).format(names);
  const failed = rows.filter((r) => r.failed.length > 0).length;
  const columns: TableColumn<Sent>[] = [
    { key: "at", header: t`Time`, width: 96, muted: true, render: (r) => when(r.at) },
    { key: "about", header: t`Alert` },
    { key: "to", header: t`Sent to`, width: 240, muted: true, render: (r) => list(r.to) },
    {
      key: "ok",
      header: t`Result`,
      width: 140,
      align: "right",
      render: (r) =>
        r.failed.length === 0 ? (
          <StatusBadge status="healthy" label={t`Delivered`} labelIsState />
        ) : (
          <StatusBadge status="down" label={t`Failed to ${list(r.failed)}`} labelIsState />
        ),
    },
  ];
  return (
    <>
      <div className="alerts-heading alerts-heading-gap">
        <h2 className="dw-text-heading">
          <Trans>Recent deliveries</Trans>
        </h2>
        <span className="alerts-muted">
          {rows.length === 0
            ? t`Nothing sent yet`
            : failed === 0
              ? t`None failed`
              : plural(failed, { one: "# failed", other: "# failed" })}
        </span>
      </div>
      {rows.length === 0 ? (
        <p className="alerts-muted alerts-note">
          <Trans>What Dockwell sends shows here, tests too, with what each channel answered.</Trans>
        </p>
      ) : phone ? (
        <ul className="alerts-phone-list">
          {rows.map((r) => (
            <li key={r.id} className="alerts-phone-delivery">
              <span className="alerts-delivery-about">{r.about}</span>
              <span className="alerts-muted">
                {when(r.at)} · {list(r.to)}
              </span>
              {r.failed.length > 0 ? (
                <StatusBadge status="down" label={t`Failed to ${list(r.failed)}`} labelIsState />
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        <Table columns={columns} rows={rows} density="compact" caption={t`Messages sent lately`} />
      )}
    </>
  );
}

/** Which channels what's critical reaches, and which a warning does, from the rules. */
function Routing() {
  const { t } = useLingui();
  const rules = useRules().data?.rules ?? [];
  const channels = useChannels().data?.channels ?? [];
  const routes = routing(rules, channels);
  return (
    <Panel title={t`Routing`}>
      <div className="alerts-routes">
        {routes.map((r) => (
          <div key={r.severity} className="alerts-route">
            <div className="alerts-route-head">
              {r.severity === Severity.CRITICAL ? (
                <StatusBadge status="down" label={t`Critical`} labelIsState />
              ) : (
                <StatusBadge status="degraded" label={t`Warning`} labelIsState />
              )}
              <span className="alerts-muted">{plural(r.rules, { one: "# rule", other: "# rules" })}</span>
            </div>
            {r.channels.length > 0 ? (
              <ul className="alerts-chips">
                {r.channels.map((name) => {
                  const c = channels.find((x) => x.name === name);
                  const kind = c && isChannelType(c.type) ? CHANNEL_KIND[c.type] : CHANNEL_KIND.webhook;
                  return (
                    <li key={name} className="alerts-chip">
                      <Icon name={kind.icon} size={14} />
                      {name}
                    </li>
                  );
                })}
              </ul>
            ) : (
              <p className="alerts-muted alerts-note">{t`Reaches no channel.`}</p>
            )}
          </div>
        ))}
        <p className="alerts-muted alerts-note">
          <Trans>A rule that names no channel sends to every one.</Trans>
        </p>
      </div>
    </Panel>
  );
}
