// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { PHONE_QUERY, Tag, useMediaQuery, useUIStrings } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, type ReactNode } from "react";

import { useTimeWords } from "../../lib/time";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { Connecting, Reconnecting } from "./FleetNotes";
import { ServerCallout } from "./ServerCallout";
import { ServerMetrics } from "./ServerMetrics";
import { AgentPanel, DisksPanel, NetworkPanel, SystemPanel } from "./ServerPanels";
import { primaryAddress, serverStatus } from "./server";
import { useFleet } from "./useFleet";
import "./ServerOverview.css";

/**
 * One server's own page, which exists from the first server on: its live
 * numbers, its disks, what it is, how to reach it, and its agent.
 */
export function ServerOverview({ id }: { id: string }) {
  const { t } = useLingui();
  const state = useFleet();
  if (state.status === "connecting") {
    return (
      <Page title={t`Server`}>
        <Connecting error={state.error} />
      </Page>
    );
  }
  const status = state.status === "reconnecting" ? <Reconnecting /> : undefined;
  const server = state.servers.find((s) => s.id === id);
  if (!server) {
    return (
      <Page title={t`Server`} status={status}>
        <EmptyState
          icon="server"
          title={<Trans>No server with this ID</Trans>}
          action={
            <Link to="/" className="dw-btn dw-btn-secondary dw-btn-lg">
              <Trans>Go to the overview</Trans>
            </Link>
          }
        >
          <p>
            <Trans>
              <code>{id}</code> may have been removed, or the link may be wrong.
            </Trans>
          </p>
        </EmptyState>
      </Page>
    );
  }
  return <ServerPage server={server} fleet={state.servers.length > 1} status={status} />;
}

function ServerPage({ server, fleet, status }: { server: Server; fleet: boolean; status: ReactNode }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const words = useTimeWords();
  const phone = useMediaQuery(PHONE_QUERY);
  const inv = server.inventory;
  const address = primaryAddress(inv?.addresses ?? []);
  // Uptime is from the last report, so it says nothing once the agent stops reporting.
  const live = server.connection === Connection.CONNECTED;
  const uptime = live && server.stats?.uptime ? words.span(durationMs(server.stats.uptime)) : undefined;
  const up = uptime ? t`up ${uptime}` : "";
  const hostname = inv?.hostname && inv.hostname !== server.name ? inv.hostname : undefined;
  const lead =
    hostname || address || up
      ? [hostname, address, up].filter(Boolean).map((part, i) => (
          <Fragment key={part}>
            {i > 0 ? " · " : null}
            {part === address ? <span className="server-address">{part}</span> : part}
          </Fragment>
        ))
      : undefined;
  return (
    <Page
      title={server.name}
      machineTitle
      badge={server.controlServer ? <Tag>{t`Runs the control plane`}</Tag> : null}
      lead={lead}
      phoneSub={[strings.status[serverStatus(server)], up].filter(Boolean).join(" · ")}
      back={fleet ? "/servers" : undefined}
      status={status}
    >
      <ServerCallout server={server} />
      <ServerMetrics server={server} fourth="load" compact={phone} />
      <DisksPanel server={server} />
      <div className="server-panels">
        <SystemPanel server={server} />
        <NetworkPanel server={server} />
        <AgentPanel server={server} />
      </div>
    </Page>
  );
}
