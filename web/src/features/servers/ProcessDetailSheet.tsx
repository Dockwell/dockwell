// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type Process, ProcessService, type Server, type SocketConnection } from "@dockwell/api";
import { Button, Callout, Icon, Loading, Sheet, SkeletonFacts, SkeletonRows, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useServices } from "../apps/useServices";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { CopyButton } from "./CopyButton";
import { ownerOf } from "./processes";
import { useControlPlaneNow } from "./useFleet";

export interface ProcessTarget {
  pid: number;
  startTicks?: bigint | undefined;
  /** Optional snapshot from the live list so the header renders immediately. */
  process?: Process | undefined;
}

export function formatEndpoint(address: string, port: number): string {
  const host = address.includes(":") ? `[${address}]` : address;
  return `${host}:${port}`;
}

/**
 * Inspects one process: its full secret-redacted command line, parent, user,
 * start time, threads, open sockets and container or service, with Stop as on
 * the process list. Never displays environment variables.
 */
export function ProcessDetailSheet({
  server,
  target,
  onSelectParent,
  onEnd,
  onClose,
}: {
  server: Server;
  target: ProcessTarget;
  onSelectParent: (pid: number) => void;
  onEnd: (process: Process) => void;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const services = useServices().data ?? [];

  const query = useQuery(
    ProcessService.method.getProcess,
    {
      serverId: server.id,
      pid: target.pid,
      startTicks: target.startTicks ?? 0n,
    },
    { retry: false, refetchOnWindowFocus: false },
  );

  const detail = query.data?.process;
  const proc = detail?.process ?? target.process;
  const name = proc?.name || t`Process ${target.pid}`;
  const owner = proc ? ownerOf(proc, services) : { kind: "none" as const };
  const cmd = detail?.fullCommand || proc?.command || proc?.name || "";
  const threads = detail?.threads || proc?.threads || 0;
  const started = proc?.startTime ? timestampDate(proc.startTime) : undefined;
  const uptime = proc?.startTime ? words.span(Math.max(0, now - timestampMs(proc.startTime))) : undefined;
  const sockets: readonly SocketConnection[] = detail?.sockets ?? [];
  const socketItems = (() => {
    const seen = new Map<string, number>();
    return sockets.map((s) => {
      const base = `${s.protocol}-${s.localAddress}-${s.localPort}-${s.remoteAddress}-${s.remotePort}-${s.state}`;
      const n = (seen.get(base) ?? 0) + 1;
      seen.set(base, n);
      return { id: n === 1 ? base : `${base}#${n}`, socket: s };
    });
  })();

  const protectedWhy = (why: string) => {
    switch (why) {
      case "init":
        return t`PID 1 starts and stops everything else. Restart the server instead.`;
      case "ssh":
        return t`This SSH server takes new logins, a way back onto the server. Restart it with systemctl restart ssh instead.`;
      default:
        return t`Dockwell needs this process, or runs under it. Restart Dockwell instead.`;
    }
  };

  return (
    <Sheet
      open
      title={name}
      subtitle={t`PID ${target.pid} on ${server.name}`}
      onClose={onClose}
      footer={
        <>
          {proc?.protected ? (
            <span className="procs-protected" title={protectedWhy(proc.protected)}>
              <Icon name="lock" size={13} />
              <Trans>Protected</Trans>
            </span>
          ) : null}
          <span className="procs-spacer" />
          <Button variant="ghost" onClick={onClose}>
            <Trans>Close</Trans>
          </Button>
          {proc && !proc.protected ? (
            <Button
              variant="danger"
              icon="stop"
              onClick={() => {
                onClose();
                onEnd(proc);
              }}
            >
              <Trans>Stop</Trans>
            </Button>
          ) : null}
        </>
      }
    >
      <div className="procs-detail">
        {query.error ? (
          isAgentOutdated(query.error) ? (
            <AgentOutdated server={server}>
              {t`It's older than the control plane, so it can't inspect process details.`}
            </AgentOutdated>
          ) : (
            <Callout tone="warning" title={t`Process couldn't be inspected`}>
              {sentence(query.error)}
            </Callout>
          )
        ) : null}

        {query.isPending && !proc ? (
          <Loading label={t`Reading process details`}>
            <SkeletonFacts count={6} layout="rows" />
          </Loading>
        ) : (
          <>
            <section className="procs-detail-section" aria-label={t`Command line`}>
              <div className="procs-detail-head">
                <h3 className="procs-detail-label">
                  <Trans>Command line</Trans>
                </h3>
                {cmd ? <CopyButton text={cmd} label={t`Copy command line`} /> : null}
              </div>
              <pre className="procs-detail-cmd">
                <code>{cmd || "–"}</code>
              </pre>
              {detail?.commandTruncated ? (
                <p className="procs-note">
                  <Trans>Truncated at 16 KB; secrets are hidden and the environment is never read.</Trans>
                </p>
              ) : (
                <p className="procs-note">
                  <Trans>Secrets are hidden; the environment is never read.</Trans>
                </p>
              )}
            </section>

            <dl className="facts procs-detail-facts">
              <dt>{t`User`}</dt>
              <dd>
                <code>{proc?.user || "–"}</code>
                {proc ? <span className="procs-detail-muted">{` (UID ${proc.uid})`}</span> : null}
              </dd>

              <dt>{t`Parent`}</dt>
              <dd>
                {proc && proc.parentPid > 0 ? (
                  <button type="button" className="procs-parent-link" onClick={() => onSelectParent(proc.parentPid)}>
                    <code>
                      {detail?.parentName ? `${detail.parentName} (${proc.parentPid})` : `PID ${proc.parentPid}`}
                    </code>
                  </button>
                ) : (
                  <span>–</span>
                )}
              </dd>

              <dt>{t`Started`}</dt>
              <dd>
                {started ? (
                  <>
                    {formatDateTime(started, i18n.locale)}
                    {uptime ? <span className="procs-detail-muted">{` · ${uptime}`}</span> : null}
                  </>
                ) : (
                  "–"
                )}
              </dd>

              <dt>{t`Threads`}</dt>
              <dd>{threads > 0 ? threads : "–"}</dd>

              {detail?.state ? (
                <>
                  <dt>{t`State`}</dt>
                  <dd>{detail.state}</dd>
                </>
              ) : null}

              <dt>{t`Memory`}</dt>
              <dd>{proc ? formatBytes(proc.memoryBytes, i18n.locale) : "–"}</dd>

              {owner.kind === "app" ? (
                <>
                  <dt>{t`App`}</dt>
                  <dd>
                    <Link className="dw-link" to="/apps/$serviceId" params={{ serviceId: owner.id }}>
                      {owner.name}
                    </Link>
                  </dd>
                </>
              ) : null}

              {proc?.container ? (
                <>
                  <dt>{t`Container`}</dt>
                  <dd>
                    {proc.container.id ? (
                      <Link
                        className="dw-link"
                        to="/servers/$serverId/docker/containers/$containerId"
                        params={{ serverId: server.id, containerId: proc.container.id }}
                        search={proc.container.name ? { name: proc.container.name } : {}}
                      >
                        <code>{proc.container.name || proc.container.id.slice(0, 12)}</code>
                      </Link>
                    ) : (
                      <code>{proc.container.name}</code>
                    )}
                  </dd>
                </>
              ) : null}

              {proc?.unit ? (
                <>
                  <dt>{t`System service`}</dt>
                  <dd>
                    <code>{proc.unit}</code>
                  </dd>
                </>
              ) : null}
            </dl>

            <section className="procs-detail-section" aria-label={t`Open sockets`}>
              <div className="procs-detail-head">
                <h3 className="procs-detail-label">
                  <Trans>Open sockets</Trans>
                </h3>
                {detail && detail.socketsTotal > 0 ? (
                  <span className="procs-detail-muted">
                    {detail.socketsTruncated
                      ? t`${sockets.length} of ${detail.socketsTotal}`
                      : String(detail.socketsTotal)}
                  </span>
                ) : null}
              </div>
              {query.isPending ? (
                <Loading label={t`Reading open sockets`}>
                  <SkeletonRows rows={3} lines={1} end="value" dense />
                </Loading>
              ) : sockets.length === 0 ? (
                <p className="procs-note">{t`No open TCP or UDP sockets.`}</p>
              ) : (
                <ul className="procs-sockets">
                  {socketItems.map(({ id, socket: s }) => (
                    <li key={id} className="procs-socket-row">
                      <Tag>{s.protocol}</Tag>
                      <code className="procs-socket-ep">
                        {formatEndpoint(s.localAddress, s.localPort)}
                        {" -> "}
                        {formatEndpoint(s.remoteAddress, s.remotePort)}
                      </code>
                      <span className="procs-socket-state">{s.state}</span>
                    </li>
                  ))}
                </ul>
              )}
            </section>
          </>
        )}
      </div>
    </Sheet>
  );
}
