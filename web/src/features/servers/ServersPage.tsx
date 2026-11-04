// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { Page } from "../shell/Page";
import { usePageReconnecting } from "./FleetNotes";
import { ServersPanel } from "./ServersPanel";
import { useFleet } from "./useFleet";

/** Every server, with its live stats. */
export function ServersPage() {
  const { t } = useLingui();
  const status = usePageReconnecting(useFleet().status);
  return (
    <Page title={t`Servers`} status={status}>
      <ServersPanel linked reconnectingNote={false} />
    </Page>
  );
}
