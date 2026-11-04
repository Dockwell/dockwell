// SPDX-License-Identifier: AGPL-3.0-only

import { Connection } from "@dockwell/api";
import { Button, Icon, StatusBadge, useUIStrings } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

import { type FleetShape, primaryAddress, serverStatus } from "../servers/server";
import { useCommandShortcut, useOpenCommandBar } from "./CommandMenu";

/**
 * The top bar: what the panel is looking at, then search. With one server
 * that is the server itself; with more, how many and how they are.
 */
export function Topbar({ shape }: { shape: FleetShape }) {
  const { t } = useLingui();
  const openCommandBar = useOpenCommandBar();
  const shortcut = useCommandShortcut();
  return (
    <header className="topbar">
      <Scope shape={shape} />
      <span className="topbar-fill" />
      <Button className="topbar-search" icon="search" kbd={shortcut} onClick={openCommandBar}>
        {t`Search or run a command`}
      </Button>
    </header>
  );
}

function Scope({ shape }: { shape: FleetShape }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const { servers, only, fleet } = shape;
  if (only) {
    const address = primaryAddress(only.inventory?.addresses ?? []);
    return (
      <div className="topbar-scope">
        <Icon name="server" size={16} className="topbar-scope-icon" />
        <span className="topbar-server">{only.name}</span>
        <StatusBadge status={serverStatus(only)} />
        {address ? <span className="topbar-address">{address}</span> : null}
      </div>
    );
  }
  if (!fleet || !servers) return null;
  const down = servers.filter((s) => s.connection === Connection.UNREACHABLE).length;
  const waiting = servers.filter((s) => s.connection === Connection.PENDING).length;
  const count = servers.length;
  return (
    <div className="topbar-scope">
      <Icon name="server" size={16} className="topbar-scope-icon" />
      <span className="topbar-count">{plural(count, { one: "# server", other: "# servers" })}</span>
      {down > 0 ? (
        <StatusBadge status="down" label={strings.fleetGrid.count(down, "down")} />
      ) : waiting > 0 ? (
        <StatusBadge status="stopped" label={t`${waiting} waiting for an agent`} />
      ) : (
        <StatusBadge status="healthy" label={t`All healthy`} />
      )}
    </div>
  );
}
