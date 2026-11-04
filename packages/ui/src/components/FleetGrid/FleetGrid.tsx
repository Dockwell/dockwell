// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";

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

const ORDER: readonly Status[] = ["healthy", "degraded", "down", "deploying", "stopped"];

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
  const [inspected, setInspected] = useState<FleetServer | null>(null);
  const shownGroups: readonly FleetGroup[] = groups ?? [{ servers: servers ?? [] }];
  const all = shownGroups.flatMap((group) => group.servers);
  const counts = new Map<Status, number>();
  for (const server of all) counts.set(server.status, (counts.get(server.status) ?? 0) + 1);
  const shown = inspected ?? (selectedId != null ? all.find((server) => server.id === selectedId) : undefined);

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
              {group.servers.map((server) => {
                const spec = STATUS[server.status];
                const selected = server.id === selectedId;
                return (
                  <li key={server.id}>
                    <button
                      type="button"
                      className={cx("dw-tile", `dw-tile-${server.status}`, selected && "is-selected")}
                      aria-label={strings.fleetGrid.tile(server.name, strings.status[server.status], server.cpu)}
                      aria-current={selected ? "true" : undefined}
                      onMouseEnter={() => setInspected(server)}
                      onMouseLeave={() => setInspected(null)}
                      onFocus={() => setInspected(server)}
                      onBlur={() => setInspected(null)}
                      onClick={onSelect ? () => onSelect(server) : undefined}
                    >
                      {server.status === "healthy" ? null : <Glyph shape={spec.shape} size={8} />}
                    </button>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </div>
      {readout ? (
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
      ) : null}
    </div>
  );
}
