// SPDX-License-Identifier: AGPL-3.0-only

import { Connection } from "@dockwell/api";
import { PHONE_QUERY, useMediaQuery } from "@dockwell/ui";

import { OnePanelCallout } from "../import/OnePanelImport";
import { StopPanelCallout } from "../import/StopPanel";
import type { ListRouteProps } from "../shell/ListTools";
import { RebootCallout } from "../updates/reboot";
import { ContainersPanel } from "./ContainersPanel";
import { DiskUsePanel } from "./DiskUse";
import { FleetUnreadNote } from "./FleetNotes";
import { PhoneServer } from "./PhoneServer";
import { RunsHere } from "./RunsHere";
import { ServerCallout } from "./ServerCallout";
import { ServerFrame } from "./ServerFrame";
import { ServerHistory } from "./ServerHistory";
import { ServerMetrics } from "./ServerMetrics";
import { AgentPanel, NetworkPanel, SystemPanel } from "./ServerPanels";
import { useFleetUnread } from "./useFleet";
import "./ServerOverview.css";

/**
 * One server's overview, as Server-Overview.dc.html has it: its live numbers, what
 * runs on it, what takes its disk, what it is, how to reach it, and its agent.
 */
export function ServerOverview({ id, search, onSearch }: { id: string } & ListRouteProps) {
  const phone = useMediaQuery(PHONE_QUERY);
  const unread = useFleetUnread();
  return (
    <ServerFrame id={id}>
      {(server) => (
        // The frame stays as wide as every other server tab, so its head and tabs never move between them; the
        // overview's own panels keep a detail page's measure.
        <div className="server-overview-body">
          {/* What it last said of itself can't stand, so that is what it says, and nothing it last said instead. */}
          {unread ? (
            <FleetUnreadNote why={unread.why} heard={unread.heard} />
          ) : phone ? null : (
            <ServerCallout server={server} />
          )}
          <OnePanelCallout server={server} />
          <StopPanelCallout server={server} />
          <RebootCallout server={server} />
          {/* A phone leads with M-Server.dc.html's: how it is, what runs, and what happened. */}
          {phone ? (
            <>
              <PhoneServer server={server} heard={unread?.heard} />
              <ServerHistory server={server} />
            </>
          ) : (
            <>
              <ServerMetrics
                server={server}
                fourth="load"
                stale={unread !== undefined || server.connection !== Connection.CONNECTED}
              />
              <ServerHistory server={server} />
              <RunsHere server={server} />
            </>
          )}
          {/* What fills the disks, beside the containers Dockwell didn't make once the page has room for both. */}
          <div className="server-pair">
            <DiskUsePanel server={server} />
            <ContainersPanel server={server} search={search} onSearch={onSearch} />
          </div>
          <div className="server-panels">
            <SystemPanel server={server} />
            <NetworkPanel server={server} stale={unread !== undefined} />
            <AgentPanel server={server} unknown={unread !== undefined} />
          </div>
        </div>
      )}
    </ServerFrame>
  );
}
