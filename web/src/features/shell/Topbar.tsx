// SPDX-License-Identifier: AGPL-3.0-only

import { Button, Icon, StatusBadge, useUIStrings } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { FleetUnknown, Reconnecting } from "../servers/FleetNotes";
import { ServerAddress } from "../servers/ServerAddress";
import { type FleetView, serverStatus } from "../servers/server";
import { useFleetUnread } from "../servers/useFleet";
import { useCommandShortcut, useOpenCommandBar } from "./CommandMenu";

/**
 * The top bar: what the panel is looking at, then search, then New, which
 * starts a new app from anywhere. With one server what it looks at is the
 * server itself; with more, how many and how they are.
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
      <RouterButtonLink to="/apps/new" variant="primary" icon="plus" className="topbar-new">
        <Trans>New</Trans>
      </RouterButtonLink>
    </header>
  );
}

function Scope({ view }: { view: FleetView }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const { servers, only, fleet, counts } = view;
  // While the watch reconnects, the states are the last ones known, so the bar says that instead of vouching for
  // them. It says it for the page below too, which keeps its own note for phones. Once what it last heard is too
  // old to stand, nobody can say how they are.
  const unread = useFleetUnread();
  const reconnecting = unread ? (
    <FleetUnknown why={unread.why} heard={unread.heard} />
  ) : view.status === "reconnecting" ? (
    <Reconnecting />
  ) : null;
  if (only) {
    return (
      <div className="topbar-scope">
        <Icon name="server" size={16} className="topbar-scope-icon" />
        <span className="topbar-server">{only.name}</span>
        {reconnecting ??
          (only.dockerUnreadableSince !== undefined ? (
            <StatusBadge status="degraded" label={t`Can't read Docker`} />
          ) : (
            <StatusBadge status={serverStatus(only)} />
          ))}
        {only.address ? <ServerAddress address={only.address} className="topbar-address" /> : null}
      </div>
    );
  }
  if (!fleet || !servers) return null;
  const { down, waiting, unknown, docker } = counts;
  return (
    <div className="topbar-scope">
      <Icon name="server" size={16} className="topbar-scope-icon" />
      <span className="topbar-count">{strings.fleetGrid.servers(servers.length)}</span>
      {reconnecting ?? (
        <>
          {down > 0 ? <StatusBadge status="down" label={strings.fleetGrid.count(down, "down")} /> : null}
          {waiting > 0 ? <StatusBadge status="stopped" label={t`${waiting} waiting for an agent`} /> : null}
          {unknown > 0 ? <StatusBadge status="stopped" label={t`${unknown} in an unknown state`} /> : null}
          {docker > 0 ? <StatusBadge status="degraded" label={t`${docker} can't read Docker`} /> : null}
          {counts.connected === servers.length && docker === 0 ? (
            <StatusBadge status="healthy" label={t`All healthy`} />
          ) : null}
        </>
      )}
    </div>
  );
}
