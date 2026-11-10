// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type Alert, type AlertRuleInfo, AlertState, AlertRule_Severity as Severity } from "@dockwell/api";
import { Callout, StatusBadge, Table, type TableColumn } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { alertTitle } from "../activity/describe";
import { EmptyState } from "../shell/EmptyState";
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

/** The Alerts tab: what's firing now, then what fired and resolved, newest first. */
export function AlertsTab() {
  const { t, i18n } = useLingui();
  const alerts = useAlerts();
  const list = alerts.data?.alerts ?? [];
  const at = (a: Alert) => (a.openTime ? formatDateTime(timestampDate(a.openTime), i18n.locale) : "");
  const columns: TableColumn<Alert>[] = [
    {
      key: "title",
      header: t`Alert`,
      render: (a) => (
        <div className="dw-cell">
          <span className="dw-cell-title">{alertTitle(a, i18n)}</span>
          <span className="alerts-channel-sub">{a.detail}</span>
        </div>
      ),
    },
    {
      key: "state",
      header: t`State`,
      width: 132,
      render: (a) =>
        a.state === AlertState.FIRING ? (
          <SeverityBadge severity={a.severity} />
        ) : (
          <StatusBadge status="healthy" label={t`Resolved`} labelIsState />
        ),
    },
    { key: "rule", header: t`Rule`, width: 180, mono: true, render: (a) => a.ruleName },
    { key: "at", header: t`Opened`, width: 172, muted: true, render: at },
  ];
  return (
    <AlertsPage>
      {alerts.error ? (
        <Callout tone="critical" title={t`Can't load the alerts`}>
          {sentence(alerts.error)}
        </Callout>
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
        <Table columns={columns} rows={list} caption={t`Alerts, firing first`} />
      )}
    </AlertsPage>
  );
}

/** The Rules tab: what Dockwell watches for, how much each matters, and where it sends. */
export function RulesTab() {
  const { t, i18n } = useLingui();
  const rules = useRules();
  const channels = useChannels().data?.channels ?? [];
  const list = rules.data?.rules ?? [];
  const sendsTo = (r: AlertRuleInfo) => {
    const named = r.spec?.channels ?? [];
    if (channels.length === 0) return t`No channel yet`;
    if (named.length === 0) return t`Every channel`;
    return new Intl.ListFormat(i18n.locale, { style: "short", type: "conjunction" }).format(named);
  };
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
      render: (r) =>
        r.spec?.disabled ? (
          <StatusBadge status="stopped" label={t`Off`} labelIsState />
        ) : (
          <SeverityBadge severity={r.spec?.severity} />
        ),
    },
    { key: "name", header: t`Name`, width: 200, mono: true, render: (r) => r.name },
    { key: "to", header: t`Sends to`, width: 200, muted: true, render: sendsTo },
  ];
  return (
    <AlertsPage>
      {rules.error ? (
        <Callout tone="critical" title={t`Can't load the rules`}>
          {sentence(rules.error)}
        </Callout>
      ) : !rules.data ? null : (
        <>
          <Table columns={columns} rows={list} caption={t`Alert rules`} />
          <p className="alerts-muted alerts-note">
            <Trans>Change a rule with dockwell alert-rules update, as a plan that undo takes back.</Trans>
          </p>
        </>
      )}
    </AlertsPage>
  );
}
