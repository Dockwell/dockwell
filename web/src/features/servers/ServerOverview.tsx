// SPDX-License-Identifier: AGPL-3.0-only

import { PHONE_QUERY, useMediaQuery } from "@dockwell/ui";

import { ContainersPanel } from "./ContainersPanel";
import { FleetUnreadNote } from "./FleetNotes";
import { ServerCallout } from "./ServerCallout";
import { ServerFrame } from "./ServerFrame";
import { ServerMetrics } from "./ServerMetrics";
import { AgentPanel, DisksPanel, NetworkPanel, SystemPanel } from "./ServerPanels";
import { useFleetUnread } from "./useFleet";
import "./ServerOverview.css";

/** One server's overview: its live numbers, its disks, what it is, how to reach it, and its agent. */
export function ServerOverview({ id }: { id: string }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const unread = useFleetUnread();
  return (
    <ServerFrame id={id}>
      {(server) => (
        <>
          {/* What it last said of itself can't stand, so that is what it says, and nothing it last said instead. */}
          {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : <ServerCallout server={server} />}
          <ServerMetrics server={server} fourth="load" compact={phone} />
          <div className="server-pair">
            <DisksPanel server={server} />
            <ContainersPanel server={server} />
          </div>
          <div className="server-panels">
            <SystemPanel server={server} />
            <NetworkPanel server={server} />
            <AgentPanel server={server} unknown={unread !== undefined} />
          </div>
        </>
      )}
    </ServerFrame>
  );
}
