// SPDX-License-Identifier: AGPL-3.0-only

import { Panel, Table, type TableColumn } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { EmptyState } from "../shell/EmptyState";
import { useApp } from "./AppContext";
import { appOf, envNames } from "./app";

interface Row {
  id: string;
}

/**
 * One app's environment variables, by name. Their values may hold secrets, so
 * no API response carries them.
 */
export function AppEnvironment() {
  const { t } = useLingui();
  const { service } = useApp();
  const rows = envNames(appOf(service)).map((id): Row => ({ id }));
  if (rows.length === 0) {
    return (
      <EmptyState icon="key" title={<Trans>No environment variables</Trans>}>
        <p>
          <Trans>This app runs with only what its image sets.</Trans>
        </p>
      </EmptyState>
    );
  }
  const columns: TableColumn<Row>[] = [
    { key: "id", header: t`Name`, mono: true },
    { key: "value", header: t`Value`, muted: true, render: () => t`Hidden` },
  ];
  return (
    <div className="app-environment">
      <Panel flush>
        <Table columns={columns} rows={rows} density="compact" caption={t`Environment variables`} />
      </Panel>
      <p className="app-note">
        <Trans>Dockwell hands these to the app, and never shows their values again, as they may hold secrets.</Trans>
      </p>
    </div>
  );
}
