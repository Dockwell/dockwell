// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertRuleInfo, AlertState, AlertRule_Severity as Severity } from "@dockwell/api";
import { PHONE_QUERY, StatusBadge, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { EmptyState } from "../shell/EmptyState";
import { ReadFailed } from "../shell/ReadFailed";
import { OpenAlerts, ResolvedAlerts } from "./AlertCards";
import { AlertsPage } from "./AlertsPage";
import { RuleSheet } from "./RuleSheet";
import { useAlerts, useChannels, useRules } from "./useAlerts";

function SeverityBadge({ severity }: { severity: Severity | undefined }) {
  const { t } = useLingui();
  return severity === Severity.CRITICAL ? (
    <StatusBadge status="down" label={t`Critical`} labelIsState />
  ) : (
    <StatusBadge status="degraded" label={t`Warning`} labelIsState />
  );
}

/** The Alerts tab: what's open now, each as a card, and what resolved beside them. */
export function AlertsTab() {
  const { t } = useLingui();
  const alerts = useAlerts();
  const list = alerts.data?.alerts ?? [];
  const open = list.filter((a) => a.state === AlertState.FIRING);
  const resolved = list.filter((a) => a.state !== AlertState.FIRING);
  const channels = useChannels().data?.channels ?? [];
  const watching = (useRules().data?.rules ?? []).filter((r) => !r.spec?.disabled).length;
  return (
    <AlertsPage>
      {alerts.error ? (
        <ReadFailed error={alerts.error} title={t`Can't load the alerts`} />
      ) : !alerts.data ? null : list.length === 0 ? (
        <EmptyState
          icon="check"
          title={t`Nothing has fired`}
          action={
            channels.length === 0 ? (
              <RouterButtonLink to="/alerts/channels" variant="primary" icon="plus">
                <Trans>Add a channel</Trans>
              </RouterButtonLink>
            ) : (
              <RouterButtonLink to="/alerts/rules" variant="secondary">
                <Trans>See the rules</Trans>
              </RouterButtonLink>
            )
          }
        >
          {watching > 0
            ? channels.length === 0
              ? plural(watching, {
                  one: "# rule watching · nowhere to send yet",
                  other: "# rules watching · nowhere to send yet",
                })
              : plural(watching, { one: "# rule watching", other: "# rules watching" })
            : null}
        </EmptyState>
      ) : (
        // As Alerts lays them out: what's open on the left, what resolved beside it.
        <div className="alerts-columns">
          <OpenAlerts alerts={open} />
          <ResolvedAlerts alerts={resolved} />
        </div>
      )}
    </AlertsPage>
  );
}

/** The Rules tab: what Dockwell watches for, how much each matters, and where it sends. */
export function RulesTab() {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const rules = useRules();
  const channels = useChannels().data?.channels ?? [];
  const [editing, setEditing] = useState<AlertRuleInfo>();
  const list = rules.data?.rules ?? [];
  const sendsTo = (r: AlertRuleInfo) => {
    const named = r.spec?.channels ?? [];
    if (channels.length === 0) return t`No channel yet`;
    if (named.length === 0) return t`Every channel`;
    return new Intl.ListFormat(i18n.locale, { style: "short", type: "conjunction" }).format(named);
  };
  const severity = (r: AlertRuleInfo) =>
    r.spec?.disabled ? (
      <StatusBadge status="stopped" label={t`Off`} labelIsState />
    ) : (
      <SeverityBadge severity={r.spec?.severity} />
    );
  const columns: TableColumn<AlertRuleInfo>[] = [
    {
      key: "title",
      header: t`Rule`,
      render: (r) => (
        <div className="dw-cell">
          <span className="dw-cell-title">{r.title}</span>
          <code className="alerts-channel-sub">{r.expression}</code>
        </div>
      ),
    },
    {
      key: "severity",
      header: t`Severity`,
      width: 132,
      render: severity,
    },
    { key: "name", header: t`Name`, width: 200, mono: true, render: (r) => r.name },
    { key: "to", header: t`Sends to`, width: 200, muted: true, render: sendsTo },
  ];
  return (
    <AlertsPage>
      {rules.error ? (
        <ReadFailed error={rules.error} title={t`Can't load the rules`} />
      ) : !rules.data ? null : phone ? (
        <ul className="alerts-phone-list" aria-label={t`Alert rules`}>
          {list.map((r) => (
            <li key={r.id} className="alerts-phone-row">
              <button type="button" className="alerts-phone-item alerts-phone-button" onClick={() => setEditing(r)}>
                <span className="alerts-phone-title">{r.title}</span>
                <code className="alerts-channel-sub">{r.expression}</code>
                <span className="alerts-phone-meta">
                  {severity(r)}
                  <span className="alerts-muted">
                    <code>{r.name}</code> · {sendsTo(r)}
                  </span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <Table columns={columns} rows={list} caption={t`Alert rules`} onRowClick={(r) => setEditing(r)} />
      )}
      {editing ? <RuleSheet rule={editing} onClose={() => setEditing(undefined)} /> : null}
    </AlertsPage>
  );
}
