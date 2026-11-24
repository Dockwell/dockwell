// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Process, ProcessService, type Server } from "@dockwell/api";
import {
  Button,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatFixed, formatLiveBytesOf } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import {
  ListPager,
  type ListPlace,
  type ListRouteProps,
  ListSearchField,
  sortableColumns,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { pagedRows, sortedRows } from "../shell/list";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { ConnectionsView } from "./ConnectionsView";
import { EndProcessSheet } from "./EndProcessSheet";
import { ProcessDetailSheet, type ProcessTarget } from "./ProcessDetailSheet";
import {
  FIRST_WAY,
  groupProcesses,
  matchesProcess,
  type Owner,
  ownerOf,
  type ProcessGroup,
  type Sort,
  type SortKey,
  sortProcesses,
  useProcesses,
} from "./processes";
import { ServerFrame } from "./ServerFrame";
import { useControlPlaneNow } from "./useFleet";
// The panels' meta lines and the stop sheet's facts are ServerPanels.css's, which a visit
// straight to this tab hasn't loaded otherwise.
import "./ServerPanels.css";
import "./ProcessesPage.css";

/** The number of processes on each page. */
const PAGE = 20;
const SORT_KEYS = Object.keys(FIRST_WAY) as SortKey[];
const GROUP_SORT_KEYS = ["name", "count", "cpu", "memory"] as const;
/** How many units the Services panel names. */
const UNITS = 7;

type View = "processes" | "connections" | "services" | "containers";

/** A server's processes, as Server-Processes.dc.html has them: what runs, sorted and searchable, and ending one. */
export function ProcessesPage({ serverId, search, onSearch }: { serverId: string } & ListRouteProps) {
  return (
    <ServerFrame id={serverId}>
      {(server) => <Processes server={server} search={search} onSearch={onSearch} />}
    </ServerFrame>
  );
}

function Processes({ server, search, onSearch }: { server: Server } & ListRouteProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const connected = server.connection === Connection.CONNECTED;
  const { list, error } = useProcesses(server.id, connected);
  const services = useServices().data ?? [];
  const [view, setView] = useState<View>("processes");
  const place = useListPlace(search, onSearch, view === "processes" ? SORT_KEYS : GROUP_SORT_KEYS);
  const filter = place.query;
  const sortKey = (place.sort?.key as SortKey) ?? "cpu";
  const descending = place.sort ? place.sort.dir === "desc" : true;
  const sort: Sort = { key: sortKey, descending };
  const [ending, setEnding] = useState<Process>();
  const [selected, setSelected] = useState<ProcessTarget>();
  const all = list?.processes ?? [];
  const measured = list?.cpuWindow !== undefined;

  const owners = useMemo(() => new Map(all.map((p) => [p, ownerOf(p, services)])), [all, services]);
  const found = useMemo(
    () =>
      sortProcesses(all, { key: sortKey, descending }).filter((p) =>
        matchesProcess(p, owners.get(p) ?? { kind: "none" }, filter),
      ),
    [all, sortKey, descending, owners, filter],
  );
  const page = pagedRows(found, place.page, PAGE);
  const units = useMemo(() => groupProcesses(all, services, "services"), [all, services]);
  const containers = useMemo(() => groupProcesses(all, services, "containers"), [all, services]);

  const views = [
    { value: "processes" as const, label: t`Processes`, count: list?.total ?? 0 },
    { value: "connections" as const, label: t`Connections` },
    { value: "services" as const, label: t`Services`, count: units.length },
    { value: "containers" as const, label: t`Containers`, count: containers.length },
  ];

  const failure = !error ? null : isAgentOutdated(error) ? (
    <AgentOutdated server={server}>{t`It's older than the control plane, so it can't list processes.`}</AgentOutdated>
  ) : (
    <p className="procs-note">{sentence(error)}</p>
  );

  const openDetail = (p: Process) => setSelected({ pid: p.pid, startTicks: p.startTicks, process: p });

  let body: ReactNode;
  if (view === "connections") {
    body = <ConnectionsView server={server} filter={filter} onSelectProcess={setSelected} />;
  } else if (!connected) {
    body = (
      <p className="procs-note" role="status">
        {t`The agent on ${server.name} isn't connected; this reads again once it reconnects.`}
      </p>
    );
  } else if (!list) {
    body =
      failure ??
      (view === "processes" && !phone ? (
        <ProcessTable
          rows={[]}
          owners={owners}
          measured={measured}
          sort={sort}
          onSort={() => {}}
          onSelect={openDetail}
          selectedPid={selected?.pid ?? null}
          onEnd={setEnding}
          caption={t`Processes on ${server.name}`}
          filtered={false}
          loading
          slow={t`${server.name} is slow to list what runs on it`}
        />
      ) : (
        <Loading label={t`Reading what runs here`} slow={t`${server.name} is slow to list what runs on it`}>
          <Panel flush>
            <SkeletonRows rows={8} end="value" dense />
          </Panel>
        </Loading>
      ));
  } else if (view === "processes") {
    const rows = page.rows;
    body = (
      <>
        {phone ? (
          <PhoneRows rows={rows} owners={owners} measured={measured} onSelect={openDetail} onEnd={setEnding} />
        ) : (
          <ProcessTable
            rows={rows}
            owners={owners}
            measured={measured}
            sort={sort}
            onSort={(key) => {
              const descending = key === sort.key ? !sort.descending : FIRST_WAY[key];
              onSearch({ ...search, q: place.typed || undefined, sort: descending ? `-${key}` : key, page: undefined });
            }}
            onSelect={openDetail}
            selectedPid={selected?.pid ?? null}
            onEnd={setEnding}
            caption={t`Processes on ${server.name}`}
            filtered={filter.trim() !== ""}
          />
        )}
        <ListPager page={page} onPage={place.setPage} />
        <p className="procs-note">
          <ShowingNote
            shown={found.length}
            matching={found.length}
            total={list.total}
            sort={sort}
            filtered={filter.trim() !== ""}
          />{" "}
          <Trans>Stopping an app's process restarts it unless you stop the app itself.</Trans>
        </p>
      </>
    );
  } else {
    body = (
      <GroupTable
        groups={view === "services" ? units : containers}
        view={view}
        place={place}
        caption={view === "services" ? t`Services on ${server.name}` : t`Containers on ${server.name}`}
      />
    );
  }

  return (
    <div className="procs">
      <section className="procs-main" aria-labelledby="procs-title">
        <h2 className="dw-sr" id="procs-title">
          {t`Processes`}
        </h2>
        <div className="procs-bar">
          <SegmentedControl
            options={views}
            value={view}
            onChange={(v) => {
              setView(v);
              onSearch({ ...search, q: place.typed || undefined, sort: undefined, page: undefined });
            }}
            label={t`Show`}
            size="sm"
          />
          <span className="procs-spacer" />
          {connected && list && !error && view !== "connections" ? (
            <span className="procs-live">
              <span className="dw-live">
                <span className="dw-live-dot" />
                <Trans>Live</Trans>
              </span>
              <Trans>every 2 s</Trans>
            </span>
          ) : null}
          <div className="procs-filter">
            <ListSearchField
              placeholder={
                view === "connections" ? t`Filter by port, address, process or state` : t`Filter by name, PID or user`
              }
              label={view === "connections" ? t`Filter connections` : t`Filter processes`}
              value={place.typed}
              onChange={place.setTyped}
            />
          </div>
        </div>
        {list && view !== "connections" ? failure : null}
        {body}
      </section>
      <div className="procs-side">
        <ServicesPanel units={units} total={units.length} busy={connected && !list} />
        <TotalsPanel server={server} count={list?.total} locale={i18n.locale} />
      </div>
      {selected ? (
        <ProcessDetailSheet
          server={server}
          target={selected}
          onSelectParent={(pid) => {
            const match = all.find((p) => p.pid === pid);
            setSelected({ pid, startTicks: match?.startTicks, process: match });
          }}
          onEnd={(p) => setEnding(p)}
          onClose={() => setSelected(undefined)}
        />
      ) : null}
      {ending ? (
        <EndProcessSheet
          server={server}
          process={ending}
          owner={owners.get(ending) ?? { kind: "none" }}
          onClose={() => setEnding(undefined)}
        />
      ) : null}
    </div>
  );
}

/** "Showing the top 50 of 187 by CPU", or what a filter found. */
function ShowingNote({
  shown,
  matching,
  total,
  sort,
  filtered,
}: {
  shown: number;
  matching: number;
  total: number;
  sort: Sort;
  filtered: boolean;
}) {
  const { t } = useLingui();
  if (filtered) {
    return <>{plural(matching, { one: "# process matches.", other: "# processes match." })}</>;
  }
  if (shown >= total) return <>{plural(total, { one: "All # process.", other: "All # processes." })}</>;
  const by = { cpu: t`CPU`, memory: t`memory`, pid: t`PID`, name: t`name`, uptime: t`uptime` }[sort.key];
  return <>{t`Showing ${shown} of ${total}, by ${by}.`}</>;
}

/** The words under a process's name: what it belongs to, or its command. */
function useOwnerWords() {
  const { t } = useLingui();
  return (owner: Owner, process: Process): string => {
    switch (owner.kind) {
      case "dockwell":
        return t`Dockwell`;
      case "app":
        return t`App ${owner.name}`;
      case "container":
        return t`Container ${owner.name}`;
      case "unit":
        return t`Service ${owner.name}`;
      case "none":
        return process.command !== process.name ? process.command : "";
    }
  };
}

/** How long a process has run, by the control plane's clock. */
function useUptime() {
  const words = useTimeWords();
  const now = useControlPlaneNow();
  return (p: Process) => (p.startTime ? words.span(Math.max(0, now - timestampMs(p.startTime))) : "–");
}

interface RowsProps {
  rows: readonly Process[];
  owners: ReadonlyMap<Process, Owner>;
  measured: boolean;
  onSelect: (p: Process) => void;
  onEnd: (p: Process) => void;
}

function ProcessTable({
  rows,
  owners,
  measured,
  sort,
  onSort,
  onSelect,
  selectedPid,
  onEnd,
  caption,
  filtered,
  loading = false,
  slow,
}: RowsProps & {
  sort: Sort;
  onSort: (key: SortKey) => void;
  selectedPid: number | null;
  caption: string;
  filtered: boolean;
  /** The first list is on its way: the table draws its rows' shapes under its real header. */
  loading?: boolean;
  /** What a long wait for it is. */
  slow?: string;
}) {
  const { t, i18n } = useLingui();
  const ownerWords = useOwnerWords();
  const uptime = useUptime();
  const mayEnd = useMay()(ProcessService.method.endProcess).allowed;
  const context = useRowMenu<Process>();
  const actionsFor = (process: Process): MenuAction[] =>
    !process.protected && mayEnd
      ? [
          {
            id: "stop",
            label: t`Stop`,
            icon: "stop",
            danger: true,
            onSelect: () => onEnd(process),
          },
        ]
      : [];
  const menuLabel = (process: Process) => t`Actions for ${process.name}`;
  const header = (key: SortKey, label: string) => {
    const on = sort.key === key;
    return (
      <button type="button" className="procs-sort" aria-pressed={on} onClick={() => onSort(key)}>
        <span>{label}</span>
        {on ? <Icon name={sort.descending ? "chevron-down" : "chevron-up"} size={12} /> : null}
      </button>
    );
  };
  const sorted = (key: SortKey) => (sort.key === key ? (sort.descending ? "descending" : "ascending") : undefined);
  const columns: TableColumn<Process>[] = [
    {
      key: "name",
      header: header("name", t`Process`),
      sort: sorted("name"),
      render: (p) => {
        const sub = ownerWords(owners.get(p) ?? { kind: "none" }, p);
        return (
          <div className="dw-cell procs-cell">
            <span className="procs-name">{p.name}</span>
            {sub ? (
              <span className="procs-sub" title={p.command}>
                {sub}
              </span>
            ) : null}
          </div>
        );
      },
    },
    {
      key: "pid",
      header: header("pid", t`PID`),
      sort: sorted("pid"),
      width: 84,
      align: "right",
      mono: true,
      muted: true,
    },
    { key: "user", header: t`User`, width: 112, mono: true, muted: true },
    {
      key: "cpu",
      header: header("cpu", t`CPU`),
      sort: sorted("cpu"),
      width: 80,
      align: "right",
      render: (p) => (measured ? `${formatFixed(p.cpuPercent, 1, i18n.locale)}%` : "–"),
    },
    {
      key: "memory",
      header: header("memory", t`Memory`),
      sort: sorted("memory"),
      width: 92,
      align: "right",
      render: (p) => formatBytes(p.memoryBytes, i18n.locale),
    },
    {
      key: "uptime",
      header: header("uptime", t`Uptime`),
      sort: sorted("uptime"),
      width: 92,
      muted: true,
      render: uptime,
    },
    {
      key: "act",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 108,
      align: "right",
      render: (p) => (
        <span className="list-row-actions">
          <EndButton process={p} onEnd={onEnd} />
          <ActionMenu label={menuLabel(p)} actions={actionsFor(p)} />
        </span>
      ),
    },
  ];
  return (
    <>
      <Table
        className="procs-table"
        columns={columns}
        rows={rows}
        rowKey="pid"
        rowHasContextMenu={(process) => !process.protected && mayEnd}
        onRowContextMenu={(process, at, row) =>
          context.open(process, at, row, {
            label: menuLabel(process),
            actions: actionsFor(process),
          })
        }
        onRowClick={onSelect}
        selectedKey={selectedPid}
        density="compact"
        caption={caption}
        loading={loading}
        loadingRows={10}
        loadingLabel={t`Reading what runs here`}
        slow={slow}
        empty={filtered ? t`Nothing that runs matches.` : t`Nothing runs here but the kernel.`}
      />
      {context.element}
    </>
  );
}

/** Stop, or why it can't be. */
function EndButton({ process, onEnd }: { process: Process; onEnd: (p: Process) => void }) {
  const { t } = useLingui();
  const mayEnd = useMay()(ProcessService.method.endProcess);
  if (process.protected) {
    let why: string;
    switch (process.protected) {
      case "init":
        why = t`PID 1 starts and stops everything else. Restart the server instead.`;
        break;
      case "ssh":
        why = t`This SSH server takes new logins, a way back onto the server. Restart it with systemctl restart ssh instead.`;
        break;
      default:
        why = t`Dockwell needs this process, or runs under it. Restart Dockwell instead.`;
    }
    return (
      <span className="procs-protected" title={why}>
        <Icon name="lock" size={13} />
        <Trans>Protected</Trans>
        <span className="dw-sr">{why}</span>
      </span>
    );
  }
  return (
    <Button
      size="sm"
      variant="ghost"
      icon="stop"
      aria-label={t`Stop ${process.name} (PID ${process.pid})`}
      onClick={(event) => {
        event.stopPropagation();
        onEnd(process);
      }}
      {...mayEnd.off}
    >
      <Trans>Stop</Trans>
    </Button>
  );
}

/** On a phone, a row each: name and what it belongs to, then CPU and memory. */
function PhoneRows({ rows, owners, measured, onSelect, onEnd }: RowsProps) {
  const { t, i18n } = useLingui();
  const ownerWords = useOwnerWords();
  if (rows.length === 0) return <p className="procs-note">{t`Nothing that runs matches.`}</p>;
  return (
    <ul className="procs-rows" aria-label={t`Processes`}>
      {rows.map((p) => {
        const sub = ownerWords(owners.get(p) ?? { kind: "none" }, p);
        const cpu = measured ? `${formatFixed(p.cpuPercent, 1, i18n.locale)}%` : "–";
        return (
          <li key={p.pid} className="procs-row">
            <button type="button" className="procs-cell procs-link-cell" onClick={() => onSelect(p)}>
              <span className="procs-name">{p.name}</span>
              <span className="procs-sub">{[`${p.pid}`, p.user, sub].filter(Boolean).join(" · ")}</span>
            </button>
            <span className="procs-row-nums">
              <span>{cpu}</span>
              <span className="procs-sub">{formatBytes(p.memoryBytes, i18n.locale)}</span>
            </span>
            <EndButton process={p} onEnd={onEnd} />
          </li>
        );
      })}
    </ul>
  );
}

/** Containers or services, each with what its processes take together. */
function GroupTable({
  groups,
  view,
  place,
  caption,
}: {
  groups: readonly ProcessGroup[];
  view: "services" | "containers";
  place: ListPlace;
  caption: string;
}) {
  const { t, i18n } = useLingui();
  const rows = groups
    .map((g) => ({ ...g, name: groupName(g) }))
    .filter((g) => g.name.toLowerCase().includes(place.query));
  const page = pagedRows(
    sortedRows(rows, place.sort, (group, key) => {
      if (key === "count") return group.processes.length;
      if (key === "cpu") return group.cpuPercent;
      if (key === "memory") return Number(group.memoryBytes);
      return group.name;
    }),
    place.page,
    PAGE,
  );
  const columns: TableColumn<(typeof rows)[number]>[] = [
    {
      key: "name",
      header: view === "services" ? t`Service` : t`Container`,
      render: (g) => (
        <div className="dw-cell procs-cell">
          <span className="procs-name">{g.name}</span>
          {g.owner.kind === "app" ? <span className="procs-sub">{t`App ${g.owner.name}`}</span> : null}
        </div>
      ),
    },
    {
      key: "count",
      header: t`Processes`,
      width: 104,
      align: "right",
      muted: true,
      render: (g) => g.processes.length,
    },
    {
      key: "cpu",
      header: t`CPU`,
      width: 80,
      align: "right",
      render: (g) => `${formatFixed(g.cpuPercent, 1, i18n.locale)}%`,
    },
    {
      key: "memory",
      header: t`Memory`,
      width: 92,
      align: "right",
      render: (g) => formatBytes(g.memoryBytes, i18n.locale),
    },
  ];
  return (
    <>
      <Table
        className="procs-table"
        columns={sortableColumns(columns, place, GROUP_SORT_KEYS)}
        rows={page.rows}
        rowKey="key"
        density="compact"
        caption={caption}
        empty={view === "services" ? t`No systemd unit runs a process here.` : t`No container runs here.`}
      />
      <ListPager page={page} onPage={place.setPage} />
    </>
  );
}

function groupName(g: ProcessGroup): string {
  switch (g.owner.kind) {
    case "app":
    case "container":
    case "unit":
      return g.owner.kind === "app" ? (g.processes[0]?.container?.name ?? g.owner.name) : g.owner.name;
    default:
      return g.key;
  }
}

/** The units that run the most, each running, with what its processes take. */
function ServicesPanel({ units, total, busy }: { units: readonly ProcessGroup[]; total: number; busy: boolean }) {
  const { t, i18n } = useLingui();
  const shown = units.slice(0, UNITS);
  return (
    <Panel
      title={t`Services`}
      actions={
        total > 0 ? (
          <span className="panel-meta">
            {total > UNITS
              ? t`${UNITS} of ${total} systemd units`
              : plural(total, { one: "# systemd unit", other: "# systemd units" })}
          </span>
        ) : null
      }
      flush
    >
      {busy ? (
        <Loading label={t`Reading what runs here`}>
          <SkeletonRows rows={4} lines={1} end="value" dense />
        </Loading>
      ) : shown.length === 0 ? (
        <p className="procs-panel-note">{t`No systemd unit runs a process here.`}</p>
      ) : (
        <ul className="procs-units">
          {shown.map((u) => {
            const name = groupName(u);
            const count = u.processes.length;
            const cpu = `${formatFixed(u.cpuPercent, 1, i18n.locale)}%`;
            const memory = formatBytes(u.memoryBytes, i18n.locale);
            return (
              <li key={u.key} className="procs-unit">
                <div className="procs-cell">
                  <span className="procs-name">{name}</span>
                  <span className="procs-sub">
                    {plural(count, {
                      one: `# process · ${cpu} · ${memory}`,
                      other: `# processes · ${cpu} · ${memory}`,
                    })}
                  </span>
                </div>
                <StatusBadge status="running" />
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}

/** What the server has in use, as meters, and its load. */
function TotalsPanel({ server, count, locale }: { server: Server; count: number | undefined; locale: string }) {
  const { t } = useLingui();
  const stats = server.stats;
  const cpus = server.inventory?.cpuCount ?? 0;
  const meters: { label: string; share: number; value: string }[] = [];
  if (stats) {
    meters.push({ label: t`CPU`, share: stats.cpuPercent, value: `${formatFixed(stats.cpuPercent, 1, locale)}%` });
    const [mem, memTotal] = formatLiveBytesOf(stats.memoryUsedBytes, stats.memoryTotalBytes, locale);
    meters.push({
      label: t`Memory`,
      share: share(stats.memoryUsedBytes, stats.memoryTotalBytes),
      value: t`${mem} of ${memTotal}`,
    });
    if (stats.swapTotalBytes > 0n) {
      const [swap, swapTotal] = formatLiveBytesOf(stats.swapUsedBytes, stats.swapTotalBytes, locale);
      meters.push({
        label: t`Swap`,
        share: share(stats.swapUsedBytes, stats.swapTotalBytes),
        value: t`${swap} of ${swapTotal}`,
      });
    }
  }
  const load = stats
    ? `${formatFixed(stats.load1, 2, locale)} / ${formatFixed(stats.load5, 2, locale)} / ${formatFixed(stats.load15, 2, locale)}`
    : "";
  return (
    <Panel
      title={t`Totals`}
      actions={
        count !== undefined ? (
          <span className="panel-meta">{plural(count, { one: "# process", other: "# processes" })}</span>
        ) : null
      }
    >
      {stats ? (
        <div className="procs-totals">
          {meters.map((m) => (
            <div key={m.label} className="procs-meter">
              <span className="procs-meter-label">{m.label}</span>
              {/* biome-ignore lint/a11y/useSemanticElements: a <meter> takes no styling that reads the same in both themes */}
              <span
                className="procs-meter-bar"
                role="meter"
                aria-label={m.label}
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={Math.round(m.share)}
                aria-valuetext={m.value}
              >
                <span style={{ width: `${Math.min(100, Math.max(0, m.share))}%` }} />
              </span>
              <span className="procs-meter-value">{m.value}</span>
            </div>
          ))}
          <p className="procs-load">
            {cpus > 0 ? (
              <Trans>
                Load average <span className="procs-load-value">{load}</span> on {cpus} vCPU
              </Trans>
            ) : (
              <Trans>
                Load average <span className="procs-load-value">{load}</span>
              </Trans>
            )}
          </p>
        </div>
      ) : (
        <p className="panel-empty">{t`The agent hasn't reported yet.`}</p>
      )}
    </Panel>
  );
}

function share(part: bigint, whole: bigint): number {
  return whole > 0n ? (Number(part) / Number(whole)) * 100 : 0;
}
