// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertRuleInfo, AlertState, ChangeService, AlertRule_Severity as Severity } from "@dockwell/api";
import {
  Button,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  SkeletonCard,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  ListNoMatch,
  ListPager,
  type ListRouteProps,
  ListToolbar,
  sortableColumns,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { ReadFailed } from "../shell/ReadFailed";
import { bySeverity, OpenAlerts, ResolvedAlerts } from "./AlertCards";
import { AlertsPage } from "./AlertsPage";
import { RuleSheet, sameAs } from "./RuleSheet";
import { useAlertHistory, useChannels, useRules } from "./useAlerts";

function SeverityBadge({ severity }: { severity: Severity | undefined }) {
  const { t } = useLingui();
  return severity === Severity.CRITICAL ? (
    <StatusBadge status="down" label={t`Critical`} labelIsState />
  ) : (
    <StatusBadge status="degraded" label={t`Warning`} labelIsState />
  );
}

/** The Alerts tab: what's open now, each as a card, and what resolved beside them. */
export interface AlertsSearch extends ListSearch {
  view?: "open" | "resolved";
}

export function AlertsTab({ search, onSearch }: ListRouteProps<AlertsSearch>) {
  const { t } = useLingui();
  const alerts = useAlertHistory();
  const list = alerts.data?.alerts ?? [];
  const place = useListPlace(search, onSearch, []);
  const matches = list.filter((alert) =>
    `${alert.title} ${alert.ruleName} ${alert.serverName} ${alert.subjectName}`.toLowerCase().includes(place.query),
  );
  const open = bySeverity(matches.filter((alert) => alert.state === AlertState.FIRING));
  const resolved = matches
    .filter((alert) => alert.state !== AlertState.FIRING)
    .sort((a, b) => Number(b.resolveTime?.seconds ?? 0n) - Number(a.resolveTime?.seconds ?? 0n));
  const page = pagedRows(
    search.view === "open" ? open : search.view === "resolved" ? resolved : [...open, ...resolved],
    place.page,
  );
  const channels = useChannels().data?.channels ?? [];
  const watching = (useRules().data?.rules ?? []).filter((r) => !r.spec?.disabled).length;
  return (
    <AlertsPage>
      {list.length > 0 ? (
        <ListToolbar place={place} label={t`Filter alerts`}>
          <SelectField
            size="sm"
            aria-label={t`State`}
            value={search.view ?? ""}
            options={[
              { value: "", label: t`Every state` },
              { value: "open", label: t({ message: "Open", context: "alerts that haven't resolved" }) },
              { value: "resolved", label: t`Resolved` },
            ]}
            onChange={(event) =>
              onSearch({
                ...search,
                view:
                  event.target.value === "open" || event.target.value === "resolved" ? event.target.value : undefined,
                page: undefined,
              })
            }
          />
        </ListToolbar>
      ) : null}
      {alerts.error ? (
        <ReadFailed error={alerts.error} title={t`Can't load the alerts`} />
      ) : !alerts.data ? (
        <Loading label={t`Loading the alerts`} className="alerts-loading">
          <SkeletonCard lines={2} />
          <SkeletonCard lines={2} />
        </Loading>
      ) : list.length === 0 ? (
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
      ) : page.total === 0 ? (
        <ListNoMatch
          clear={() => {
            place.clearQuery();
            onSearch({ ...search, q: undefined, view: undefined, page: undefined });
          }}
        />
      ) : (
        <>
          <div className="alerts-columns">
            {search.view !== "resolved" ? (
              <OpenAlerts alerts={page.rows.filter((a) => a.state === AlertState.FIRING)} />
            ) : null}
            <ResolvedAlerts alerts={page.rows.filter((a) => a.state !== AlertState.FIRING)} />
          </div>
          <ListPager page={page} onPage={place.setPage} />
        </>
      )}
    </AlertsPage>
  );
}

/** The Rules tab: what Dockwell watches for, how much each matters, and where it sends. */
const RULE_SORT_KEYS = ["title", "severity", "name"] as const;

export function RulesTab({ search, onSearch }: ListRouteProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const rules = useRules();
  const channels = useChannels().data?.channels ?? [];
  const [editing, setEditing] = useState<AlertRuleInfo | "new">();
  const list = rules.data?.rules ?? [];
  const place = useListPlace(search, onSearch, RULE_SORT_KEYS);
  const context = useRowMenu<AlertRuleInfo>();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const actionsFor = (rule: AlertRuleInfo): MenuAction[] => [
    { id: "open", label: t`Open`, onSelect: () => setEditing(rule) },
  ];
  const page = pagedRows(
    sortedRows(
      list.filter((rule) => `${rule.name} ${rule.title} ${rule.expression}`.toLowerCase().includes(place.query)),
      place.sort,
      (rule, key) => (key === "title" ? rule.title : key === "severity" ? rule.spec?.severity : rule.name),
    ),
    place.page,
  );
  const add = (
    <Button variant="primary" icon="plus" onClick={() => setEditing("new")} {...mayPlan.off}>
      <Trans>New rule</Trans>
    </Button>
  );
  // A rule that repeats another says so, as a hint: either may go (#794).
  const twinOf = (r: AlertRuleInfo) => (r.spec ? sameAs(r.spec, list, r.name) : undefined);
  const twinTag = (r: AlertRuleInfo) => {
    const twin = twinOf(r);
    if (!twin) return null;
    const other = twin.name;
    return <Tag words>{t`Same as ${other}`}</Tag>;
  };
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
          <span className="dw-cell-title">
            {r.title} {twinTag(r)}
          </span>
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
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 48,
      render: (rule) => <ActionMenu label={t`Actions for ${rule.name}`} actions={actionsFor(rule)} />,
    },
  ];
  return (
    <AlertsPage
      actions={list.length > 0 ? add : undefined}
      phoneAction={
        mayPlan.allowed && list.length > 0 ? (
          <button
            type="button"
            className="mobile-header-button"
            aria-label={t`New rule`}
            onClick={() => setEditing("new")}
          >
            <Icon name="plus" size={20} />
          </button>
        ) : undefined
      }
    >
      {list.length > 0 ? <ListToolbar place={place} label={t`Filter alert rules`} /> : null}
      {rules.error ? (
        <ReadFailed error={rules.error} title={t`Can't load the rules`} />
      ) : !rules.data ? (
        phone ? (
          <Loading label={t`Loading the rules`}>
            <Panel flush>
              <SkeletonRows rows={3} end="pill" />
            </Panel>
          </Loading>
        ) : (
          <Table columns={columns} rows={[]} caption={t`Alert rules`} loading loadingLabel={t`Loading the rules`} />
        )
      ) : list.length === 0 ? (
        <EmptyState icon="bell" title={t`No alert rules yet`} action={add} />
      ) : page.total === 0 ? (
        <ListNoMatch clear={place.clearQuery} />
      ) : phone ? (
        <ul className="alerts-phone-list" aria-label={t`Alert rules`}>
          {page.rows.map((r) => (
            <li key={r.id} className="alerts-phone-row">
              <button type="button" className="alerts-phone-item alerts-phone-button" onClick={() => setEditing(r)}>
                <span className="alerts-phone-title">
                  {r.title} {twinTag(r)}
                </span>
                <code className="alerts-channel-sub">{r.expression}</code>
                <span className="alerts-phone-meta">
                  {severity(r)}
                  <span className="alerts-muted">
                    <code>{r.name}</code> · {sendsTo(r)}
                  </span>
                </span>
              </button>
              <ActionMenu label={t`Actions for ${r.name}`} actions={actionsFor(r)} />
            </li>
          ))}
        </ul>
      ) : (
        <Table
          columns={sortableColumns(columns, place, RULE_SORT_KEYS)}
          rows={page.rows}
          caption={t`Alert rules`}
          onRowClick={setEditing}
          onRowContextMenu={(rule, at, row) =>
            context.open(rule, at, row, {
              label: t`Actions for ${rule.name}`,
              actions: actionsFor(rule),
            })
          }
        />
      )}
      <ListPager page={page} onPage={place.setPage} />
      {context.element}
      {editing ? (
        <RuleSheet rule={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} />
      ) : null}
    </AlertsPage>
  );
}
