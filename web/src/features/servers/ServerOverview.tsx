// SPDX-License-Identifier: AGPL-3.0-only

import { PHONE_QUERY, useMediaQuery } from "@dockwell/ui";

import { ContainersPanel } from "./ContainersPanel";
import { DiskUsePanel } from "./DiskUse";
import { FleetUnreadNote } from "./FleetNotes";
import { PhoneServer } from "./PhoneServer";
import { RunsHere } from "./RunsHere";
import { ServerCallout } from "./ServerCallout";
import { ServerFrame } from "./ServerFrame";
import { ServerMetrics } from "./ServerMetrics";
import { AgentPanel, NetworkPanel, SystemPanel } from "./ServerPanels";
import { useFleetUnread } from "./useFleet";
import "./ServerOverview.css";

/**
 * One server's overview, as Server-Overview.dc.html has it: its live numbers, what
 * runs on it, what takes its disk, what it is, how to reach it, and its agent.
 */
export function ServerOverview({ id }: { id: string }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const unread = useFleetUnread();
  return (
    <ServerFrame id={id}>
      {(server) => (
        <>
          {/* What it last said of itself can't stand, so that is what it says, and nothing it last said instead. */}
          {unread ? (
            <FleetUnreadNote why={unread.why} heard={unread.heard} />
          ) : phone ? null : (
            <ServerCallout server={server} />
          )}
          {/* A phone leads with M-Server.dc.html's: how it is, what runs, and what happened. */}
          {phone ? (
            <PhoneServer server={server} />
          ) : (
            <>
              <ServerMetrics server={server} fourth="load" />
              <RunsHere server={server} />
            </>
          )}
          <DiskUsePanel server={server} />
          <ContainersPanel server={server} />
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
