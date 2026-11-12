// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ActorKind, Connection, DeploymentState, EventKind, type Server, ServerStrain } from "@dockwell/api";
import { Icon, Metric, type Status, StatusBadge, Timeline, type TimelineEvent } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { formatClock, formatFixed, formatLiveBytesOf } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useDescribe } from "../activity/describe";
import { useEvents } from "../activity/useEvents";
import { nameOf, revisionName } from "../apps/app";
import { useHealth } from "../apps/health";
import { useApps } from "../apps/useServices";
import { CopyButton } from "../system/CopyCommand";
import { useCopy } from "./CopyButton";
import { serviceCommands } from "./commands";
import { forwardToOf, isSharedAddress, publicAddressOf } from "./network";
import { HIGH_USE as HIGH, isPrivateAddress, serverStatus, useServerWords } from "./server";
import { useControlPlaneNow } from "./useFleet";
import "./PhoneServer.css";

/** How many recent events the page shows. */
const RECENT = 3;

/**
 * A server on a phone, as M-Server.dc.html has it: how it is, in a word and a
 * line, its CPU and memory, its addresses to copy, what runs on it, and what
 * happened lately. The rest of its Overview follows, for whoever scrolls.
 */
export function PhoneServer({ server, heard }: { server: Server; heard?: Date | undefined }) {
  const unread = heard !== undefined;
  return (
    <>
      <PhoneStatus server={server} unread={unread} heard={heard} />
      <PhoneRunsHere server={server} />
      <PhoneRecent server={server} />
    </>
  );
}

function share(part: bigint, whole: bigint): number {
  return whole > 0n ? (Number(part) / Number(whole)) * 100 : 0;
}

/**
 * How the server is: its state in a word, a line on what matters most, what to do
 * when that's something, its CPU and memory, and its addresses. While the panel
 * can't hear the control plane, it says it can't say, as the rest of the page does.
 */
function PhoneStatus({ server, unread, heard }: { server: Server; unread: boolean; heard: Date | undefined }) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const { uptime } = useServerWords(server);
  const locale = i18n.locale;
  const stats = server.connection === Connection.CONNECTED ? server.stats : undefined;
  const cpu = stats?.cpuPercent ?? 0;
  const memory = stats ? share(stats.memoryUsedBytes, stats.memoryTotalBytes) : 0;
  const lasted = (since: Parameters<typeof timestampDate>[0]) =>
    words.span(Math.max(0, now - timestampDate(since).getTime()));

  // The line is a phrase, as the mock's is; what to do, when there's something, the command to copy.
  const commands = serviceCommands(server);
  let status: Status = serverStatus(server);
  let line: string;
  let fix: ReactNode = null;
  if (unread) {
    status = "unknown";
    line = t`Can't say how it is now`;
  } else if (server.connection === Connection.UNREACHABLE) {
    line = server.lastSeenTime ? t`Not reporting for ${lasted(server.lastSeenTime)}` : t`Not reporting`;
    fix = (
      <CopyButton
        text={commands.restartAgent}
        label={t`Copy the restart command`}
        copiedLabel={t`Restart command copied`}
        variant="secondary"
      />
    );
  } else if (server.connection === Connection.PENDING) {
    line = t`Waiting for its agent`;
    fix = (
      <CopyButton
        text={commands.startAgent}
        label={t`Copy the start command`}
        copiedLabel={t`Start command copied`}
        variant="secondary"
      />
    );
  } else if (server.dockerUnreadableSince) {
    line = t`Docker hasn't answered for ${lasted(server.dockerUnreadableSince)}`;
    fix = (
      <CopyButton
        text={commands.restartDocker}
        label={t`Copy the command that restarts Docker`}
        copiedLabel={t`Command copied`}
        variant="secondary"
      />
    );
  } else if (server.strainedSince) {
    // The control plane's, once it has lasted a minute, as the mock says it.
    const since = formatClock(timestampDate(server.strainedSince), locale);
    line =
      server.strain === ServerStrain.CPU
        ? t`CPU above ${HIGH}% since ${since}`
        : t`Memory above ${HIGH}% since ${since}`;
  } else {
    line = uptime ? t`Up ${uptime}` : t`Reporting`;
  }

  const [used, total] = stats ? formatLiveBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, locale) : ["–", ""];
  // Down, never up, so 89.6 doesn't read as 90 beside Healthy.
  const cpuValue = stats ? formatFixed(Math.floor(cpu), 0, locale) : "–";
  // What a meter last heard says how long ago, once it can't be taken as now.
  const stale = (text: string) => (heard ? t`${text}, as last heard ${words.ago(heard, now)}` : text);
  const publicIp = publicAddressOf(server);
  const privateIp = privateAddressOf(server);

  return (
    <section className="dw-panel phone-status" aria-label={t`Status`}>
      <div className="phone-status-head">
        <div>
          <StatusBadge
            status={status}
            variant="pill"
            label={server.connection === Connection.PENDING ? t`Waiting` : undefined}
            labelIsState={server.connection === Connection.PENDING}
          />
        </div>
        <span className="phone-status-line">{line}</span>
        {fix ? <div className="phone-status-fix">{fix}</div> : null}
      </div>
      {/* While nothing can say how it is, what it last said is greyed, not shown as now. */}
      <div className={unread ? "phone-status-grid is-stale" : "phone-status-grid"} aria-disabled={unread || undefined}>
        <div className="phone-status-cell">
          <Metric label={t`CPU`} value={cpuValue} unit="%" />
          <Meter label={t`CPU`} share={cpu} text={stale(`${cpuValue}%`)} />
        </div>
        <div className="phone-status-cell">
          <Metric label={t`Memory`} value={used} unit={total ? t`of ${total}` : undefined} />
          <Meter label={t`Memory`} share={memory} text={stale(total ? t`${used} of ${total}` : "–")} />
        </div>
      </div>
      <div className="phone-status-grid">
        <CopyCell label={t`Public IP`} address={publicIp} none={t`Not known`} />
        <CopyCell
          label={t`Private IP`}
          address={privateIp === "unknown" ? undefined : privateIp}
          none={privateIp === "unknown" ? t`Not known` : t`None`}
        />
      </div>
    </section>
  );
}

function Meter({ label, share: value, text }: { label: string; share: number; text: string }) {
  const high = value > HIGH;
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

/**
 * Half a row that copies an address, the whole of it a target for a thumb. A
 * browser that won't copy, as one on plain HTTP has no clipboard, leaves the
 * address on screen to select.
 */
function CopyCell({ label, address, none }: { label: string; address: string | undefined; none: string }) {
  const { t } = useLingui();
  const { copied, copy } = useCopy(address ?? "");
  if (!address) {
    return (
      <div className="phone-copy is-empty">
        <span className="phone-copy-text">
          <span className="phone-copy-label">{label}</span>
          <span className="phone-copy-none">{none}</span>
        </span>
      </div>
    );
  }
  return (
    <>
      <button
        type="button"
        className="phone-copy"
        aria-label={copied ? t`Copied ${address}` : t`Copy ${label} ${address}`}
        onClick={copy}
      >
        <span className="phone-copy-text">
          <span className="phone-copy-label">{label}</span>
          <code className="phone-copy-value">{address}</code>
        </span>
        <Icon name={copied ? "check" : "copy"} size={16} />
      </button>
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Copied ${address}` : ""}
      </span>
    </>
  );
}

/** The apps on the server, each with how many copies run here, its revision and how it is. */
function PhoneRunsHere({ server }: { server: Server }) {
  const { t } = useLingui();
  // Apps as Apps lists them: a website's own services are on Websites (ADR 0039).
  const services = useApps();
  const health = useHealth();
  const here = useMemo(
    () => (services.data ?? []).filter((s) => s.deployments.some((d) => d.serverId === server.id)),
    [services.data, server.id],
  );
  const rows = here.map((service) => {
    const read = health.of(service);
    const on = read.servers.find((s) => s.serverId === server.id);
    // How its deployment went here, not on another server, then how it runs here.
    const state = service.deployments.find((d) => d.serverId === server.id)?.state;
    const failed = state === DeploymentState.FAILED;
    const status: Status =
      state === DeploymentState.PENDING ? "deploying" : failed ? "down" : (on?.status ?? read.status);
    return { service, replicas: on?.containers ?? 0, status, failed };
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
          {rows.map(({ service, replicas: count, status, failed }) => (
            <li key={service.id}>
              <Link to="/apps/$serviceId" params={{ serviceId: service.id }} className="phone-row">
                <Icon name="service" size={16} />
                <span className="phone-row-name">{nameOf(service)}</span>
                {count > 0 ? <span className="phone-row-count">×{count}</span> : null}
                <code className="phone-row-rev">{revisionName(service.revision)}</code>
                <span className="phone-row-spacer" />
                <StatusBadge status={status} {...(failed ? { label: t`Failed`, labelIsState: true } : {})} />
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

/**
 * A server's private address: the one its route out leaves from, when that's
 * private, as a home server's is; or the first private one on its physical
 * interfaces, as a cloud's private network gives one. An agent from before it
 * listed only those lists Docker's bridges too, so its list says nothing here.
 * "unknown" for one whose agent hasn't said how it goes out: one still waiting,
 * or one from before it said.
 */
export function privateAddressOf(server: Server): string | "unknown" | undefined {
  const inv = server.inventory;
  if (!inv || (inv.outboundAddress === "" && !inv.addressesPhysical)) return "unknown";
  const forward = forwardToOf(server);
  if (forward || !inv.addressesPhysical) return forward;
  return inv.addresses.find((ip) => !ip.includes(":") && isPrivateAddress(ip) && !isSharedAddress(ip));
}
