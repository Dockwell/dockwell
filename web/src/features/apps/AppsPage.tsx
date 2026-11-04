// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { Page } from "../shell/Page";

/** Every app. */
export function AppsPage() {
  const { t } = useLingui();
  return <Page title={t`Apps`} />;
}
