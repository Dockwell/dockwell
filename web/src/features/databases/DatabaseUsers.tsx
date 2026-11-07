// SPDX-License-Identifier: AGPL-3.0-only

import {
  type Database,
  type DatabaseAccount,
  DatabaseEngine,
  type DatabaseReading,
  type InnerDatabase,
} from "@dockwell/api";
import { Callout, Panel, PHONE_QUERY, Table, type TableColumn, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { formatBytes } from "../../lib/format";
import { useThisDatabase } from "./DatabaseLayout";
import { engineName, firstReading, readOf } from "./useDatabases";

/** The Users tab: who can sign in to the database, and the databases in it. */
export function DatabaseUsers() {
  const db = useThisDatabase();
  const reading = firstReading(db);
  const read = readOf(reading);
  const { t } = useLingui();
  if (!read) {
    const engine = engineName(db.engine);
    return (
      <Callout tone="warning" title={t`${engine} didn't say who's in it`}>
        {reading?.unread ?? t`No server has said yet`}. <Trans>Its users show again once it does.</Trans>
      </Callout>
    );
  }
  return (
    <div className="database-overview">
      <UsersSection db={db} read={read} />
      <SchemasSection db={db} read={read} />
    </div>
  );
}

/** A section of the page, with a heading and a line beside it. */
function Section({
  id,
  title,
  aside,
  children,
}: {
  id: string;
  title: ReactNode;
  aside: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="database-section" aria-labelledby={id}>
      <div className="databases-section-head">
        <h2 id={id} className="dw-text-heading">
          {title}
        </h2>
        <span className="databases-muted">{aside}</span>
      </div>
      {children}
    </section>
  );
}

/** Who can sign in, leaving out those the server made for itself. */
export function UsersSection({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const users = read.users.filter((u) => !u.system);
  const mysql = db.engine === DatabaseEngine.MYSQL || db.engine === DatabaseEngine.MARIADB;
  const name = db.resource?.name ?? "";
  const role = (u: DatabaseAccount) =>
    u.superuser ? t`Can do anything` : u.canSignIn ? t`Signs in` : t`Can't sign in`;
  const theirs = (u: DatabaseAccount): ReactNode =>
    u.databases.length > 0 ? (
      <span className="databases-tags">
        {u.databases.map((d) => (
          <Tag key={d}>{d}</Tag>
        ))}
      </span>
    ) : (
      <span className="databases-muted">{t`None of their own`}</span>
    );
  const columns: TableColumn<DatabaseAccount & { id: string }>[] = [
    { key: "name", header: t`User`, render: (u) => <span className="databases-name">{u.name}</span> },
    ...(mysql
      ? [
          {
            key: "host",
            header: t`From`,
            width: 160,
            render: (u: DatabaseAccount) => <code className="databases-code">{u.host}</code>,
          },
        ]
      : []),
    { key: "role", header: t`Role`, width: 180, render: role },
    { key: "databases", header: mysql ? t`Granted on` : t`Owns`, width: 280, render: theirs },
  ];
  const rows = users.map((u) => ({ ...u, id: `${u.name}@${u.host}` }));
  const count = users.length;
  return (
    <Section
      id="database-users"
      title={<Trans>Users</Trans>}
      aside={plural(count, { one: `# can sign in to ${name}`, other: `# can sign in to ${name}` })}
    >
      {phone ? (
        <PhoneRows
          rows={rows.map((u) => ({
            id: u.id,
            name: u.name,
            sub: [mysql ? u.host : "", role(u), u.databases.join(", ")].filter(Boolean).join(" · "),
          }))}
        />
      ) : (
        <Table
          className="database-table"
          columns={columns}
          rows={rows}
          density="compact"
          caption={t`Users of ${name}`}
        />
      )}
    </Section>
  );
}

/** The databases in it, each with who owns it and how much it takes. */
export function SchemasSection({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const name = db.resource?.name ?? "";
  const postgres = db.engine === DatabaseEngine.POSTGRESQL;
  const own = (d: InnerDatabase) => (d.system ? <span className="databases-muted">{t`Its own`}</span> : null);
  const columns: TableColumn<InnerDatabase & { id: string }>[] = [
    {
      key: "name",
      header: t`Database`,
      render: (d) => (
        <span className="databases-schema">
          <span className="databases-name">{d.name}</span>
          {own(d)}
        </span>
      ),
    },
    ...(postgres
      ? [
          {
            key: "owner",
            header: t`Owner`,
            width: 200,
            render: (d: InnerDatabase) => <code className="databases-code">{d.owner}</code>,
          },
        ]
      : []),
    {
      key: "size",
      header: t({ message: "Size", context: "how much data" }),
      width: 120,
      align: "right",
      render: (d) => formatBytes(d.sizeBytes, i18n.locale),
    },
  ];
  const rows = read.databases.map((d) => ({ ...d, id: d.name }));
  const count = read.databases.filter((d) => !d.system).length;
  const more = read.unlisted;
  return (
    <Section
      id="database-schemas"
      title={<Trans>Databases</Trans>}
      aside={plural(count, { one: `# of your own in ${name}`, other: `# of your own in ${name}` })}
    >
      {phone ? (
        <PhoneRows
          rows={rows.map((d) => ({
            id: d.id,
            name: d.name,
            sub: [postgres ? d.owner : "", formatBytes(d.sizeBytes, i18n.locale), d.system ? t`Its own` : ""]
              .filter(Boolean)
              .join(" · "),
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
      {more > 0 ? (
        <p className="databases-muted database-more">
          {plural(more, {
            one: "# more database or user isn't listed here",
            other: "# more databases and users aren't listed here",
          })}
        </p>
      ) : null}
    </Section>
  );
}

/** Rows on a phone: a name in mono, and a line of what it is. */
function PhoneRows({ rows }: { rows: readonly { id: string; name: string; sub: string }[] }) {
  return (
    <Panel flush className="databases-phone">
      {rows.map((row) => (
        <div key={row.id} className="databases-phone-row is-static">
          <span className="databases-phone-text">
            <span className="databases-phone-name databases-name">{row.name}</span>
            {row.sub ? <span className="databases-phone-sub">{row.sub}</span> : null}
          </span>
        </div>
      ))}
    </Panel>
  );
}
