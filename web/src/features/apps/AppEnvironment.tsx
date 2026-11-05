// SPDX-License-Identifier: AGPL-3.0-only

import { Panel, PHONE_QUERY, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { EmptyState } from "../shell/EmptyState";
import { useAllVariables } from "../variables/VariablesPage";
import { useApp } from "./AppContext";
import { environmentOf, type ServiceValue, valuesOf } from "./app";

/**
 * What one app runs with: the variables and secrets of its environment it uses,
 * by the names it sees them under, and what its own spec sets. A variable isn't
 * secret, so its value shows. No API response carries a secret's, or a value of
 * the app's own, as those may hold secrets.
 */
export function AppEnvironment() {
  const { t } = useLingui();
  const { service } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const rows = valuesOf(service);
  const environment = environmentOf(service);
  const usesAny = rows.some((row) => row.kind !== "own");
  const hasOwn = rows.some((row) => row.kind === "own");
  // Only a variable has a value to show, so an app with none reads nothing.
  const list = useAllVariables(rows.some((row) => row.kind === "variable") ? environment : undefined);
  if (rows.length === 0) {
    return (
      <EmptyState icon="key" title={<Trans>No environment variables</Trans>}>
        <p>
          <Trans>This app runs with only what its image sets.</Trans>
        </p>
      </EmptyState>
    );
  }
  const source = (row: ServiceValue) => {
    const from = row.from;
    const named = from !== row.name;
    switch (row.kind) {
      case "variable":
        return named ? t`Variable ${from}` : t`Variable`;
      case "secret":
        return named ? t`Secret ${from}` : t`Secret`;
      case "own":
        return t`Set on this app`;
    }
  };
  const value = (row: ServiceValue) => {
    if (row.kind !== "variable") return <span className="app-muted">{t`Hidden`}</span>;
    const found = list.data?.variables.find((v) => v.name === row.from && !v.secret);
    if (found) return <code className="app-env-value">{found.value}</code>;
    return <span className="app-muted">{list.isPending ? "…" : t`Not loaded`}</span>;
  };
  const columns: TableColumn<ServiceValue>[] = [
    phone
      ? {
          key: "who",
          header: t`Name`,
          render: (row) => (
            <>
              <code className="app-env-name">{row.name}</code>
              <span className="app-env-source">{source(row)}</span>
            </>
          ),
        }
      : { key: "name", header: t`Name`, mono: true },
    { key: "value", header: t`Value`, render: value },
    ...(phone ? [] : [{ key: "source", header: t`From`, muted: true, render: source }]),
  ];
  return (
    <div className="app-environment">
      <Panel flush>
        <Table
          columns={columns}
          rows={rows.map((row) => ({ ...row, id: row.name }))}
          density="compact"
          caption={t`Environment variables`}
        />
      </Panel>
      {usesAny ? (
        <p className="app-note">
          <Trans>
            A value lives in its environment, and goes only to the servers that run an app using it. Change one in{" "}
            <Link to="/variables" search={{ environment }}>
              Variables and secrets
            </Link>
            , and its plan restarts each app that uses it.
          </Trans>
        </p>
      ) : null}
      {hasOwn ? (
        <p className="app-note">
          <Trans>
            Dockwell hands what's set on this app to it, and never shows those values again, as they may hold secrets.
          </Trans>
        </p>
      ) : null}
    </div>
  );
}
