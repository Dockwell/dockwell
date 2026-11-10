// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { ActorKind, Connection, EventKind, type Server, ServerStrain } from "@dockwell/api";
import { Button, Icon, Metric, StatusBadge, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { formatBytes, formatClock, formatFixed, formatLiveBytesOf } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { EventTimeline } from "../activity/ActivityPage";
import { type EventFilter, useEvents } from "../activity/useEvents";
import { HealthBadge } from "../apps/Health";
import { useHealth } from "../apps/health";
import { useServices } from "../apps/useServices";
import { shortVersion } from "../shell/version";
import { trend } from "./history";
import { providerName, publicAddressOf } from "./network";
import { HIGH_USE as HIGH, isPrivateAddress, serverStatus } from "./server";
import { useControlPlaneNow } from "./useFleet";

/** How many of a server's events the inspector shows; its page and Activity have the rest. */
const RECENT = 3;

/** What happened on a server, as the inspector tells it: not who looked at its files. */
const HAPPENED: EventKind[] = [EventKind.CHANGE, EventKind.SERVER, EventKind.HEALTH, EventKind.BACKUP];

/**
 * One server, beside the list, as Servers-List inspects it: why it isn't well, where
 * it is, its CPU and memory over the last minutes, what runs on it and what happened
 * there lately, and the way to its page.
 */
export function ServerInspector({
  server,
  unknown = false,
  onClose,
}: {
  server: Server;
  /** What the fleet last said can't stand, so nobody can say how the server is now. */
  unknown?: boolean;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const status = unknown ? "unknown" : serverStatus(server);
  const name = server.name;
  const since = unknown ? undefined : unwellSince(server);
  const forSpan = (span: string) => t`for ${span}`;
  return (
    <aside className="dw-panel servers-inspector" aria-label={name}>
      <div className="dw-panel-head servers-inspector-head">
        <div className="servers-inspector-title">
          <Icon name="server" size={16} />
          <h2 className="dw-panel-title servers-inspector-name">{name}</h2>
          <StatusBadge
            status={status}
            variant="pill"
            meta={since !== undefined ? forSpan(words.span(Math.max(now - since, 0))) : undefined}
          />
        </div>
        <Button variant="ghost" size="sm" icon="x" aria-label={t`Close ${name}`} onClick={onClose} />
      </div>
      <Facts server={server} unknown={unknown} />
      {/* Its numbers stand only while what the fleet says does. */}
      {unknown ? null : <Numbers server={server} />}
      <RunsHere server={server} />
      <Recent server={server} />
      <div className="servers-inspector-actions">
        <RouterButtonLink to="/servers/$serverId" params={{ serverId: server.id }} size="sm" icon="arrow-right">
          {t`Open ${name}`}
        </RouterButtonLink>
        <RouterButtonLink to="/servers/$serverId/files" params={{ serverId: server.id }} size="sm" icon="folder">
          <Trans>Files</Trans>
        </RouterButtonLink>
      </div>
    </aside>
  );
}

/** Since when, by the control plane's clock, a server has been unwell; nothing while it's well. */
function unwellSince(server: Server): number | undefined {
  if (server.connection === Connection.UNREACHABLE && server.lastSeenTime) return timestampMs(server.lastSeenTime);
  if (server.connection === Connection.CONNECTED && server.dockerUnreadableSince) {
    return timestampMs(server.dockerUnreadableSince);
  }
  if (server.connection === Connection.CONNECTED && server.strainedSince) return timestampMs(server.strainedSince);
  return undefined;
}

/** Why it isn't well, then where it is and what it is. */
function Facts({ server, unknown }: { server: Server; unknown: boolean }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(15_000);
  const inv = server.inventory;
  const seen = server.lastSeenTime ? words.ago(timestampDate(server.lastSeenTime), now) : undefined;
  const since = server.strainedSince ? formatClock(timestampDate(server.strainedSince), i18n.locale) : "";
  const strained =
    server.connection === Connection.CONNECTED && server.strainedSince
      ? server.strain === ServerStrain.CPU
        ? t`CPU above ${HIGH}% since ${since}`
        : t`Memory above ${HIGH}% since ${since}`
      : undefined;
  const why: ReactNode = unknown ? undefined : server.connection === Connection.UNREACHABLE ? (
    seen ? (
      <Trans>
        Its agent last reported {seen}. Check that <code>dockwell run</code> is still running on the server.
      </Trans>
    ) : (
      <Trans>
        Its agent has disconnected. Check that <code>dockwell run</code> is still running on the server.
      </Trans>
    )
  ) : server.connection === Connection.PENDING ? (
    <Trans>
      It joined, but its agent hasn't connected yet. Start it with <code>dockwell run</code> on the server.
    </Trans>
  ) : server.dockerUnreadableSince ? (
    <Trans>Its agent is connected, but Docker doesn't answer, so nobody can say how what runs there is.</Trans>
  ) : strained ? (
    // Why it reads Degraded, as the control plane says once it has lasted, in its own page's words.
    strained
  ) : undefined;
  const addresses = inv?.addresses ?? [];
  const publicIp = publicAddressOf(server) ?? addresses.find((ip) => !isPrivateAddress(ip));
  const privateIp = addresses.find((ip) => isPrivateAddress(ip) && !ip.includes(":"));
  const provider = providerName(server.network?.provider);
  const cpus = inv?.cpuCount ?? 0;
  const size = [
    cpus > 0 ? t`${cpus} vCPU` : "",
    inv && inv.memoryBytes > 0n ? formatBytes(inv.memoryBytes, i18n.locale) : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const version = server.agentBuild?.version;
  const facts: [string, ReactNode][] = [];
  if (publicIp) facts.push([t`Public IP`, <code key="public">{publicIp}</code>]);
  if (privateIp) facts.push([t`Private IP`, <code key="private">{privateIp}</code>]);
  if (provider) facts.push([t`Provider`, provider]);
  if (inv?.osName) facts.push([t`System`, inv.osName]);
  if (size) facts.push([t({ message: "Size", context: "server" }), size]);
  if (version) {
    facts.push([
      t`Agent`,
      <span key="agent" className="servers-inspector-agent">
        {shortVersion(version)}
        {server.controlServer ? <Tag>{t`Control server`}</Tag> : null}
      </span>,
    ]);
  }
  return (
    <div className="servers-inspector-section">
      {why ? <p className="servers-inspector-why">{why}</p> : null}
      {facts.length > 0 ? (
        <dl className="servers-inspector-facts">
          {facts.map(([term, value]) => (
            <div key={term}>
              <dt>{term}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      ) : null}
    </div>
  );
}

/** Its CPU and memory now, with their trend since the agent last stopped reporting. */
function Numbers({ server }: { server: Server }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const stats = server.connection === Connection.CONNECTED ? server.stats : undefined;
  if (!stats) return null;
  const { samples, spanMs } = trend(server.history.length > 0 ? server.history : [stats]);
  const share = (used: bigint, total: bigint) => (total > 0n ? (Number(used) / Number(total)) * 100 : 0);
  const [used, total] = formatLiveBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, i18n.locale);
  const span = words.span(spanMs);
  const caption = spanMs > 0 ? t`Last ${span}` : undefined;
  return (
    <div className="servers-inspector-section servers-inspector-numbers">
      <Metric
        label={t`CPU`}
        value={formatFixed(stats.cpuPercent, 0, i18n.locale)}
        unit="%"
        data={samples.map((s) => s.cpuPercent)}
        min={0}
        max={100}
        caption={caption}
      />
      <Metric
        label={t`Memory`}
        value={used}
        unit={t`of ${total}`}
        data={samples.map((s) => share(s.memoryUsedBytes, s.memoryTotalBytes))}
        min={0}
        max={100}
        caption={caption}
      />
    </div>
  );
}

/** The apps with a deployment on this server, each with how it runs, opening its page. */
function RunsHere({ server }: { server: Server }) {
  const { t } = useLingui();
  const services = useServices();
  const health = useHealth();
  const here = useMemo(
    () =>
      (services.data ?? [])
        .filter((s) => s.deployments.some((d) => d.serverId === server.id))
        .sort((a, b) => (a.resource?.name ?? "").localeCompare(b.resource?.name ?? "")),
    [services.data, server.id],
  );
  if (services.isPending) return null;
  return (
    <section className="servers-inspector-section" aria-label={t`Runs here`}>
      <h3 className="servers-inspector-label">
        <Trans>Runs here</Trans>
      </h3>
      {here.length === 0 ? (
        <p className="servers-inspector-quiet">
          <Trans>No apps run here.</Trans>
        </p>
      ) : (
        <ul className="servers-inspector-apps">
          {here.map((service) => (
            <li key={service.id}>
              <Icon name="service" size={16} />
              <Link to="/apps/$serviceId" params={{ serviceId: service.id }} className="servers-inspector-app">
                {service.resource?.name ?? service.id}
              </Link>
              <code className="servers-inspector-rev">r{String(service.revision)}</code>
              <HealthBadge health={health.of(service)} className="servers-inspector-health" />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** The newest of what happened on this server, as Activity draws it. */
function Recent({ server }: { server: Server }) {
  const { t } = useLingui();
  const now = useControlPlaneNow(15_000);
  const filter = useMemo<EventFilter>(
    () => ({ kinds: HAPPENED, serverId: server.id, resourceId: "", actorId: "", actorKind: ActorKind.UNSPECIFIED }),
    [server.id],
  );
  const { state } = useEvents(filter);
  const events = state.status === "connecting" ? undefined : state.events.slice(0, RECENT);
  const summaries = useMemo(() => {
    const out = new Map<string, string>();
    for (const e of events ?? []) if (e.change) out.set(e.id, e.change.summary);
    return out;
  }, [events]);
  if (events === undefined) return null;
  return (
    <section className="servers-inspector-section" aria-label={t`Recent`}>
      <h3 className="servers-inspector-label">
        <Trans>Recent</Trans>
      </h3>
      {events.length === 0 ? (
        <p className="servers-inspector-quiet">
          <Trans>Nothing has happened here yet.</Trans>
        </p>
      ) : (
        <EventTimeline events={events} summaries={summaries} now={now} framed={false} />
      )}
    </section>
  );
}
