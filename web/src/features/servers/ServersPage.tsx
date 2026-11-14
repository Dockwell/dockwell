// SPDX-License-Identifier: AGPL-3.0-only

import { type Server, ServerService } from "@dockwell/api";
import {
  Button,
  cx,
  FleetGrid,
  type FleetServer,
  Icon,
  PHONE_QUERY,
  STATUS,
  StatusBadge,
  Table,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useMemo, useRef, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useApps } from "../apps/useServices";
import { useMay } from "../auth/may";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AddServerSheet } from "./AddServerSheet";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "./FleetNotes";
import { ServerInspector } from "./ServerInspector";
import { serverStatus } from "./server";
import { useServerTiles } from "./tiles";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServersPage.css";

/** Which server Servers inspects, or that none is, as its URL says. */
export interface ServersSearch {
  server?: string;
  /** The inspector was closed, so none opens on its own, a reload and Back included. */
  closed?: boolean;
  /** Add a server opens with the page, as the top bar's New offers it (#617). */
  add?: boolean;
}

/** How many rows the table shows before "Show all", as Servers-List pages them. */
export const TABLE_PAGE = 20;

/**
 * Whether a server needs a look: it isn't reporting, hasn't connected, can't read Docker, or is strained,
 * which is whatever its badge doesn't call healthy, so the two can't disagree.
 */
function needsLook(server: Server): boolean {
  return serverStatus(server) !== "healthy";
}

/**
 * Every server, as Servers-List lays them out: each as a tile, then as a row with
 * its numbers and what runs there, and one inspected beside them. Without a server
 * in the URL, the first that needs a look is. On a phone, M-Servers: the tiles,
 * what needs attention, and the server chosen, with the way to its page.
 */
export function ServersPage({
  search,
  onSearch,
}: {
  search: ServersSearch;
  onSearch: (next: ServersSearch, how?: { replace?: boolean }) => void;
}) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const fleet = useFleet();
  const unread = useFleetUnread();
  const status = usePageReconnecting(fleet.status);
  const phone = useMediaQuery(PHONE_QUERY);
  const [adding, setAddingHere] = useState(search.add === true);
  // Closing the sheet takes its word out of the address, so a reload doesn't open it again.
  const setAdding = (open: boolean) => {
    setAddingHere(open);
    if (!open && search.add) onSearch({ ...search, add: undefined }, { replace: true });
  };
  const mayAdd = useMay()(ServerService.method.createJoinTicket);
  const servers = useMemo(
    () => (fleet.status === "connecting" ? [] : [...fleet.servers].sort((a, b) => a.name.localeCompare(b.name))),
    [fleet],
  );
  // Without a server in the URL, the first that needs a look opens, once: it goes into the
  // URL, so it stays as others come and go, and a reload or Back keeps it.
  const firstLook = servers.find(needsLook)?.id;
  const pick = search.server === undefined && !search.closed ? firstLook : undefined;
  useEffect(() => {
    if (pick) onSearch({ server: pick }, { replace: true });
  }, [pick, onSearch]);

  if (fleet.status === "connecting") {
    return (
      // At the width it takes once the fleet arrives, so the page doesn't change size then.
      <Page width="wide" title={t`Servers`}>
        <Connecting error={fleet.error} />
      </Page>
    );
  }
  const lead = strings.fleetGrid.servers(servers.length);
  const controlServer = servers.find((s) => s.controlServer);
  const control = controlServer ? { id: controlServer.id, name: controlServer.name } : undefined;
  const chosenId = search.server ?? pick;
  const chosen = chosenId ? servers.find((s) => s.id === chosenId) : undefined;
  // Each choice is a step Back goes through; closing is one too.
  const select = (id: string | undefined) => onSearch(id ? { server: id } : { closed: true });

  return (
    // The list with its inspector beside it, as Servers-List lays them out, uses the room: a wide page
    // (Layout in design/system/README.md).
    <Page
      width="wide"
      title={t`Servers`}
      lead={lead}
      phoneSub={lead}
      status={status}
      actions={
        <Button variant="primary" icon="plus" onClick={() => setAdding(true)} {...mayAdd.off}>
          <Trans>Add server</Trans>
        </Button>
      }
      phoneAction={
        // A header icon has no room for a reason: it's there only for whoever may use it.
        mayAdd.allowed ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`Add server`}
            onClick={() => setAdding(true)}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : null}
      {servers.length === 0 ? (
        <EmptyState
          icon="server"
          title={<Trans>No servers yet</Trans>}
          action={
            <Button variant="primary" icon="plus" onClick={() => setAdding(true)} {...mayAdd.off}>
              <Trans>Add server</Trans>
            </Button>
          }
        />
      ) : phone ? (
        <PhoneServers servers={servers} chosen={chosen} select={select} unknown={unread !== undefined} />
      ) : (
        <WideServers servers={servers} chosen={chosen} select={select} unknown={unread !== undefined} />
      )}
      {adding ? <AddServerSheet control={control} onClose={() => setAdding(false)} /> : null}
    </Page>
  );
}

interface ListProps {
  servers: readonly Server[];
  chosen: Server | undefined;
  select: (id: string | undefined) => void;
  /** What the fleet last said can't stand, so nobody can say how any server is. */
  unknown: boolean;
}

/**
 * The tiles and the table, with the inspector beside them while a server is chosen. It
 * stays in view as the page scrolls; where it wraps below the table, a choice brings it
 * into view, and closing it puts the keyboard back on the row it was about.
 */
function WideServers({ servers, chosen, select, unknown }: ListProps) {
  const { t } = useLingui();
  const tiles = useServerTiles(servers, unknown);
  const [all, setAll] = useState(false);
  const beyond = chosen ? servers.findIndex((s) => s.id === chosen.id) >= TABLE_PAGE : false;
  const layout = useRef<HTMLDivElement>(null);
  const after = useRef<{ reveal?: boolean; focus?: string }>({});
  const choose = (id: string | undefined) => {
    // Keep a revealed row mounted when its inspector closes, so focus can return to it.
    if (beyond || (id !== undefined && servers.findIndex((s) => s.id === id) >= TABLE_PAGE)) setAll(true);
    after.current = id ? { reveal: true } : { focus: chosen?.id };
    select(id);
  };
  useEffect(() => {
    const { reveal, focus } = after.current;
    after.current = {};
    const box = layout.current;
    if (!box) return;
    if (focus) box.querySelector<HTMLElement>(`.servers-row-${CSS.escape(focus)}`)?.focus();
    if (!reveal) return;
    const inspector = box.querySelector<HTMLElement>(".servers-inspector");
    const main = box.querySelector<HTMLElement>(".servers-main");
    // Beside the table it's in view already; below it, as on a narrow screen, it isn't.
    if (inspector && main && inspector.offsetTop > main.offsetTop + 8) {
      inspector.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
    }
  });
  return (
    <div ref={layout} className={cx("servers-layout", chosen && "is-inspecting")}>
      <div className="servers-main">
        {/* A map is for telling many apart at a glance. One server is its row, which says the same and more. */}
        {servers.length > 1 ? (
          <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
            <FleetGrid servers={tiles} selectedId={chosen?.id} onSelect={(s) => choose(s.id)} />
          </section>
        ) : null}
        <ServersTable
          servers={servers}
          tiles={tiles}
          chosen={chosen}
          select={choose}
          unknown={unknown}
          all={all || beyond}
          onShowAll={() => setAll(true)}
        />
      </div>
      {chosen ? (
        <ServerInspector key={chosen.id} server={chosen} unknown={unknown} onClose={() => choose(undefined)} />
      ) : null}
    </div>
  );
}

/** Every server as a row: its address, its state, its numbers, and what runs there. */
function ServersTable({
  servers,
  tiles,
  chosen,
  select,
  all,
  onShowAll,
}: ListProps & { tiles: readonly FleetServer[]; all: boolean; onShowAll: () => void }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const runs = useRunsByServer();
  const byId = new Map(tiles.map((tile) => [tile.id, tile]));
  const rows = all ? servers : servers.slice(0, TABLE_PAGE);
  const count = servers.length;
  const shown = rows.length;
  return (
    <section className="servers-table" aria-label={t`Server list`}>
      <Table<Server>
        density="compact"
        caption={t`Servers, sorted by name`}
        rows={rows}
        rowKey="id"
        rowClassName={(s) => `servers-row-${s.id}`}
        selectedKey={chosen?.id ?? null}
        onRowClick={(s) => select(s.id)}
        columns={[
          {
            key: "name",
            header: t`Server`,
            width: 200,
            render: (s) => (
              <span className="dw-cell">
                <span className="dw-cell-title servers-table-name">{s.name}</span>
                {byId.get(s.id)?.ip ? <code className="dw-cell-sub">{byId.get(s.id)?.ip}</code> : null}
              </span>
            ),
          },
          {
            key: "status",
            header: t`Status`,
            width: 160,
            render: (s) => {
              // Why it isn't well, as the old list said it, then what it is.
              const meta = [byId.get(s.id)?.note ?? "", s.controlServer ? t`Control server` : ""]
                .filter(Boolean)
                .join(" · ");
              return <StatusBadge status={byId.get(s.id)?.status ?? serverStatus(s)} meta={meta || undefined} />;
            },
          },
          {
            key: "cpu",
            header: t`CPU`,
            width: 72,
            align: "right",
            render: (s) => percent(byId.get(s.id)?.cpu),
          },
          {
            key: "memory",
            header: t`Memory`,
            width: 84,
            align: "right",
            render: (s) => percent(byId.get(s.id)?.mem),
          },
          {
            key: "runs",
            header: t`Runs`,
            muted: true,
            render: (s) => {
              const said = runs.get(s.id) ?? "";
              return said ? (
                <span className="servers-table-runs" title={said}>
                  {said}
                </span>
              ) : (
                <span aria-hidden="true">—</span>
              );
            },
          },
        ]}
      />
      <div className="servers-table-foot">
        {rows.length < count ? (
          <>
            <span>
              <Trans>
                Showing {shown} of {count} · sorted by name
              </Trans>
            </span>
            <Button variant="ghost" size="sm" onClick={onShowAll}>
              <Trans>Show all {count}</Trans>
            </Button>
          </>
        ) : (
          <span>
            {strings.fleetGrid.servers(count)} · <Trans>sorted by name</Trans>
          </span>
        )}
      </div>
    </section>
  );
}

function percent(value: number | null | undefined) {
  return value == null ? <span aria-hidden="true">—</span> : `${value}%`;
}

/**
 * The apps on each server, by name, as their deployments say: a name that two apps
 * share, in two projects, once with how many, as "db ×2".
 */
function useRunsByServer(): Map<string, string> {
  // Apps as Apps lists them: a website's own services are on Websites (ADR 0039).
  const services = useApps();
  return useMemo(() => {
    const names = new Map<string, Map<string, number>>();
    for (const s of services.data ?? []) {
      const name = s.resource?.name ?? s.id;
      for (const d of s.deployments) {
        const on = names.get(d.serverId) ?? new Map<string, number>();
        on.set(name, (on.get(name) ?? 0) + 1);
        names.set(d.serverId, on);
      }
    }
    const out = new Map<string, string>();
    for (const [server, on] of names) {
      const said = [...on.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([name, n]) => (n > 1 ? `${name} ×${n}` : name));
      out.set(server, said.join(", "));
    }
    return out;
  }, [services.data]);
}

/**
 * On a phone: the tiles, the servers that need attention (or every server, when
 * none does), and the one chosen, above the tab bar, with the way to its page.
 */
function PhoneServers({ servers, chosen, select, unknown }: ListProps) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const tiles = useServerTiles(servers, unknown);
  const attention = servers.filter(needsLook);
  const listed = attention.length > 0 ? attention : servers;
  const byId = new Map(tiles.map((tile) => [tile.id, tile]));
  const tile = chosen ? byId.get(chosen.id) : undefined;
  const cpuWords = (cpu: number) => t`CPU ${cpu}%`;
  return (
    <>
      {servers.length > 1 ? (
        <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
          <FleetGrid servers={tiles} size="sm" readout={false} selectedId={chosen?.id} onSelect={(s) => select(s.id)} />
        </section>
      ) : null}
      <section className="servers-phone-section" aria-labelledby="servers-phone-heading">
        <div className="servers-phone-head">
          <h2 className="dw-text-heading" id="servers-phone-heading">
            {attention.length > 0 ? <Trans>Needs attention</Trans> : <Trans>Every server</Trans>}
          </h2>
          <span>{strings.fleetGrid.servers(listed.length)}</span>
        </div>
        <ul className="dw-panel servers-phone-list">
          {listed.map((s) => {
            const status = byId.get(s.id)?.status ?? serverStatus(s);
            const why = byId.get(s.id)?.note;
            return (
              <li key={s.id}>
                <button
                  type="button"
                  className={cx("servers-phone-row", s.id === chosen?.id && "is-chosen")}
                  aria-pressed={s.id === chosen?.id}
                  onClick={() => select(s.id)}
                >
                  <StatusBadge status={status} label="" />
                  <span className="servers-phone-text">
                    <span className="servers-phone-name">{s.name}</span>
                    <span className="servers-phone-sub">
                      <span
                        className={cx("servers-phone-word", status !== "healthy" && `dw-tone-${STATUS[status].tone}`)}
                      >
                        {strings.status[status]}
                      </span>
                      {why ? <span> · {why}</span> : null}
                    </span>
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      </section>
      {chosen && tile ? (
        <section className="servers-phone-chosen" aria-label={t`Chosen server`}>
          <div className="servers-phone-chosen-text">
            <span className="servers-phone-chosen-name">
              <StatusBadge status={tile.status} label="" />
              <span>{chosen.name}</span>
            </span>
            <span className="servers-phone-sub">
              {[strings.status[tile.status], tile.cpu != null ? cpuWords(tile.cpu) : "", tile.ip ?? ""]
                .filter(Boolean)
                .join(" · ")}
            </span>
          </div>
          <RouterButtonLink
            to="/servers/$serverId"
            params={{ serverId: chosen.id }}
            variant="primary"
            iconAfter="arrow-right"
          >
            <Trans>Open</Trans>
          </RouterButtonLink>
        </section>
      ) : null}
    </>
  );
}
