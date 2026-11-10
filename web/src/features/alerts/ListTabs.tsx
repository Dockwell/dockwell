// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertRuleInfo, AlertState, AlertRule_Severity as Severity } from "@dockwell/api";
import { PHONE_QUERY, StatusBadge, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { EmptyState } from "../shell/EmptyState";
import { ReadFailed } from "../shell/ReadFailed";
import { OpenAlerts, ResolvedAlerts } from "./AlertCards";
import { AlertsPage } from "./AlertsPage";
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
  return (
    <AlertsPage>
      {alerts.error ? (
        <ReadFailed error={alerts.error} title={t`Can't load the alerts`} />
      ) : !alerts.data ? null : list.length === 0 ? (
        <EmptyState
          icon="check"
          title={t`Nothing has fired`}
          action={
            <RouterButtonLink to="/alerts/channels" variant="secondary">
              <Trans>Choose where alerts go</Trans>
            </RouterButtonLink>
          }
        >
          <Trans>
            Dockwell watches every server and app, and says here when a rule holds: an app down, a server it can't
            reach, a disk nearly full, a certificate about to end, a backup that failed.
          </Trans>
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
      ) : !rules.data ? null : (
        <>
          {phone ? (
            <ul className="alerts-phone-list" aria-label={t`Alert rules`}>
              {list.map((r) => (
                <li key={r.id} className="alerts-phone-item">
                  <span className="alerts-phone-title">{r.title}</span>
                  <code className="alerts-channel-sub">{r.expression}</code>
                  <span className="alerts-phone-meta">
                    {severity(r)}
                    <span className="alerts-muted">
                      <code>{r.name}</code> · {sendsTo(r)}
                    </span>
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <Table columns={columns} rows={list} caption={t`Alert rules`} />
          )}
          <p className="alerts-muted alerts-note">
            <Trans>Change a rule with dockwell alert-rules update, as a plan that undo takes back.</Trans>
          </p>
        </>
      )}
    </AlertsPage>
  );
}
