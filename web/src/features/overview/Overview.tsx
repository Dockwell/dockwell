// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { ServersPanel } from "../servers/ServersPanel";
import { Page } from "../shell/Page";

/** The first screen: how the servers are doing. */
export function Overview() {
  const { t } = useLingui();
  return (
    <Page title={t`Overview`}>
      <ServersPanel />
    </Page>
  );
}
