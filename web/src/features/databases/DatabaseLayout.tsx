// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { CatalogService, type Database, DatabaseEngine, TerminalService } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  PHONE_QUERY,
  SkeletonCard,
  SkeletonFacts,
  SkeletonTabs,
  TabNav,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { createContext, type ReactNode, use } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { environmentOf, nameOf } from "../apps/app";
import { HealthBadge, useHealthTitle, useHealthWords } from "../apps/Health";
import { useHealth } from "../apps/health";
import { useShownName } from "../apps/names";
import { usePlaces, useServices } from "../apps/useServices";
import { useSignedIn } from "../auth/useAuthState";
import { OpenAppButton } from "../catalog/access";
import { useServiceLabel } from "../catalog/Companions";
import { useUpgradeUnavailable } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAgoNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import {
  consoleTarget,
  engineAndVersion,
  firstReading,
  makesUsers,
  readOf,
  speaksSQL,
  useDatabase,
  useOwnInstall,
} from "./useDatabases";
import "../apps/Apps.css";
import "./Databases.css";
import { useMay } from "../auth/may";

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
      <Page title={t`Database`} pending={query.isPending} breadcrumb={<Crumbs />} backLink={<BackToDatabases />}>
        {query.isPending ? (
          <Loading label={t`Loading the database`} className="database-loading">
            <SkeletonTabs count={3} />
            <div className="database-skeleton-metrics">
              <SkeletonCard noHead height={64} />
              <SkeletonCard noHead height={64} />
              <SkeletonCard noHead height={64} />
            </div>
            <SkeletonCard>
              <SkeletonFacts count={6} />
            </SkeletonCard>
          </Loading>
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

/** The breadcrumb: Databases, its project once there are two, as a link (ADR 0074), and its name. */
function Crumbs({ name, project }: { name?: string; project?: { id: string; label: string } | undefined }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/databases">
        <Trans>Databases</Trans>
      </Link>
      {project ? (
        <>
          <span aria-hidden="true" className="database-crumb-slash">
            /
          </span>
          <Link to="/projects/$projectId" params={{ projectId: project.id }}>
            {project.label}
          </Link>
        </>
      ) : null}
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
  // A tool beside it goes by its own name, such as Adminer, rather than its service's (ADR 0076).
  const label = useServiceLabel(install);
  // The admin tool a catalog install runs beside it opens through the panel (ADR 0061).
  const onLocalhost = useSignedIn()?.rpId === "localhost";
  const tool = useServices().data?.find((s) => s.id === install?.webServiceId && s.id !== db.id);
  const next = install?.upgrade?.version ?? "";
  // A release that can't be planned where it runs offers nothing to take; the note
  // under the header says why (#771's review, M2).
  const unavailableOf = useUpgradeUnavailable();
  const unavailable = install ? unavailableOf(install) : "";
  const upgrade =
    install?.upgrade && !unavailable
      ? () =>
          sheet.open({
            kind: "plan",
            make: () => createClient(CatalogService, transport).upgradeCatalogInstall({ id: install.projectId }),
          })
      : undefined;
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay();
  const mayUpgrade = may(CatalogService.method.upgradeCatalogInstall, { project: install?.projectId ?? "" });
  const mayConsole = may(TerminalService.method.openTerminal);
  const consoleAt = consoleTarget(db, health);
  const upgradeWords =
    next && next === install?.pin?.version ? (
      <Trans>Review the change to {next}</Trans>
    ) : (
      <Trans>Upgrade to {next}</Trans>
    );
  const backedUp = db.volumeId !== "";
  const newest = db.newestBackup?.startedAt ? timestampDate(db.newestBackup.startedAt) : undefined;
  const now = useAgoNow(newest);
  // A catalog install's database goes by the name the person gave the install (#796).
  const shown = useShownName()(db, "database");
  const name = shown.title;
  const read = readOf(firstReading(db));
  const place = places.place(db.resource?.scope ?? "");
  const users = read?.users.filter((u) => !u.system).length ?? 0;
  const logicals = read?.databases.filter((d) => !d.system).length ?? 0;
  const placeName = place?.label ?? "";
  const others = db.beside.map(label).join(", ");
  const ago = newest ? time.ago(newest, now) : "";
  const app = shown.app;
  const lead = [
    engineAndVersion(db, read),
    // The title is the install's own name already, and beside says the app it serves.
    !app && place && !place.isDefault ? t`in ${placeName}` : "",
    others ? t`beside ${others}` : "",
    !db.volume ? t`not in a volume` : newest ? t`backed up ${ago}` : db.volumeExternal ? "" : t`not backed up yet`,
    unavailable,
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <Page
      title={name}
      machineTitle
      breadcrumb={
        <Crumbs
          name={name}
          project={place && !place.isDefault && placeName !== name ? { id: place.scope, label: placeName } : undefined}
        />
      }
      badge={<HealthBadge health={health} title={healthTitle} />}
      lead={lead}
      phoneSub={[engineAndVersion(db, read), healthWords.word(health)].join(" · ")}
      backLink={<BackToDatabases />}
      actions={
        <>
          {tool && onLocalhost ? (
            <OpenAppButton serviceId={tool.id} environment={environmentOf(tool)} app={label(nameOf(tool))} />
          ) : null}
          {mayConsole.allowed && consoleAt ? (
            <RouterButtonLink
              to="/servers/$serverId/terminal"
              params={{ serverId: consoleAt.serverId }}
              search={{ container: consoleAt.container, console: true }}
              icon="terminal"
            >
              <Trans>Open console</Trans>
            </RouterButtonLink>
          ) : mayConsole.allowed ? (
            <Button icon="terminal" disabled title={t`The database container isn't running on a server yet`}>
              <Trans>Open console</Trans>
            </Button>
          ) : null}
          <RouterButtonLink to="/apps/$serviceId" params={{ serviceId: db.id }} icon="service">
            <Trans>Open as an app</Trans>
          </RouterButtonLink>
          {upgrade ? (
            <Button variant="primary" icon="deploy" onClick={upgrade} {...mayUpgrade.off}>
              {upgradeWords}
            </Button>
          ) : null}
        </>
      }
      className="database-page"
    >
      {/* A phone has no header row for these, so they lead the page, not trail its Danger zone (#798). */}
      {phone ? (
        <div className="database-actionbar">
          {mayConsole.allowed && consoleAt ? (
            <RouterButtonLink
              to="/servers/$serverId/terminal"
              params={{ serverId: consoleAt.serverId }}
              search={{ container: consoleAt.container, console: true }}
              icon="terminal"
              size="lg"
            >
              <Trans>Open console</Trans>
            </RouterButtonLink>
          ) : null}
          <RouterButtonLink to="/apps/$serviceId" params={{ serviceId: db.id }} icon="service" size="lg">
            <Trans>Open as an app</Trans>
          </RouterButtonLink>
          {upgrade ? (
            <Button variant="primary" icon="deploy" size="lg" onClick={upgrade} {...mayUpgrade.off}>
              {upgradeWords}
            </Button>
          ) : null}
        </div>
      ) : null}
      <TabNav label={t`Database sections`}>
        <RouterTabLink to="/databases/$serviceId" params={{ serviceId: db.id }} activeOptions={{ exact: true }}>
          <Trans>Overview</Trans>
        </RouterTabLink>
        {makesUsers(db.engine) || db.engine === DatabaseEngine.CLICKHOUSE ? (
          <RouterTabLink
            to="/databases/$serviceId/databases"
            params={{ serviceId: db.id }}
            {...(read ? { count: logicals } : {})}
          >
            <Trans>Databases</Trans>
          </RouterTabLink>
        ) : null}
        {backedUp ? (
          <RouterTabLink to="/databases/$serviceId/backups" params={{ serviceId: db.id }}>
            <Trans>Backups</Trans>
          </RouterTabLink>
        ) : null}
        {speaksSQL(db.engine) && db.engine !== DatabaseEngine.CLICKHOUSE ? (
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
    </Page>
  );
}
