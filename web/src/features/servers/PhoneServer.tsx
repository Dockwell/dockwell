// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ActorKind, Connection, EventKind, type Server } from "@dockwell/api";
import { Icon, Metric, StatusBadge, Timeline, type TimelineEvent } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";

import { formatFixed, formatLiveBytesOf } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useDescribe } from "../activity/describe";
import { useEvents } from "../activity/useEvents";
import { appState, nameOf, revisionName } from "../apps/app";
import { useHealth } from "../apps/health";
import { useServices } from "../apps/useServices";
import { publicAddressOf } from "./network";
import { isPrivateAddress, serverStatus, useServerWords } from "./server";
import { useControlPlaneNow, useFleetView } from "./useFleet";
import "./PhoneServer.css";

/** Past this share, CPU or memory reads as a warning: the state the mock gives it. */
const HIGH = 90;

/** How many recent events the page shows. */
const RECENT = 3;

/**
 * A server on a phone, as M-Server.dc.html has it: how it is, in a word and a
 * line, its CPU and memory, its addresses to copy, what runs on it, and what
 * happened lately. The rest of its Overview follows, for whoever scrolls.
 */
export function PhoneServer({ server }: { server: Server }) {
  return (
    <>
      <PhoneStatus server={server} />
      <PhoneRunsHere server={server} />
      <PhoneRecent server={server} />
    </>
  );
}

function share(part: bigint, whole: bigint): number {
  return whole > 0n ? (Number(part) / Number(whole)) * 100 : 0;
}

/** How the server is: its state in a word, a line on what matters most, its CPU and memory, and its addresses. */
function PhoneStatus({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const { uptime } = useServerWords(server);
  const locale = i18n.locale;
  const stats = server.connection === Connection.CONNECTED ? server.stats : undefined;
  const cpu = stats?.cpuPercent ?? 0;
  const memory = stats ? share(stats.memoryUsedBytes, stats.memoryTotalBytes) : 0;
  const name = server.name;

  let line: string;
  if (server.connection === Connection.UNREACHABLE) {
    const seen = server.lastSeenTime ? words.ago(timestampDate(server.lastSeenTime), now) : "";
    line = seen ? t`${name} isn't reporting. It last did ${seen}.` : t`${name} isn't reporting.`;
  } else if (server.connection === Connection.PENDING) {
    line = t`${name} is waiting for its agent.`;
  } else if (server.dockerUnreadableSince) {
    const since = words.ago(timestampDate(server.dockerUnreadableSince), now);
    line = t`Docker hasn't answered since ${since}.`;
  } else if (memory >= HIGH) {
    line = t`Memory above ${HIGH}%`;
  } else if (cpu >= HIGH) {
    line = t`CPU above ${HIGH}%`;
  } else {
    line = uptime ? t`Up ${uptime}` : t`Reporting`;
  }

  const [used, total] = stats ? formatLiveBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, locale) : ["–", ""];
  const cpuValue = stats ? formatFixed(cpu, 0, locale) : "–";
  const publicIp = publicAddressOf(server);
  const privateIp = server.inventory?.addresses.find(isPrivateAddress);

  return (
    <section className="dw-panel phone-status" aria-label={t`Status`}>
      <div className="phone-status-head">
        <div>
          <StatusBadge status={serverStatus(server)} variant="pill" />
        </div>
        <span className="phone-status-line">{line}</span>
      </div>
      <div className="phone-status-grid">
        <div className="phone-status-cell">
          <Metric label={t`CPU`} value={cpuValue} unit="%" />
          <Meter label={t`CPU`} share={cpu} text={`${cpuValue}%`} />
        </div>
        <div className="phone-status-cell">
          <Metric label={t`Memory`} value={used} unit={total ? t`of ${total}` : undefined} />
          <Meter label={t`Memory`} share={memory} text={total ? t`${used} of ${total}` : "–"} />
        </div>
      </div>
      <div className="phone-status-grid">
        <CopyCell label={t`Public IP`} address={publicIp} />
        <CopyCell label={t`Private IP`} address={privateIp} />
      </div>
    </section>
  );
}

function Meter({ label, share: value, text }: { label: string; share: number; text: string }) {
  const high = value >= HIGH;
  return (
    // biome-ignore lint/a11y/useSemanticElements: a <meter> takes no styling that reads the same in both themes
    <div
      className="phone-meter"
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(value)}
      aria-valuetext={text}
    >
      <span className={high ? "is-high" : undefined} style={{ width: `${Math.min(100, Math.max(0, value))}%` }} />
    </div>
  );
}

/** Half a row that copies an address, the whole of it a target for a thumb. */
function CopyCell({ label, address }: { label: string; address: string | undefined }) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  if (!address) {
    return (
      <div className="phone-copy is-empty">
        <span className="phone-copy-text">
          <span className="phone-copy-label">{label}</span>
          <span className="phone-copy-none">{t`Not known`}</span>
        </span>
      </div>
    );
  }
  return (
    <button
      type="button"
      className="phone-copy"
      aria-label={copied ? t`Copied ${address}` : t`Copy ${label} ${address}`}
      onClick={() => {
        void navigator.clipboard.writeText(address).then(
          () => setCopied(true),
          () => {},
        );
      }}
    >
      <span className="phone-copy-text">
        <span className="phone-copy-label">{label}</span>
        <code className="phone-copy-value">{address}</code>
      </span>
      <Icon name={copied ? "check" : "copy"} size={16} />
    </button>
  );
}

/** The apps on the server, each with how many copies run here, its revision and how it is. */
function PhoneRunsHere({ server }: { server: Server }) {
  const { t } = useLingui();
  const services = useServices();
  const health = useHealth();
  const { servers } = useFleetView();
  const here = useMemo(
    () => (services.data ?? []).filter((s) => s.deployments.some((d) => d.serverId === server.id)),
    [services.data, server.id],
  );
  const rows = here.map((service) => {
    const read = health.of(service);
    const on = read.servers.find((s) => s.serverId === server.id);
    const record = appState(service, servers);
    const moving = record.kind === "deploying" || record.kind === "waiting";
    const failed = record.kind === "failed" || record.kind === "rollback-failed";
    const status = moving ? "deploying" : failed ? "down" : (on?.status ?? read.status);
    return { service, replicas: on?.containers ?? 0, status };
  });
  const replicas = rows.reduce((n, r) => n + r.replicas, 0);
  return (
    <section className="phone-section" aria-labelledby="phone-runs">
      <div className="phone-section-head">
        <h2 className="dw-text-heading" id="phone-runs">
          {t`Runs here`}
        </h2>
        {here.length > 0 ? (
          <span className="phone-section-meta">
            {plural(here.length, { one: "# app", other: "# apps" })}
            {replicas > 0 ? ` · ${plural(replicas, { one: "# replica", other: "# replicas" })}` : ""}
          </span>
        ) : null}
      </div>
      {services.isPending ? (
        <div className="dw-panel phone-list" aria-busy="true">
          <div className="phone-row">
            <span className="phone-skeleton" />
          </div>
        </div>
      ) : here.length === 0 ? (
        <div className="dw-panel phone-list">
          <Link to="/apps/new" className="phone-row phone-empty">
            <Icon name="plus" size={16} />
            <Trans>No apps here yet. Deploy one from an image or the catalog.</Trans>
          </Link>
        </div>
      ) : (
        <ul className="dw-panel phone-list">
          {rows.map(({ service, replicas: count, status }) => (
            <li key={service.id}>
              <Link to="/apps/$serviceId" params={{ serviceId: service.id }} className="phone-row">
                <Icon name="service" size={16} />
                <span className="phone-row-name">{nameOf(service)}</span>
                {count > 0 ? <span className="phone-row-count">×{count}</span> : null}
                <code className="phone-row-rev">{revisionName(service.revision)}</code>
                <span className="phone-row-spacer" />
                <StatusBadge status={status} />
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** What happened on the server lately, newest first, and the way to all of it. */
function PhoneRecent({ server }: { server: Server }) {
  const { t } = useLingui();
  const describe = useDescribe();
  const time = useTimeWords();
  const now = useControlPlaneNow();
  const filter = useMemo(
    () => ({ kinds: [], serverId: server.id, resourceId: "", actorId: "", actorKind: ActorKind.UNSPECIFIED }),
    [server.id],
  );
  const { state } = useEvents(filter);
  const events = state.status === "connecting" ? undefined : state.events.slice(0, RECENT);
  const summaries = useMemo(() => new Map<string, string>(), []);
  const items: TimelineEvent[] = (events ?? []).map((event) => {
    const d = describe(event, summaries);
    const at = event.time ? timestampDate(event.time) : new Date(now);
    return {
      id: event.id,
      kind: event.change
        ? event.change.reverts
          ? "rollback"
          : "plan"
        : event.kind === EventKind.SERVER
          ? "server"
          : "config",
      icon: d.icon,
      ...(d.tone ? { tone: d.tone } : {}),
      title: d.title,
      actor: event.actor?.name ?? "",
      time: time.ago(at, now),
      datetime: at.toISOString(),
    };
  });
  return (
    <section className="phone-section" aria-labelledby="phone-recent">
      <div className="phone-section-head">
        <h2 className="dw-text-heading" id="phone-recent">
          {t`Recent`}
        </h2>
        <Link to="/activity" search={{ server: server.id }} className="phone-section-link">
          <Trans>All activity</Trans>
        </Link>
      </div>
      {events === undefined ? (
        <p className="phone-note" aria-busy="true">
          <Trans>Reading what happened here</Trans>
        </p>
      ) : items.length === 0 ? (
        <p className="phone-note">{t`Nothing has happened here yet.`}</p>
      ) : (
        <Timeline events={items} />
      )}
    </section>
  );
}
