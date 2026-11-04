// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Glyph, Icon, StatusBadge, useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { type FleetView, serverStatus } from "../servers/server";
import { useCommandShortcut, useOpenCommandBar } from "./CommandMenu";

/**
 * The top bar: what the panel is looking at, then search. With one server
 * that is the server itself; with more, how many and how they are.
 */
export function Topbar({ view }: { view: FleetView }) {
  const { t } = useLingui();
  const openCommandBar = useOpenCommandBar();
  const shortcut = useCommandShortcut();
  return (
    <header className="topbar">
      <Scope view={view} />
      <span className="topbar-fill" />
      <Button className="topbar-search" icon="search" kbd={shortcut} onClick={openCommandBar}>
        {t`Search or run a command`}
      </Button>
    </header>
  );
}

function Scope({ view }: { view: FleetView }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const { servers, only, fleet, counts } = view;
  // While the watch reconnects, the states are the last ones known, so the bar says that instead of vouching for
  // them. The page announces it; this note only shows it.
  const reconnecting =
    view.status === "reconnecting" ? (
      <span className="topbar-reconnecting">
        <Glyph shape="spinner" />
        {t`Reconnecting`}
      </span>
    ) : null;
  if (only) {
    return (
      <div className="topbar-scope">
        <Icon name="server" size={16} className="topbar-scope-icon" />
        <span className="topbar-server">{only.name}</span>
        {reconnecting ?? <StatusBadge status={serverStatus(only)} />}
        {only.address ? <span className="topbar-address">{only.address}</span> : null}
      </div>
    );
  }
  if (!fleet || !servers) return null;
  const { down, waiting, unknown } = counts;
  return (
    <div className="topbar-scope">
      <Icon name="server" size={16} className="topbar-scope-icon" />
      <span className="topbar-count">{strings.fleetGrid.servers(servers.length)}</span>
      {reconnecting ?? (
        <>
          {down > 0 ? <StatusBadge status="down" label={strings.fleetGrid.count(down, "down")} /> : null}
          {waiting > 0 ? <StatusBadge status="stopped" label={t`${waiting} waiting for an agent`} /> : null}
          {unknown > 0 ? <StatusBadge status="stopped" label={t`${unknown} in an unknown state`} /> : null}
          {counts.connected === servers.length ? <StatusBadge status="healthy" label={t`All healthy`} /> : null}
        </>
      )}
    </div>
  );
}
