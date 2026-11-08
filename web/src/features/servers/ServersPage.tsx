// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type Server } from "@dockwell/api";
import {
  Button,
  cx,
  FleetGrid,
  type FleetServer,
  PHONE_QUERY,
  STATUS,
  StatusBadge,
  Table,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useServices } from "../apps/useServices";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AddServerSheet } from "./AddServerSheet";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "./FleetNotes";
import { ServerInspector } from "./ServerInspector";
import { serverStatus } from "./server";
import { useServerTiles } from "./tiles";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServersPage.css";

/** Which server Servers inspects, as its URL says. */
export interface ServersSearch {
  server?: string;
}

/** Whether a server needs a look: it isn't reporting, can't read Docker, or hasn't connected. */
function needsLook(server: Server): boolean {
  return server.connection !== Connection.CONNECTED || server.dockerUnreadableSince !== undefined;
}

/**
 * Every server, as Servers-List lays them out: each as a tile, then as a row with
 * its numbers and what runs there, and one inspected beside them. Without a server
 * in the URL, the first that needs a look is. On a phone, M-Servers: the tiles,
 * what needs attention, and the server chosen, with the way to its page.
 */
export function ServersPage({ search, onSearch }: { search: ServersSearch; onSearch: (next: ServersSearch) => void }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const fleet = useFleet();
  const unread = useFleetUnread();
  const status = usePageReconnecting(fleet.status);
  const phone = useMediaQuery(PHONE_QUERY);
  const [adding, setAdding] = useState(false);
  // Closing the inspector keeps it closed, rather than opening the next server that needs a look.
  const [dismissed, setDismissed] = useState(false);
  const servers = useMemo(
    () => (fleet.status === "connecting" ? [] : [...fleet.servers].sort((a, b) => a.name.localeCompare(b.name))),
    [fleet],
  );

  if (fleet.status === "connecting") {
    return (
      <Page title={t`Servers`}>
        <Connecting error={fleet.error} />
      </Page>
    );
  }
  const lead = strings.fleetGrid.servers(servers.length);
  const control = servers.find((s) => s.controlServer)?.name;
  const chosen = search.server
    ? servers.find((s) => s.id === search.server)
    : dismissed
      ? undefined
      : servers.find(needsLook);
  const select = (id: string | undefined) => {
    if (!id) setDismissed(true);
    onSearch(id ? { server: id } : {});
  };

  return (
    <Page
      title={t`Servers`}
      lead={lead}
      phoneSub={lead}
      status={status}
      actions={
        <Button variant="primary" icon="plus" onClick={() => setAdding(true)}>
          <Trans>Add server</Trans>
        </Button>
      }
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : null}
      {servers.length === 0 ? (
        <EmptyState icon="server" title={<Trans>No servers yet</Trans>}>
          <p>
            <Trans>Add one with a ticket from the control server.</Trans>
          </p>
        </EmptyState>
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

/** The tiles and the table, with the inspector beside them while a server is chosen. */
function WideServers({ servers, chosen, select, unknown }: ListProps) {
  const { t } = useLingui();
  const tiles = useServerTiles(servers, unknown);
  return (
    <div className={cx("servers-layout", chosen && "is-inspecting")}>
      <div className="servers-main">
        <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
          <FleetGrid servers={tiles} selectedId={chosen?.id} onSelect={(s) => select(s.id)} />
        </section>
        <ServersTable servers={servers} tiles={tiles} chosen={chosen} select={select} unknown={unknown} />
      </div>
      {chosen ? (
        <ServerInspector key={chosen.id} server={chosen} unknown={unknown} onClose={() => select(undefined)} />
      ) : null}
    </div>
  );
}

/** Every server as a row: its address, its state, its numbers, and what runs there. */
function ServersTable({ servers, tiles, chosen, select }: ListProps & { tiles: readonly FleetServer[] }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const runs = useRunsByServer();
  const byId = new Map(tiles.map((tile) => [tile.id, tile]));
  return (
    <section className="servers-table" aria-label={t`Server list`}>
      <Table<Server>
        density="compact"
        caption={t`Servers, sorted by name`}
        rows={servers}
        rowKey="id"
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
            render: (s) => (
              <StatusBadge
                status={byId.get(s.id)?.status ?? serverStatus(s)}
                meta={s.controlServer ? t`Control server` : undefined}
              />
            ),
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
              const names = runs.get(s.id) ?? [];
              return names.length > 0 ? (
                <span className="servers-table-runs" title={names.join(", ")}>
                  {names.join(", ")}
                </span>
              ) : (
                <span aria-hidden="true">—</span>
              );
            },
          },
        ]}
      />
      <p className="servers-table-foot">
        {strings.fleetGrid.servers(servers.length)} · <Trans>sorted by name</Trans>
      </p>
    </section>
  );
}

function percent(value: number | null | undefined) {
  return value == null ? <span aria-hidden="true">—</span> : `${value}%`;
}

/** The apps on each server, by name, as their deployments say. */
function useRunsByServer(): Map<string, string[]> {
  const services = useServices();
  return useMemo(() => {
    const out = new Map<string, string[]>();
    for (const s of services.data ?? []) {
      const name = s.resource?.name ?? s.id;
      for (const d of s.deployments) out.set(d.serverId, [...(out.get(d.serverId) ?? []), name]);
    }
    for (const names of out.values()) names.sort((a, b) => a.localeCompare(b));
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
      <section className="dw-panel servers-map" aria-label={t`Servers at a glance`}>
        <FleetGrid servers={tiles} size="sm" readout={false} selectedId={chosen?.id} onSelect={(s) => select(s.id)} />
      </section>
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
