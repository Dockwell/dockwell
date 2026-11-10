// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, type Server } from "@dockwell/api";
import { Button, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { AddServerSheet } from "../servers/AddServerSheet";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "../servers/FleetNotes";
import { ServerCallout } from "../servers/ServerCallout";
import { ServerMetrics } from "../servers/ServerMetrics";
import { useServerWords } from "../servers/server";
import { useFleet, useFleetUnread } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page, type PageProps } from "../shell/Page";
import { UpdatesCard, UpdatesPhoneCard } from "../updates/UpdatesCard";
import { FleetOverview } from "./FleetOverview";
import { OverviewApps } from "./OverviewApps";
import { OverviewBackups } from "./OverviewBackups";
import { RecentChanges } from "./RecentChanges";
import "./Overview.css";

/**
 * The first screen: is everything OK? With one server, that server's health,
 * live, then what runs on it, what changed, how its backups stand and which
 * updates wait, as Overview-Single has them. With none, how to add one. From the
 * second server on, the fleet's, as Overview-Fleet has it.
 */
export function Overview() {
  const { t } = useLingui();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <OverviewPage title={t`Overview`}>
        <Connecting error={state.error} />
      </OverviewPage>
    );
  }
  const { servers } = state;
  const [only] = servers;
  if (servers.length === 1 && only) return <OneServer server={only} status={status} />;
  if (servers.length === 0) {
    return (
      <OverviewPage servers={0} title={t`Overview`} status={status}>
        <NoServers />
      </OverviewPage>
    );
  }
  return <FleetOverview servers={servers} status={status} />;
}

/**
 * The overview's page, at the width of what it shows: one server's dashboard is a wide page, and so is a fleet's
 * (FleetOverview); the note while it connects or before a server joins needs no room, so those are standard (Layout
 * in design/system/README.md). Each state takes its width from here, so none takes another by mistake. One server's
 * dashboard grows from the connecting note's width once the fleet's state arrives.
 */
function OverviewPage({ servers, ...props }: Omit<PageProps, "width"> & { servers?: number }) {
  return <Page {...props} width={servers === 1 ? "wide" : "standard"} />;
}

function OneServer({ server, status }: { server: Server; status: ReactNode }) {
  const { t, i18n } = useLingui();
  const { up, phoneSub } = useServerWords(server);
  const unread = useFleetUnread();
  const phone = useMediaQuery(PHONE_QUERY);
  const inv = server.inventory;
  const cpus = inv?.cpuCount ?? 0;
  const lead = [
    cpus > 0 ? t`${cpus} vCPU` : "",
    inv && inv.memoryBytes > 0n ? formatBytes(inv.memoryBytes, i18n.locale) : "",
    inv?.osName ?? "",
    up,
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <OverviewPage
      servers={1}
      title={t`Overview`}
      lead={lead || undefined}
      phoneTitle={server.name}
      phoneMachineTitle
      phoneSub={unread ? t`Unknown` : phoneSub}
      status={status}
      actions={
        <RouterButtonLink to="/servers/$serverId/files" params={{ serverId: server.id }} icon="folder">
          <Trans>Files</Trans>
        </RouterButtonLink>
      }
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : <ServerCallout server={server} />}
      <ServerMetrics
        server={server}
        fourth="network"
        compact={phone}
        stale={unread !== undefined || server.connection !== Connection.CONNECTED}
      />
      <div className="overview-columns">
        <div className="overview-main">
          <OverviewApps />
          {/* On a phone, the updates follow what runs, as M-Overview-Single has them. */}
          {phone ? <UpdatesPhoneCard server={server} /> : null}
        </div>
        <div className="overview-side">
          <RecentChanges />
          <OverviewBackups />
          {phone ? null : <UpdatesCard server={server} />}
        </div>
      </div>
    </OverviewPage>
  );
}

/** No server has joined: adding one is the way on. */
function NoServers() {
  const [adding, setAdding] = useState(false);
  return (
    <>
      <EmptyState
        icon="server"
        title={<Trans>No servers yet</Trans>}
        action={
          <Button variant="primary" icon="plus" onClick={() => setAdding(true)}>
            <Trans>Add server</Trans>
          </Button>
        }
      />
      {adding ? <AddServerSheet control={undefined} onClose={() => setAdding(false)} /> : null}
    </>
  );
}
