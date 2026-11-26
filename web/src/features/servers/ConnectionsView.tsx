// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { Connection, ProcessService, type Server, type SocketConnection } from "@dockwell/api";
import {
  Button,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  SkeletonRows,
  Table,
  type TableColumn,
  Tag,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { formatEndpoint, type ProcessTarget } from "./ProcessDetailSheet";

const REFRESH_MS = 15_000;
const PAGE = 50;

export function matchesSocket(conn: SocketConnection, query: string, protocol: string, state: string): boolean {
  if (protocol && !conn.protocol.startsWith(protocol)) return false;
  if (state && conn.state !== state) return false;
  const q = query.trim().toLowerCase();
  if (!q) return true;
  const fields = [
    conn.protocol,
    conn.localAddress,
    String(conn.localPort),
    `${conn.localAddress}:${conn.localPort}`,
    conn.remoteAddress,
    String(conn.remotePort),
    `${conn.remoteAddress}:${conn.remotePort}`,
    conn.state,
    conn.processName,
    conn.pid > 0 ? String(conn.pid) : "",
    conn.user,
    conn.container?.name ?? "",
    conn.unit,
  ];
  return fields.some((f) => f.toLowerCase().includes(q));
}

/**
 * Live network sockets on a server (as ss -tunap shows them), filterable by
 * port, address, process, protocol and state, with click-to-inspect process detail.
 */
export function ConnectionsView({
  server,
  filter,
  onSelectProcess,
}: {
  server: Server;
  filter: string;
  onSelectProcess: (target: ProcessTarget) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const connected = server.connection === Connection.CONNECTED;
  const [protoFilter, setProtoFilter] = useState("");
  const [stateFilter, setStateFilter] = useState("");
  const [shown, setShown] = useState(PAGE);

  const query = useQuery(
    ProcessService.method.listSockets,
    { serverId: server.id },
    {
      enabled: connected,
      retry: false,
      refetchInterval: REFRESH_MS,
      refetchIntervalInBackground: false,
      refetchOnWindowFocus: false,
    },
  );

  const all = query.data?.connections ?? [];
  const states = useMemo(() => {
    const set = new Set<string>();
    for (const c of all) {
      if (c.state) set.add(c.state);
    }
    return [...set].sort();
  }, [all]);

  const filtered = useMemo(
    () => all.filter((c) => matchesSocket(c, filter, protoFilter, stateFilter)),
    [all, filter, protoFilter, stateFilter],
  );

  if (!connected) {
    return (
      <p className="procs-note" role="status">
        {t`The agent on ${server.name} isn't connected; connections read again once it reconnects.`}
      </p>
    );
  }

  if (query.error) {
    return isAgentOutdated(query.error) ? (
      <AgentOutdated server={server}>
        {t`It's older than the control plane, so it can't list network connections.`}
      </AgentOutdated>
    ) : (
      <p className="procs-note">{sentence(query.error)}</p>
    );
  }

  if (query.isPending && !query.data) {
    return (
      <Loading label={t`Reading network connections`} slow={t`${server.name} is slow to list its sockets`}>
        <Panel flush>
          <SkeletonRows rows={8} end="value" dense />
        </Panel>
      </Loading>
    );
  }

  const rows = filtered.slice(0, shown).map((c, idx) => ({
    ...c,
    rowId: `${c.protocol}-${c.localAddress}-${c.localPort}-${c.remoteAddress}-${c.remotePort}-${c.state}-${c.pid}-${idx}`,
  }));

  const ownerText = (c: SocketConnection) => {
    if (c.container?.name) return t`Container ${c.container.name}`;
    if (c.unit) return t`Service ${c.unit.replace(/\.service$/, "")}`;
    return c.user || "";
  };

  const columns: TableColumn<(typeof rows)[number]>[] = [
    {
      key: "protocol",
      header: t`Protocol`,
      width: 78,
      render: (c) => <Tag>{c.protocol}</Tag>,
    },
    {
      key: "local",
      header: t`Local address`,
      mono: true,
      render: (c) => formatEndpoint(c.localAddress, c.localPort),
    },
    {
      key: "remote",
      header: t`Remote address`,
      mono: true,
      muted: true,
      render: (c) => formatEndpoint(c.remoteAddress, c.remotePort),
    },
    {
      key: "state",
      header: t`State`,
      width: 120,
      mono: true,
      render: (c) => <span className="procs-conn-state">{c.state}</span>,
    },
    {
      key: "process",
      header: t`Process`,
      render: (c) => {
        if (!c.pid) return <span className="procs-sub">–</span>;
        const sub = ownerText(c);
        return (
          <button
            type="button"
            className="procs-link-cell"
            onClick={() => onSelectProcess({ pid: c.pid, startTicks: c.startTicks })}
          >
            <span className="procs-name">
              {c.processName || `PID ${c.pid}`}
              <span className="procs-conn-pid">{` (${c.pid})`}</span>
            </span>
            {sub ? <span className="procs-sub">{sub}</span> : null}
          </button>
        );
      },
    },
  ];

  const isFiltered = filter.trim() !== "" || protoFilter !== "" || stateFilter !== "";

  return (
    <div className="procs-conns">
      <div className="procs-conn-filters">
        <SelectField
          size="sm"
          aria-label={t`Filter by protocol`}
          value={protoFilter}
          options={[
            { value: "", label: t`All protocols` },
            { value: "tcp", label: "TCP" },
            { value: "udp", label: "UDP" },
          ]}
          onChange={(e) => {
            setProtoFilter(e.target.value);
            setShown(PAGE);
          }}
        />
        <SelectField
          size="sm"
          aria-label={t`Filter by state`}
          value={stateFilter}
          options={[{ value: "", label: t`All states` }, ...states.map((s) => ({ value: s, label: s }))]}
          onChange={(e) => {
            setStateFilter(e.target.value);
            setShown(PAGE);
          }}
        />
        <span className="procs-spacer" />
        <Button size="sm" variant="ghost" icon="refresh" onClick={() => void query.refetch()}>
          <Trans>Refresh</Trans>
        </Button>
      </div>

      {phone ? (
        rows.length === 0 ? (
          <p className="procs-note">{isFiltered ? t`No connections match.` : t`No open network sockets.`}</p>
        ) : (
          <ul className="procs-rows" aria-label={t`Network connections`}>
            {rows.map((c) => {
              const sub = ownerText(c);
              return (
                <li key={c.rowId} className="procs-conn-phone">
                  <div className="procs-conn-phone-top">
                    <Tag>{c.protocol}</Tag>
                    <span className="procs-conn-state">{c.state}</span>
                    <span className="procs-spacer" />
                    {c.pid > 0 ? (
                      <button
                        type="button"
                        className="procs-parent-link"
                        onClick={() => onSelectProcess({ pid: c.pid, startTicks: c.startTicks })}
                      >
                        <code>{`${c.processName || "pid"} (${c.pid})`}</code>
                      </button>
                    ) : null}
                  </div>
                  <code className="procs-socket-ep">
                    {formatEndpoint(c.localAddress, c.localPort)}
                    {" -> "}
                    {formatEndpoint(c.remoteAddress, c.remotePort)}
                  </code>
                  {sub ? <span className="procs-sub">{sub}</span> : null}
                </li>
              );
            })}
          </ul>
        )
      ) : (
        <Table
          className="procs-table procs-conn-table"
          columns={columns}
          rows={rows}
          rowKey="rowId"
          density="compact"
          caption={t`Connections on ${server.name}`}
          empty={isFiltered ? t`No connections match.` : t`No open network sockets.`}
        />
      )}

      {filtered.length > shown ? (
        <div className="procs-more">
          <Button size="sm" variant="secondary" onClick={() => setShown((n) => n + PAGE)}>
            {plural(Math.min(PAGE, filtered.length - shown), { one: "Show # more", other: "Show # more" })}
          </Button>
        </div>
      ) : null}

      <p className="procs-note">
        {isFiltered
          ? plural(filtered.length, { one: "# connection matches.", other: "# connections match." })
          : query.data?.truncated
            ? t`Showing ${rows.length} of ${query.data.total} sockets (bounded at 1,000).`
            : plural(all.length, { one: "# open socket.", other: "# open sockets." })}
      </p>
    </div>
  );
}
