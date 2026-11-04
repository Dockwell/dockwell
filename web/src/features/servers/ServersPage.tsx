// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { Page } from "../shell/Page";
import { Reconnecting } from "./FleetNotes";
import { ServersPanel } from "./ServersPanel";
import { useFleet } from "./useFleet";

/** Every server, with its live stats. */
export function ServersPage() {
  const { t } = useLingui();
  const reconnecting = useFleet().status === "reconnecting";
  return (
    <Page title={t`Servers`} status={reconnecting ? <Reconnecting /> : undefined}>
      <ServersPanel linked reconnectingNote={false} />
    </Page>
  );
}
