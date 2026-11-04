// SPDX-License-Identifier: AGPL-3.0-only

import { durationMs } from "@bufbuild/protobuf/wkt";
import { Connection, type Server } from "@dockwell/api";
import { PHONE_QUERY, useMediaQuery, useUIStrings } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { Connecting, Reconnecting } from "../servers/FleetNotes";
import { ServerCallout } from "../servers/ServerCallout";
import { ServerMetrics } from "../servers/ServerMetrics";
import { ServersPanel } from "../servers/ServersPanel";
import { serverStatus } from "../servers/server";
import { useFleet } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";

/**
 * The first screen: is everything OK? With one server, that server's health,
 * live. With none, how to add one. From the second server on, every server
 * in a list, until the fleet overview exists.
 */
export function Overview() {
  const { t } = useLingui();
  const state = useFleet();
  if (state.status === "connecting") {
    return (
      <Page title={t`Overview`}>
        <Connecting error={state.error} />
      </Page>
    );
  }
  const status = state.status === "reconnecting" ? <Reconnecting /> : undefined;
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
    <Page title={t`Overview`} lead={plural(count, { one: "# server", other: "# servers" })} status={status}>
      <ServersPanel linked />
    </Page>
  );
}

function OneServer({ server, status }: { server: Server; status: ReactNode }) {
  const { t, i18n } = useLingui();
  const strings = useUIStrings();
  const words = useTimeWords();
  const phone = useMediaQuery(PHONE_QUERY);
  const inv = server.inventory;
  const cpus = inv?.cpuCount ?? 0;
  // Uptime is from the last report, so it says nothing once the agent stops reporting.
  const live = server.connection === Connection.CONNECTED;
  const uptime = live && server.stats?.uptime ? words.span(durationMs(server.stats.uptime)) : undefined;
  const up = uptime ? t`up ${uptime}` : "";
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
      phoneSub={[strings.status[serverStatus(server)], up].filter(Boolean).join(" · ")}
      status={status}
    >
      <ServerCallout server={server} />
      <ServerMetrics server={server} fourth="network" compact={phone} />
    </Page>
  );
}
