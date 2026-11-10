// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { CatalogService, type Database } from "@dockwell/api";
import { Button, Callout, Icon, PHONE_QUERY, TabNav, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { createContext, type ReactNode, use } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { HealthBadge, useHealthTitle, useHealthWords } from "../apps/Health";
import { useHealth } from "../apps/health";
import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAgoNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { engineAndVersion, firstReading, readOf, speaksSQL, useDatabase, useOwnInstall } from "./useDatabases";
import "../apps/Apps.css";
import "./Databases.css";

const DatabaseContext = createContext<Database | undefined>(undefined);

/** The database whose page this is, for its tabs. */
export function useThisDatabase(): Database {
  const db = use(DatabaseContext);
  if (!db) throw new Error("useThisDatabase outside a database's page");
  return db;
}

/**
 * One database's frame, whichever tab is open: where it sits, its name and how it
 * runs, what it is, and its tabs, each a route of its own under /databases/<id>. The
 * tabs read the database from here, so moving between them reads nothing again.
 */
export function DatabaseLayout({ serviceId, children }: { serviceId: string; children: ReactNode }) {
  const { t } = useLingui();
  const query = useDatabase(serviceId);
  const db = query.data?.database;
  if (!db) {
    const gone = query.error instanceof ConnectError && query.error.code === Code.NotFound;
    return (
      <Page title={t`Database`} breadcrumb={<Crumbs />} backLink={<BackToDatabases />}>
        {query.isPending ? (
          <div className="database-loading" aria-busy="true">
            <span className="dw-sr">
              <Trans>Loading the database</Trans>
            </span>
            <div className="databases-skeleton-block is-tabs" />
            <div className="databases-skeleton-block is-metrics" />
            <div className="databases-skeleton-block is-panel" />
          </div>
        ) : gone || !query.error ? (
          <EmptyState
            icon="database"
            title={<Trans>No database with this ID</Trans>}
            action={
              <RouterButtonLink to="/databases" size="lg">
                <Trans>Go to the databases</Trans>
              </RouterButtonLink>
            }
          >
            <p>
              <Trans>
                <code>{serviceId}</code> may have been deleted, or it may no longer run a database Dockwell knows.
              </Trans>
            </p>
          </EmptyState>
        ) : (
          <Callout tone="critical" title={t`Can't load this database`}>
            {sentence(query.error)} <Trans>Nothing changed on the server. The page tries again by itself.</Trans>
          </Callout>
        )}
      </Page>
    );
  }
  return (
    <DatabaseContext value={db}>
      <DatabaseFrame db={db}>{children}</DatabaseFrame>
    </DatabaseContext>
  );
}

function Crumbs({ name }: { name?: string }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/databases">
        <Trans>Databases</Trans>
      </Link>
      {name ? (
        <>
          <span aria-hidden="true" className="database-crumb-slash">
            /
          </span>
          <span aria-current="page" className="database-crumb-name">
            {name}
          </span>
        </>
      ) : null}
    </nav>
  );
}

function BackToDatabases() {
  const { t } = useLingui();
  return (
    <Link to="/databases" className="mobile-header-button" aria-label={t`Back`}>
      <Icon name="arrow-left" size={20} />
    </Link>
  );
}

function DatabaseFrame({ db, children }: { db: Database; children: ReactNode }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const health = useHealth().of({ id: db.id });
  const healthTitle = useHealthTitle(health);
  const healthWords = useHealthWords();
  const places = usePlaces();
  const time = useTimeWords();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const install = useOwnInstall(db);
  const next = install?.upgrade?.version ?? "";
  const upgrade = install?.upgrade
    ? () =>
        sheet.open({
          kind: "plan",
          make: () => createClient(CatalogService, transport).upgradeCatalogInstall({ id: install.projectId }),
        })
    : undefined;
  const upgradeWords =
    next && next === install?.pin?.version ? (
      <Trans>Review the change to {next}</Trans>
    ) : (
      <Trans>Upgrade to {next}</Trans>
    );
  const backedUp = db.volumeId !== "";
  const newest = db.newestBackup?.startedAt ? timestampDate(db.newestBackup.startedAt) : undefined;
  const now = useAgoNow(newest);
  const name = db.resource?.name ?? db.id;
  const read = readOf(firstReading(db));
  const place = places.place(db.resource?.scope ?? "");
  const users = read?.users.filter((u) => !u.system).length ?? 0;
  const placeName = place?.label ?? "";
  const others = db.beside.join(", ");
  const ago = newest ? time.ago(newest, now) : "";
  const lead = [
    engineAndVersion(db, read),
    place && !place.isDefault ? t`in ${placeName}` : "",
    others ? t`beside ${others}` : "",
    !db.volume ? t`not in a volume` : newest ? t`backed up ${ago}` : db.volumeExternal ? "" : t`not backed up yet`,
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <Page
      title={name}
      machineTitle
      breadcrumb={<Crumbs name={name} />}
      badge={<HealthBadge health={health} title={healthTitle} />}
      lead={lead}
      phoneSub={[engineAndVersion(db, read), healthWords.word(health)].join(" · ")}
      backLink={<BackToDatabases />}
      actions={
        <>
          <RouterButtonLink to="/apps/$serviceId" params={{ serviceId: db.id }} icon="service">
            <Trans>Open as an app</Trans>
          </RouterButtonLink>
          {upgrade ? (
            <Button variant="primary" icon="deploy" onClick={upgrade}>
              {upgradeWords}
            </Button>
          ) : null}
        </>
      }
      className="database-page"
    >
      <TabNav label={t`Database sections`}>
        <RouterTabLink to="/databases/$serviceId" params={{ serviceId: db.id }} activeOptions={{ exact: true }}>
          <Trans>Overview</Trans>
        </RouterTabLink>
        {backedUp ? (
          <RouterTabLink to="/databases/$serviceId/backups" params={{ serviceId: db.id }}>
            <Trans>Backups</Trans>
          </RouterTabLink>
        ) : null}
        {speaksSQL(db.engine) ? (
          <RouterTabLink
            to="/databases/$serviceId/users"
            params={{ serviceId: db.id }}
            {...(read ? { count: users } : {})}
          >
            <Trans>Users</Trans>
          </RouterTabLink>
        ) : null}
      </TabNav>
      {children}
      {phone ? (
        <div className="database-actionbar">
          <RouterButtonLink to="/apps/$serviceId" params={{ serviceId: db.id }} icon="service" size="lg">
            <Trans>Open as an app</Trans>
          </RouterButtonLink>
          {upgrade ? (
            <Button variant="primary" icon="deploy" size="lg" onClick={upgrade}>
              {upgradeWords}
            </Button>
          ) : null}
        </div>
      ) : null}
    </Page>
  );
}
