// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { Page } from "../shell/Page";
import { ServersPanel } from "./ServersPanel";

/** Every server, with its live stats. */
export function ServersPage() {
  const { t } = useLingui();
  return (
    <Page title={t`Servers`}>
      <ServersPanel />
    </Page>
  );
}
