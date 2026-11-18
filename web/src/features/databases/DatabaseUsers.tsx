// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type Database,
  type DatabaseAccount,
  DatabaseEngine,
  type DatabaseReading,
  DatabaseService,
  type InnerDatabase,
} from "@dockwell/api";
import { Button, Callout, Panel, PHONE_QUERY, Table, type TableColumn, Tag, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";
import { formatBytes } from "../../lib/format";
import { StateLine } from "../apps/DomainStep";
import { useShownName } from "../apps/names";
import { useServicesNamed } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu } from "../shell/ActionMenu";
import { RefLink } from "../shell/RefLink";
import { AddUserSheet } from "./AddUserSheet";
import { useThisDatabase } from "./DatabaseLayout";
import { UseInAppSheet } from "./UseInAppSheet";
import { engineName, firstReading, makesUsers, readOf } from "./useDatabases";

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
  const sheet = useChangeSheet();
  const transport = useTransport();
  const may = useMay();
  const where = { environment: db.resource?.scope ?? "" };
  const mayChange = may(DatabaseService.method.changeDatabasePassword, where);
  const mayConnect = may(DatabaseService.method.connectDatabase, where);
  const mayRemove = may(ChangeService.method.proposeChange, where);
  const [using, setUsing] = useState<DatabaseAccount>();
  const navigate = useNavigate();
  const named = useServicesNamed(db.resource?.scope);
  const users = read.users.filter((u) => !u.system);
  const mysql = db.engine === DatabaseEngine.MYSQL || db.engine === DatabaseEngine.MARIADB;
  const mongo = db.engine === DatabaseEngine.MONGODB;
  // Where Dockwell makes no users yet, there's nothing to add and no secret to name.
  const makes = makesUsers(db.engine);
  // As its page is titled: an install's database by the install's name (#796).
  const name = useShownName()(db, "database").title;
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
    ...(makes
      ? ([
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
                  {u.usedBy.map((app) => {
                    const service = named.get(app);
                    return service ? (
                      <RefLink key={app} kind="app" id={service.id}>
                        {app}
                      </RefLink>
                    ) : (
                      <Tag key={app}>{app}</Tag>
                    );
                  })}
                </span>
              ) : (
                <span className="databases-muted">{u.secret ? t`No app yet` : ""}</span>
              ),
          },
        ] satisfies TableColumn<DatabaseAccount & { id: string }>[])
      : []),
  ];
  // A new generated password, in the database and the secret at once, through a plan:
  // for a user Dockwell made, by name, and for its first user, whose is the database's own.
  const changePassword = (user: string) =>
    sheet.open({
      kind: "plan",
      make: () => createClient(DatabaseService, transport).changeDatabasePassword({ id: db.id, user }),
    });
  const first = (u: DatabaseAccount) => !u.resourceId && u.secret !== "" && u.secret === db.passwordSecret;
  // A user Dockwell made goes as its resource does, which drops them in the database too.
  const remove = (u: DatabaseAccount) => {
    const who = u.name;
    sheet.open({
      kind: "propose",
      summary: t`Remove the database user ${who}`,
      edits: [{ resource: { kind: "database.user", scope: db.resource?.scope ?? "", name: who }, delete: true }],
    });
  };
  const menu = (u: DatabaseAccount) => {
    const who = u.name;
    // MongoDB keeps its first user's password in its data, which a new secret doesn't change.
    const changes = u.resourceId !== "" || (first(u) && !mongo);
    return (
      <ActionMenu
        label={t`Actions for ${who}`}
        actions={[
          {
            id: "password",
            icon: "key",
            label: t`Change password`,
            off: !changes ? t`Its password isn't one Dockwell keeps` : mayChange.allowed ? undefined : mayChange.reason,
            onSelect: () => changePassword(u.resourceId ? who : ""),
          },
          {
            id: "use",
            icon: "link",
            label: t`Use in an app`,
            off: u.superuser
              ? t`One who can do anything isn't an app's. Add a user for it`
              : !u.canSignIn
                ? t`They can't sign in`
                : mayConnect.allowed
                  ? undefined
                  : mayConnect.reason,
            onSelect: () => setUsing(u),
          },
          {
            id: "remove",
            icon: "trash",
            label: t`Remove`,
            danger: true,
            off: !u.resourceId
              ? t`Dockwell didn't make ${who}, so it doesn't remove them`
              : mayRemove.allowed
                ? undefined
                : mayRemove.reason,
            onSelect: () => remove(u),
          },
        ]}
      />
    );
  };
  if (makes)
    columns.push({
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 56,
      align: "right",
      render: menu,
    });
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
        makes ? (
          <Button size="sm" icon="plus" onClick={() => setAdding(true)}>
            <Trans>Add a user</Trans>
          </Button>
        ) : undefined
      }
    >
      {phone ? (
        <PhoneRows
          rows={rows.map((u) => ({
            id: u.id,
            name: u.name,
            sub: [
              mysql ? u.host : mongo ? u.authDatabase : "",
              role(u),
              u.databases.join(", "),
              u.secret,
              u.usedBy.join(", "),
            ]
              .filter(Boolean)
              .join(" · "),
            action: makes ? menu(u) : undefined,
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
      {/* A fact, not a fault: as a state line, with why one step away (#798). */}
      {db.engine === DatabaseEngine.POSTGRESQL ? <TrustLine /> : null}
      {mongo ? (
        <p className="database-line">
          <Trans>
            Dockwell can't add a MongoDB user yet. Add one with <code>db.createUser</code> in <code>mongosh</code>.
          </Trans>
        </p>
      ) : null}
      {adding ? <AddUserSheet db={db} onClose={() => setAdding(false)} /> : null}
      {using ? (
        <UseInAppSheet
          db={db}
          user={using}
          onClose={() => setUsing(undefined)}
          onAddVariable={(serviceId) => {
            setUsing(undefined);
            void navigate({ to: "/apps/$serviceId/environment", params: { serviceId } });
          }}
        />
      ) : null}
    </Section>
  );
}

/** That PostgreSQL's image asks no password of what runs in its own container, and why that's so. */
function TrustLine() {
  const { t } = useLingui();
  const [why, setWhy] = useState(false);
  return (
    <div className="database-trust">
      <StateLine
        status="stopped"
        action={
          <button type="button" className="domain-step-act" aria-expanded={why} onClick={() => setWhy(!why)}>
            {why ? <Trans>Hide why</Trans> : <Trans>Why</Trans>}
          </button>
        }
      >
        {t`Apps sign in with a password. Inside its own container, nothing asks for one`}
      </StateLine>
      {why ? (
        <p className="database-trust-why">
          {t`PostgreSQL's image trusts what runs in its container, as only a shell there reaches it that way.`}
        </p>
      ) : null}
    </div>
  );
}

/** The databases in it, each with who owns it and how much it takes. */
export function SchemasSection({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  // As its page is titled: an install's database by the install's name (#796).
  const name = useShownName()(db, "database").title;
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
