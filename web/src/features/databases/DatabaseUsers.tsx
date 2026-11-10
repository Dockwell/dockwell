// SPDX-License-Identifier: AGPL-3.0-only

import {
  type Database,
  type DatabaseAccount,
  DatabaseEngine,
  type DatabaseReading,
  type InnerDatabase,
} from "@dockwell/api";
import { Button, Callout, Panel, PHONE_QUERY, Table, type TableColumn, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { formatBytes } from "../../lib/format";
import { AddUserSheet } from "./AddUserSheet";
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
  action,
  children,
}: {
  id: string;
  title: ReactNode;
  aside: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="database-section" aria-labelledby={id}>
      <div className="databases-section-head">
        <h2 id={id} className="dw-text-heading">
          {title}
        </h2>
        <span className="databases-muted">{aside}</span>
        {action ? <div className="database-section-action">{action}</div> : null}
      </div>
      {children}
    </section>
  );
}

/** Its users, and whether each can sign in, leaving out those the server made for itself. */
export function UsersSection({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const [adding, setAdding] = useState(false);
  const users = read.users.filter((u) => !u.system);
  const mysql = db.engine === DatabaseEngine.MYSQL || db.engine === DatabaseEngine.MARIADB;
  const mongo = db.engine === DatabaseEngine.MONGODB;
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
    ...(mongo
      ? [
          {
            key: "authDatabase",
            header: t`Signs in to`,
            width: 160,
            render: (u: DatabaseAccount) => <code className="databases-code">{u.authDatabase}</code>,
          },
        ]
      : []),
    { key: "role", header: t`Role`, width: 160, render: role },
    { key: "databases", header: mysql ? t`Granted on` : mongo ? t`Has roles in` : t`Owns`, width: 200, render: theirs },
    {
      key: "secret",
      header: t`Password in`,
      width: 240,
      render: (u) =>
        u.secret ? (
          <code className="databases-code">{u.secret}</code>
        ) : (
          <span className="databases-muted">{t`Not one of Dockwell's`}</span>
        ),
    },
    {
      key: "used",
      header: t`Used by`,
      width: 200,
      render: (u) =>
        u.usedBy.length > 0 ? (
          <span className="databases-tags">
            {u.usedBy.map((app) => (
              <Tag key={app}>{app}</Tag>
            ))}
          </span>
        ) : (
          <span className="databases-muted">{u.secret ? t`No app yet` : ""}</span>
        ),
    },
  ];
  // A MongoDB user's name is theirs in one database, so two can share one.
  const rows = users.map((u) => ({ ...u, id: `${u.name}@${u.host || u.authDatabase}` }));
  const count = users.length;
  // A PostgreSQL group is a role that can't sign in, so it's a user here, and says so.
  const signIn = users.filter((u) => u.canSignIn).length;
  const aside =
    signIn === count
      ? plural(count, { one: `# user of ${name}`, other: `# users of ${name}` })
      : plural(count, {
          one: `# user of ${name}, ${signIn} of whom can sign in`,
          other: `# users of ${name}, ${signIn} of whom can sign in`,
        });
  return (
    <Section
      id="database-users"
      title={<Trans>Users</Trans>}
      aside={aside}
      action={
        <Button size="sm" icon="plus" onClick={() => setAdding(true)}>
          <Trans>Add a user</Trans>
        </Button>
      }
    >
      {phone ? (
        <PhoneRows
          rows={rows.map((u) => ({
            id: u.id,
            name: u.name,
            sub: [mysql ? u.host : mongo ? u.authDatabase : "", role(u), u.databases.join(", "), u.secret, u.usedBy.join(", ")]
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
          caption={t`Users of ${name}`}
        />
      )}
      {db.engine === DatabaseEngine.POSTGRESQL ? (
        <p className="database-line">
          {t`Over the network, each user signs in with their password. Inside its own container, PostgreSQL's official image lets anything sign in as any user without one.`}
        </p>
      ) : null}
      {adding ? <AddUserSheet db={db} onClose={() => setAdding(false)} /> : null}
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
