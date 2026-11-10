// SPDX-License-Identifier: AGPL-3.0-only

import { TabNav } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { RouterTabLink } from "../../lib/links";
import { Page } from "../shell/Page";
import { useAlerts, useChannels, useRules } from "./useAlerts";
import "./Alerts.css";

/**
 * The Alerts screen's frame, as each of its tabs shows it: what's open, how many
 * rules there are and where they send, and the tabs.
 */
export function AlertsPage({
  actions,
  phoneAction,
  children,
}: {
  actions?: ReactNode;
  /** The phone header's button, in place of its search, for the tab's own action. */
  phoneAction?: ReactNode;
  children: ReactNode;
}) {
  const { t } = useLingui();
  const firing = useAlerts().data?.firing ?? 0;
  const rules = useRules().data?.rules;
  const channels = useChannels().data?.channels;
  const lead =
    rules && channels
      ? [
          firing > 0 ? t`${firing} open` : t`Nothing open`,
          plural(rules.length, { one: "# rule", other: "# rules" }),
          channels.length > 0
            ? plural(channels.length, { one: "sent to # channel", other: "sent to # channels" })
            : t`no channel yet, so nothing is sent`,
        ].join(" · ")
      : undefined;
  return (
    <Page title={t`Alerts`} lead={lead} actions={actions} phoneAction={phoneAction} className="alerts">
      <TabNav label={t`Alerts sections`}>
        <RouterTabLink
          to="/alerts"
          activeOptions={{ exact: true }}
          count={firing > 0 ? firing : undefined}
          countTone="warning"
        >
          <Trans>Alerts</Trans>
        </RouterTabLink>
        <RouterTabLink to="/alerts/rules" count={rules?.length}>
          <Trans>Rules</Trans>
        </RouterTabLink>
        <RouterTabLink to="/alerts/channels" count={channels?.length}>
          <Trans>Channels</Trans>
        </RouterTabLink>
      </TabNav>
      {children}
    </Page>
  );
}
