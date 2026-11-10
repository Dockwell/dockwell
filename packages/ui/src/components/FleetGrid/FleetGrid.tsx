// SPDX-License-Identifier: AGPL-3.0-only

import { memo, useEffect, useState, useSyncExternalStore } from "react";

import { cx } from "../../lib/cx";
import { STATUS, type Status } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Glyph } from "../Glyph/Glyph";
import { StatusBadge } from "../StatusBadge/StatusBadge";
import "./FleetGrid.css";

export interface FleetServer {
  id: string;
  name: string;
  status: Status;
  /** Percentages, shown in the readout. */
  cpu?: number | null;
  mem?: number | null;
  ip?: string;
  /** A short note for the readout, such as "memory climbing". */
  note?: string;
}

export interface FleetGroup {
  /** A region or pool, such as "fra-1". */
  label?: string;
  /** Defaults to the number of servers. */
  meta?: string;
  servers: readonly FleetServer[];
}

export interface FleetGridProps {
  /** Servers grouped by region or pool. */
  groups?: readonly FleetGroup[];
  /** A flat list, when there are no groups. */
  servers?: readonly FleetServer[];
  selectedId?: string;
  /** Opens a server. */
  onSelect?: (server: FleetServer) => void;
  /** sm beyond about 200 servers per group. */
  size?: "sm" | "md";
  /** The total and a count per state. On by default. */
  legend?: boolean;
  /** The line under the grid naming the hovered, focused or selected server. On by default. */
  readout?: boolean;
  /** The readout while nothing is hovered, focused or selected. */
  hint?: string;
  className?: string;
}

const ORDER: readonly Status[] = ["healthy", "degraded", "down", "deploying", "stopped", "unknown"];

/**
 * The id of the hovered or focused server, outside React state, so moving
 * across tiles re-renders only the readout and not every tile.
 */
interface Inspection {
  get: () => string | null;
  set: (id: string | null) => void;
  subscribe: (listener: () => void) => () => void;
}

function createInspection(): Inspection {
  let current: string | null = null;
  const listeners = new Set<() => void>();
  return {
    get: () => current,
    set: (id) => {
      if (id === current) return;
      current = id;
      for (const listener of listeners) listener();
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/**
 * Every server as one tile, grouped by region or pool, so the fleet's state
 * reads at a glance. Healthy tiles stay quiet; problems get full colour and
 * a glyph. Each tile is a button named with the server and its state, in
 * the tab order of the grid.
 */
export function FleetGrid({
  groups,
  servers,
  selectedId,
  onSelect,
  size = "md",
  legend = true,
  readout = true,
  hint,
  className,
}: FleetGridProps) {
  const strings = useUIStrings();
  const [inspection] = useState(createInspection);
  const shownGroups: readonly FleetGroup[] = groups ?? [{ servers: servers ?? [] }];
  const all = shownGroups.flatMap((group) => group.servers);
  const counts = new Map<Status, number>();
  for (const server of all) counts.set(server.status, (counts.get(server.status) ?? 0) + 1);

  return (
    <div className={cx("dw-fleet", `dw-fleet-${size}`, className)}>
      {legend ? (
        <div className="dw-fleet-legend">
          <span className="dw-fleet-total">{strings.fleetGrid.servers(all.length)}</span>
          {ORDER.filter((status) => counts.has(status)).map((status) => (
            <span key={status} className={cx("dw-status", "dw-status-dot", `dw-tone-${STATUS[status].tone}`)}>
              <Glyph shape={STATUS[status].shape} />
              <span className="dw-status-label">{strings.fleetGrid.count(counts.get(status) ?? 0, status)}</span>
            </span>
          ))}
        </div>
      ) : null}
      <div className="dw-fleet-groups">
        {shownGroups.map((group, i) => (
          // Groups have no id of their own; their order is the caller's.
          // biome-ignore lint/suspicious/noArrayIndexKey: see above
          <div key={i} className="dw-fleet-group">
            {group.label ? (
              <div className="dw-fleet-glabel">
                <span>{group.label}</span>
                <span className="dw-fleet-gmeta">{group.meta ?? strings.fleetGrid.servers(group.servers.length)}</span>
              </div>
            ) : null}
            <ul className="dw-fleet-tiles" aria-label={group.label ?? strings.fleetGrid.group}>
              {group.servers.map((server) => (
                <Tile
                  key={server.id}
                  server={server}
                  selected={server.id === selectedId}
                  inspection={inspection}
                  onSelect={onSelect}
                />
              ))}
            </ul>
          </div>
        ))}
      </div>
      {readout ? <Readout inspection={inspection} servers={all} selectedId={selectedId} hint={hint} /> : null}
    </div>
  );
}

interface TileProps {
  server: FleetServer;
  selected: boolean;
  inspection: Inspection;
  onSelect: ((server: FleetServer) => void) | undefined;
}

/** One server. Memoized, so a render of the grid redraws only the tiles whose server or selection changed. */
const Tile = memo(function Tile({ server, selected, inspection, onSelect }: TileProps) {
  const strings = useUIStrings();
  const { id } = server;
  // A tile that goes away while hovered or focused gets no mouseleave or blur.
  useEffect(
    () => () => {
      if (inspection.get() === id) inspection.set(null);
    },
    [inspection, id],
  );
  return (
    <li>
      <button
        type="button"
        className={cx("dw-tile", `dw-tile-${server.status}`, selected && "is-selected")}
        aria-label={strings.fleetGrid.tile(server.name, strings.status[server.status], server.cpu)}
        aria-current={selected ? "true" : undefined}
        onMouseEnter={() => inspection.set(id)}
        onMouseLeave={() => inspection.set(null)}
        onFocus={() => inspection.set(id)}
        onBlur={() => inspection.set(null)}
        onClick={onSelect ? () => onSelect(server) : undefined}
      >
        {server.status === "healthy" ? null : <Glyph shape={STATUS[server.status].shape} size={8} />}
      </button>
    </li>
  );
});

interface ReadoutProps {
  inspection: Inspection;
  servers: readonly FleetServer[];
  selectedId: string | undefined;
  hint: string | undefined;
}

/** The line naming the hovered, focused or selected server, looked up by id so it shows the latest data. */
function Readout({ inspection, servers, selectedId, hint }: ReadoutProps) {
  const strings = useUIStrings();
  const inspectedId = useSyncExternalStore(inspection.subscribe, inspection.get, inspection.get);
  const find = (id: string | null | undefined) => (id == null ? undefined : servers.find((server) => server.id === id));
  const shown = find(inspectedId) ?? find(selectedId);
  return (
    <div className="dw-fleet-readout" aria-live="polite">
      {shown ? (
        <>
          <StatusBadge status={shown.status} label={shown.name} />
          <span aria-hidden="true">{strings.status[shown.status]}</span>
          {shown.cpu != null ? (
            <span>
              {strings.fleetGrid.cpu} <b>{shown.cpu}%</b>
            </span>
          ) : null}
          {shown.mem != null ? (
            <span>
              {strings.fleetGrid.memory} <b>{shown.mem}%</b>
            </span>
          ) : null}
          {shown.ip ? <code>{shown.ip}</code> : null}
          {shown.note ? <span>{shown.note}</span> : null}
        </>
      ) : (
        <span>{hint ?? strings.fleetGrid.hint}</span>
      )}
    </div>
  );
}
