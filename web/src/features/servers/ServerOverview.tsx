// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";
import { PHONE_QUERY, Tag, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { Fragment, type ReactNode } from "react";

import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { ContainersPanel } from "./ContainersPanel";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "./FleetNotes";
import { ServerCallout } from "./ServerCallout";
import { ServerMetrics } from "./ServerMetrics";
import { AgentPanel, DisksPanel, NetworkPanel, SystemPanel } from "./ServerPanels";
import { primaryAddress, useServerWords } from "./server";
import { useFleet, useFleetUnread } from "./useFleet";
import "./ServerOverview.css";

/**
 * One server's own page, which exists from the first server on: its live
 * numbers, its disks, what it is, how to reach it, and its agent.
 */
export function ServerOverview({ id }: { id: string }) {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <Page title={t`Server`}>
        <Connecting error={state.error} />
      </Page>
    );
  }
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
  const unread = useFleetUnread();
  const { up, phoneSub } = useServerWords(server);
  const phone = useMediaQuery(PHONE_QUERY);
  const inv = server.inventory;
  const address = primaryAddress(inv?.addresses ?? []);
  // The hostname only when it says something the name and address don't.
  const hostname = inv?.hostname && inv.hostname !== server.name && inv.hostname !== address ? inv.hostname : "";
  const parts: { key: string; node: ReactNode }[] = [
    { key: "hostname", node: hostname },
    { key: "address", node: address ? <span className="server-address">{address}</span> : "" },
    { key: "up", node: up },
  ].filter((part) => part.node);
  const lead =
    parts.length > 0
      ? parts.map(({ key, node }, i) => (
          <Fragment key={key}>
            {i > 0 ? " · " : null}
            {node}
          </Fragment>
        ))
      : undefined;
  return (
    <Page
      title={server.name}
      machineTitle
      badge={server.controlServer ? <Tag>{t`Runs the control plane`}</Tag> : null}
      lead={lead}
      phoneSub={unread ? t`Unknown` : phoneSub}
      back={fleet ? "/servers" : undefined}
      status={status}
    >
      {/* What it last said of itself can't stand, so that is what it says, and nothing it last said instead. */}
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : <ServerCallout server={server} />}
      <ServerMetrics server={server} fourth="load" compact={phone} />
      <DisksPanel server={server} />
      <ContainersPanel server={server} />
      <div className="server-panels">
        <SystemPanel server={server} />
        <NetworkPanel server={server} />
        <AgentPanel server={server} unknown={unread !== undefined} />
      </div>
    </Page>
  );
}
