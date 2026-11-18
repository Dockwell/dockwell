// SPDX-License-Identifier: AGPL-3.0-only

import type { AlertChannelInfo } from "@dockwell/api";
import { Button, Callout, TabNav } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { Page } from "../shell/Page";
import { useChecks } from "./checks";
import { useAlerts, useChannels, useRules, useSendTest } from "./useAlerts";
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
  // Checks are an admin's: the tab is there for whoever may list them.
  const checks = useChecks();
  // A channel whose newest message failed reaches no one until it takes one (#794).
  const failing = channels?.filter((c) => c.last && !c.last.ok) ?? [];
  const reaching = (channels?.length ?? 0) - failing.length;
  const lead =
    rules && channels
      ? [
          firing > 0 ? t`${firing} open` : t`Nothing open`,
          plural(rules.length, { one: "# rule", other: "# rules" }),
          channels.length === 0
            ? t`no channel yet, so nothing is sent`
            : failing.length === 0
              ? plural(channels.length, { one: "sent to # channel", other: "sent to # channels" })
              : reaching === 0
                ? t`reaching no channel`
                : plural(failing.length, {
                    one: "# channel failing",
                    other: "# channels failing",
                  }),
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
        {checks.may.allowed ? (
          <RouterTabLink to="/alerts/checks" count={checks.data?.checks.length}>
            <Trans>Uptime checks</Trans>
          </RouterTabLink>
        ) : null}
        <RouterTabLink to="/alerts/rules" count={rules?.length}>
          <Trans>Rules</Trans>
        </RouterTabLink>
        <RouterTabLink to="/alerts/channels" count={channels?.length}>
          <Trans>Channels</Trans>
        </RouterTabLink>
      </TabNav>
      {channels && channels.length > 0 && reaching === 0 ? <NotReaching failing={failing} /> : null}
      {children}
    </Page>
  );
}

/**
 * Every channel's newest message failed, so what fires reaches no one: said once,
 * above every tab, with a test and the way to the channel (#794).
 */
function NotReaching({ failing }: { failing: readonly AlertChannelInfo[] }) {
  const { t } = useLingui();
  const test = useSendTest();
  const first = failing[0];
  if (!first) return null;
  const name = first.name;
  const said = test.last?.name === name && test.last.error ? sentence(test.last.error) : (first.last?.said ?? "");
  return (
    <Callout
      tone="critical"
      title={t`Alerts aren't reaching anyone`}
      actions={
        <>
          <Button size="sm" loading={test.sending === name} onClick={() => void test.send(name)}>
            <Trans>Send test</Trans>
          </Button>
          <RouterButtonLink to="/alerts/channels" search={{ edit: name }} size="sm" variant="ghost">
            <Trans>Edit channel</Trans>
          </RouterButtonLink>
        </>
      }
    >
      {failing.length > 1
        ? plural(failing.length, {
            one: "# channel failed its last message.",
            other: "Each of # channels failed its last message.",
          })
        : said
          ? t`${name} failed: ${said}`
          : t`${name} failed its last message.`}
    </Callout>
  );
}
