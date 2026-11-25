// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Database,
  type DatabaseReading,
  DatabaseService,
  type InnerDatabase,
  TerminalService,
} from "@dockwell/api";
import { Button, Callout, Panel, PHONE_QUERY, Table, type TableColumn, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { formatBytes } from "../../lib/format";
import { useHealth } from "../apps/health";
import { useShownName } from "../apps/names";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { useThisDatabase } from "./DatabaseLayout";
import { backsUp } from "./DatabaseOverview";
import { ImportDumpSheet } from "./ImportDumpSheet";
import { NewDatabaseSheet } from "./NewDatabaseSheet";
import { consoleTarget, engineName, firstReading, readOf } from "./useDatabases";

/**
 * The Databases tab on PostgreSQL, MySQL and MariaDB engines: lists logical
 * databases with size, owner, active connections and linked apps, with New
 * database and Delete as changes with a plan.
 */
export function DatabaseSchemas() {
  const db = useThisDatabase();
  const reading = firstReading(db);
  const read = readOf(reading);
  const { t } = useLingui();
  if (!read) {
    const engine = engineName(db.engine);
    return (
      <Callout tone="warning" title={t`${engine} didn't list its databases`}>
        {reading?.unread ?? t`No server has said yet`}. <Trans>Its databases show again once it does.</Trans>
      </Callout>
    );
  }
  return (
    <div className="database-overview">
      <LogicalDatabasesSection db={db} read={read} />
    </div>
  );
}

function LogicalDatabasesSection({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const health = useHealth().of({ id: db.id });
  const [creating, setCreating] = useState(false);
  const [importingFor, setImportingFor] = useState<string | null>(null);

  const may = useMay();
  const environment = db.resource?.scope ?? "";
  const mayCreate = may(DatabaseService.method.createLogicalDatabase, { environment });
  const mayDelete = may(DatabaseService.method.deleteLogicalDatabase, { environment });
  const mayImport = may(DatabaseService.method.beginDatabaseImport, { environment });
  const mayConsole = may(TerminalService.method.openTerminal);
  const consoleAt = consoleTarget(db, health);
  const canBackUp = backsUp(db);

  const name = useShownName()(db, "database").title;
  const rows = read.databases.map((d) => ({ ...d, id: d.name }));
  const count = read.databases.filter((d) => !d.system).length;
  const numberFormat = new Intl.NumberFormat(i18n.locale);
  const importOff = !canBackUp
    ? db.volumeExternal
      ? t`Its volume isn't one Dockwell backs up`
      : t`It needs a managed volume so Dockwell can back it up first`
    : count === 0
      ? t`Create a database first before importing a dump`
      : mayImport.allowed
        ? undefined
        : mayImport.reason;

  const planDelete = (d: InnerDatabase) => {
    sheet.open({
      kind: "plan",
      make: () => createClient(DatabaseService, transport).deleteLogicalDatabase({ id: db.id, database: d.name }),
    });
  };

  const menu = (d: InnerDatabase) => {
    const actions: MenuAction[] = [];
    if (mayConsole.allowed) {
      actions.push({
        id: "console",
        icon: "terminal",
        label: t`Open console`,
        off: consoleAt ? undefined : t`The database container isn't running on a server yet`,
        onSelect: () => {
          if (!consoleAt) return;
          void navigate({
            to: "/servers/$serverId/terminal",
            params: { serverId: consoleAt.serverId },
            search: { container: consoleAt.container, console: true, database: d.name },
          });
        },
      });
    }
    if (!d.system) {
      actions.push({
        id: "import",
        icon: "upload",
        label: t`Import dump`,
        off: importOff,
        onSelect: () => setImportingFor(d.name),
      });
    }
    const linked = d.linkedBy.join(", ");
    actions.push({
      id: "delete",
      icon: "trash",
      label: t`Delete`,
      danger: true,
      off: d.system
        ? t`Its server keeps ${d.name} for itself`
        : d.linkedBy.length > 0
          ? t`Linked by ${linked}. Disconnect first`
          : !d.resourceId
            ? t`Dockwell didn't make ${d.name}, so it doesn't delete it`
            : mayDelete.allowed
              ? undefined
              : mayDelete.reason,
      onSelect: () => planDelete(d),
    });
    return <ActionMenu label={t`Actions for ${d.name}`} actions={actions} />;
  };

  const columns: TableColumn<InnerDatabase & { id: string }>[] = [
    {
      key: "name",
      header: t`Database`,
      render: (d) => (
        <span className="databases-schema">
          <span className="databases-name">{d.name}</span>
          {d.system ? <span className="databases-muted">{t`Its own`}</span> : null}
        </span>
      ),
    },
    {
      key: "owner",
      header: t`Owner`,
      width: 180,
      render: (d) =>
        d.owner ? <code className="databases-code">{d.owner}</code> : <span className="databases-muted">—</span>,
    },
    {
      key: "size",
      header: t({ message: "Size", context: "how much data" }),
      width: 112,
      align: "right",
      render: (d) => formatBytes(d.sizeBytes, i18n.locale),
    },
    {
      key: "connections",
      header: t`Connections`,
      width: 120,
      align: "right",
      render: (d) => numberFormat.format(Number(d.connections)),
    },
    {
      key: "linked",
      header: t`Used by`,
      width: 200,
      render: (d) =>
        d.linkedBy.length > 0 ? (
          <span className="databases-tags">
            {d.linkedBy.map((app) => (
              <Tag key={app}>{app}</Tag>
            ))}
          </span>
        ) : (
          <span className="databases-muted">—</span>
        ),
    },
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 56,
      align: "right",
      render: menu,
    },
  ];

  return (
    <section className="database-section" aria-labelledby="database-schemas">
      <div className="databases-section-head">
        <h2 id="database-schemas" className="dw-text-heading">
          <Trans>Databases</Trans>
        </h2>
        <span className="databases-muted">
          {plural(count, { one: `# of your own in ${name}`, other: `# of your own in ${name}` })}
        </span>
        <div className="database-section-action">
          <Button
            variant="ghost"
            size="sm"
            icon="upload"
            disabled={Boolean(importOff)}
            title={importOff}
            onClick={() => setImportingFor("")}
          >
            <Trans>Import dump</Trans>
          </Button>
          <Button variant="primary" size="sm" icon="plus" onClick={() => setCreating(true)} {...mayCreate.off}>
            <Trans>New database</Trans>
          </Button>
        </div>
      </div>
      {phone ? (
        <PhoneRows
          rows={rows.map((d) => ({
            id: d.id,
            name: d.name,
            sub: [
              d.owner,
              formatBytes(d.sizeBytes, i18n.locale),
              plural(Number(d.connections), { one: "# connection", other: "# connections" }),
              d.linkedBy.length > 0 ? d.linkedBy.join(", ") : "",
              d.system ? t`Its own` : "",
            ]
              .filter(Boolean)
              .join(" · "),
            action: menu(d),
          }))}
        />
      ) : (
        <Table
          className="database-table"
          columns={columns}
          rows={rows}
          density="compact"
          caption={t`Databases in ${name}`}
        />
      )}
      {read.unlisted > 0 ? (
        <p className="databases-muted database-more">
          {plural(read.unlisted, {
            one: "# more database or user isn't listed here",
            other: "# more databases and users aren't listed here",
          })}
        </p>
      ) : null}
      {creating ? <NewDatabaseSheet db={db} read={read} onClose={() => setCreating(false)} /> : null}
      {importingFor !== null ? (
        <ImportDumpSheet
          db={db}
          read={read}
          initialDatabase={importingFor || undefined}
          onClose={() => setImportingFor(null)}
        />
      ) : null}
    </section>
  );
}

function PhoneRows({ rows }: { rows: readonly { id: string; name: string; sub: string; action?: ReactNode }[] }) {
  return (
    <Panel flush className="databases-phone">
      {rows.map((row) => (
        <div key={row.id} className="databases-phone-row is-static">
          <span className="databases-phone-text">
            <span className="databases-phone-name databases-name">{row.name}</span>
            {row.sub ? <span className="databases-phone-sub">{row.sub}</span> : null}
          </span>
          {row.action}
        </div>
      ))}
    </Panel>
  );
}
