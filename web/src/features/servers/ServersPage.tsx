// SPDX-License-Identifier: AGPL-3.0-only

import { useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { Page } from "../shell/Page";
import { usePageReconnecting } from "./FleetNotes";
import { ServersPanel } from "./ServersPanel";
import { useFleet } from "./useFleet";

/** Every server, with its live stats, and how many there are under the title. */
export function ServersPage() {
  const { t } = useLingui();
  const strings = useUIStrings();
  const fleet = useFleet();
  const status = usePageReconnecting(fleet.status);
  const lead = fleet.status === "connecting" ? undefined : strings.fleetGrid.servers(fleet.servers.length);
  return (
    <Page title={t`Servers`} lead={lead} status={status}>
      <ServersPanel linked reconnectingNote={false} />
    </Page>
  );
}
