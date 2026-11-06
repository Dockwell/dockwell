// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";
import { PHONE_QUERY, useMediaQuery, useUIStrings } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { formatBytes } from "../../lib/format";
import { Connecting, FleetUnreadNote, usePageReconnecting } from "../servers/FleetNotes";
import { ServerCallout } from "../servers/ServerCallout";
import { ServerMetrics } from "../servers/ServerMetrics";
import { ServersPanel } from "../servers/ServersPanel";
import { useServerWords } from "../servers/server";
import { useFleet, useFleetUnread } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";

/**
 * The first screen: is everything OK? With one server, that server's health,
 * live. With none, how to add one. From the second server on, every server
 * in a list, until the fleet overview exists.
 */
export function Overview() {
  const { t } = useLingui();
  const strings = useUIStrings();
  const state = useFleet();
  const status = usePageReconnecting(state.status);
  if (state.status === "connecting") {
    return (
      <Page title={t`Overview`}>
        <Connecting error={state.error} />
      </Page>
    );
  }
  const { servers } = state;
  const [only] = servers;
  const count = servers.length;
  if (servers.length === 1 && only) return <OneServer server={only} status={status} />;
  if (servers.length === 0) {
    return (
      <Page title={t`Overview`} status={status}>
        <EmptyState icon="server" title={<Trans>No servers yet</Trans>}>
          <p>
            <Trans>
              Run <code>dockwell token create</code> on the control server, then <code>dockwell join</code> with the
              ticket it prints on the server you want to add.
            </Trans>
          </p>
        </EmptyState>
      </Page>
    );
  }
  return (
    <Page title={t`Overview`} lead={strings.fleetGrid.servers(count)} status={status}>
      <ServersPanel linked reconnectingNote={false} />
    </Page>
  );
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
    <Page
      title={t`Overview`}
      lead={lead || undefined}
      phoneTitle={server.name}
      phoneMachineTitle
      phoneSub={unread ? t`Unknown` : phoneSub}
      status={status}
    >
      {unread ? <FleetUnreadNote why={unread.why} heard={unread.heard} /> : <ServerCallout server={server} />}
      <ServerMetrics server={server} fourth="network" compact={phone} />
    </Page>
  );
}
