// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Backup,
  BackupService,
  ChangeService,
  type Database,
  DatabaseEngine,
  type DatabaseReading,
  DatabaseService,
  UpdateSource,
  VariableService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  ConnectionCard,
  type ConnectionItem,
  cx,
  Metric,
  type MetricProps,
  Panel,
  PHONE_QUERY,
  Skeleton,
  StatusBadge,
  Table,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime, formatFixed } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { appOf } from "../apps/app";
import { DangerZone } from "../apps/DangerZone";
import { offered, UpdateCard } from "../apps/updates";
import { usePlaces, useServices } from "../apps/useServices";
import { BackUpSheet } from "../backups/BackUpSheet";
import { useDestinations } from "../backups/DestinationsPanel";
import { usePolicies } from "../backups/PoliciesPanel";
import { useBackUpDaily } from "../backups/PolicySheet";
import { useMethodWord, useScheduleWords } from "../backups/words";
import { CompanionsPanel } from "../catalog/Companions";
import { useUpgradeUnavailable } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { useReveals } from "../variables/useReveal";
import { useThisDatabase } from "./DatabaseLayout";
import { ExternalAccess } from "./ExternalAccess";
import "../servers/ServerMetrics.css";
import { MayEdit } from "../auth/MayEdit";
import { useMay } from "../auth/may";
import { useRestoreWhole } from "../backups/RestoreWhole";
import {
  connectionURL,
  engineName,
  firstReading,
  firstUser,
  policiesOf,
  readOf,
  speaksSQL,
  useBackupHow,
  useDatabaseDeleteUsers,
  useOwnInstall,
  useUnreadWord,
} from "./useDatabases";

/**
 * A database at a glance (Database-Detail): its numbers as it last said them, how it's
 * backed up, how its services reach it, and who and what is in it.
 */
export function DatabaseOverview() {
  const db = useThisDatabase();
  const reading = firstReading(db);
  const read = readOf(reading);
  const install = useOwnInstall(db);
  return (
    <div className="database-overview">
      {reading && !read ? <UnreadNote db={db} reading={reading} /> : null}
      {read?.checksNoOne ? <ChecksNoOne db={db} read={read} /> : null}
      <FromServer db={db} reading={reading} />
      <UpgradeNote db={db} />
      <DatabaseUpdate db={db} />
      {db.engine === DatabaseEngine.MONGODB && read ? <MongoLine read={read} /> : null}
      <Numbers db={db} reading={reading} read={read} />
      <div className="database-panels dw-card-row">
        <BackupsPanel db={db} />
        <ConnectionPanel db={db} read={read} />
      </div>
      <ExternalAccess db={db} />
      <CompanionsPanel install={install} />
      {speaksSQL(db.engine) && read ? <WhoIsIn db={db} read={read} /> : null}
      <DatabaseDanger db={db} />
    </div>
  );
}

/** The first five people and services that can sign in, with the full list one step away. */
function WhoIsIn({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t } = useLingui();
  const users = read.users.filter((u) => !u.system);
  const own = read.databases.filter((d) => !d.system).length;
  const counts = [
    plural(users.length, { one: "# user", other: "# users" }),
    plural(own, { one: "# database of your own", other: "# databases of your own" }),
  ].join(" · ");
  return (
    <Panel
      title={t`Users`}
      flush={users.length > 0}
      actions={
        <RouterButtonLink to="/databases/$serviceId/users" params={{ serviceId: db.id }} size="sm" variant="ghost">
          <Trans>Open users</Trans>
        </RouterButtonLink>
      }
    >
      {users.length > 0 ? (
        <Table
          caption={counts}
          density="compact"
          rows={users.slice(0, 5).map((user) => ({
            ...user,
            id: JSON.stringify([user.name, user.host, user.authDatabase]),
          }))}
          rowKey="id"
          columns={[
            { key: "name", header: t`User`, render: (user) => <code>{user.name}</code> },
            {
              key: "role",
              header: t`Access`,
              render: (user) => (user.superuser ? t`Can do anything` : user.canSignIn ? t`Signs in` : t`Can't sign in`),
            },
            {
              key: "databases",
              header: t`Databases`,
              render: (user) => (user.databases.length > 0 ? <code>{user.databases.join(", ")}</code> : "—"),
            },
          ]}
        />
      ) : (
        <p className="database-line">{t`No users yet`}</p>
      )}
    </Panel>
  );
}

/**
 * Deleting the database, as an app's danger zone has it: the last thing in its project takes the
 * project too, behind a switch that's on (ADR 0074). The users Dockwell made in it go with it, as
 * nothing else can hold them, and its service goes first, so their databases stay in its volume.
 */
function DatabaseDanger({ db }: { db: Database }) {
  const { t } = useLingui();
  const may = useMay();
  const service = useServices().data?.find((s) => s.id === db.id);
  const scope = db.resource?.scope ?? "";
  const { alongside } = useDatabaseDeleteUsers(db);
  if (!service) return null;
  return (
    <MayEdit offer={may(ChangeService.method.proposeChange, { environment: scope })}>
      <DangerZone
        service={service}
        title={t`Danger zone`}
        after="/databases"
        alongside={alongside}
        stays={
          <Trans>
            Stops it and removes its container. Its volume stays, with every database and user in it. You review a plan
            first.
          </Trans>
        }
      />
    </MayEdit>
  );
}

/**
 * A MongoDB that runs without --auth, as its image does with no root user set:
 * anything that reaches its port signs in as anyone.
 */
function ChecksNoOne({ db, read }: { db: Database; read: DatabaseReading }) {
  const { t } = useLingui();
  const engine = engineName(db.engine);
  const set = read.replicaSet;
  return (
    <Callout
      tone="warning"
      title={t`${engine} lets anyone in`}
      actions={
        <RouterButtonLink to="/apps/$serviceId/environment" params={{ serviceId: db.id }} size="sm">
          <Trans>Its environment</Trans>
        </RouterButtonLink>
      }
    >
      {set ? (
        <Trans>
          Give it a <code>--keyFile</code> every member of {set} shares, make a root user, then set{" "}
          <code>MONGO_INITDB_ROOT_USERNAME</code> and <code>MONGO_INITDB_ROOT_PASSWORD</code>.
        </Trans>
      ) : (
        <Trans>
          Make a root user in it, then set <code>MONGO_INITDB_ROOT_USERNAME</code> and{" "}
          <code>MONGO_INITDB_ROOT_PASSWORD</code>.
        </Trans>
      )}
    </Callout>
  );
}

/**
 * What MongoDB says of its upgrades and its backups, as facts on a line: the features
 * its data may use, and whether a backup is one moment and can come back into it.
 */
function MongoLine({ read }: { read: DatabaseReading }) {
  const { t } = useLingui();
  const features = read.featureCompatibility;
  const release = read.version.split(".").slice(0, 2).join(".");
  const set = read.replicaSet;
  const facts = [
    features && features !== release ? t`Data at ${features}'s features, which its next upgrade raises` : "",
    set
      ? t`Replica set ${set}: each backup is one moment, and can't be restored into a member yet`
      : t`Runs alone: a backup isn't one moment while something writes`,
  ].filter(Boolean);
  return <p className="database-line">{facts.join(" · ")}</p>;
}

/** On a database that runs on several servers, which one its numbers are from. */
function FromServer({ db, reading }: { db: Database; reading: DatabaseReading | undefined }) {
  const { t } = useLingui();
  const count = db.readings.length;
  if (count < 2 || !reading) return null;
  const server = reading.serverName;
  return (
    <p className="database-line">{t`Its numbers, databases and users are as ${server} says, one of the ${count} servers it runs on.`}</p>
  );
}

/** A newer release of the catalog entry it came from, and what upgrading does first. */
/**
 * What's newer than the image a database runs, as an app's page has it (ADR 0079). A
 * catalog install's says it in UpgradeNote, with what its plan backs up.
 */
function DatabaseUpdate({ db }: { db: Database }) {
  const service = useServices().data?.find((s) => s.id === db.id);
  if (!service || service.update?.source === UpdateSource.CATALOG || !offered(service.update)) return null;
  return <UpdateCard service={service} />;
}

function UpgradeNote({ db }: { db: Database }) {
  const { t } = useLingui();
  const install = useOwnInstall(db);
  const how = useBackupHow(db);
  const next = install?.upgrade?.version ?? "";
  // A release that can't be planned where it runs is no upgrade to take: the page's
  // header says why, on every tab, and a phone's, which has no room for it, here
  // (#771's review, M2).
  const unavailableOf = useUpgradeUnavailable();
  const unavailable = install ? unavailableOf(install) : "";
  const phone = useMediaQuery(PHONE_QUERY);
  if (!install?.upgrade || (unavailable && !phone)) return null;
  const engine = engineName(db.engine);
  const dumpOf = how.database || engine;
  const now = install.pin?.version ?? "";
  return (
    <Callout
      tone="info"
      title={next === now ? t`A change to how ${engine} ${now} runs is out` : t`${engine} ${next} is out`}
    >
      {unavailable
        ? `${unavailable}.`
        : how.known && how.dump
          ? t`Its plan backs up the data with ${dumpOf}'s dump first; undoing it keeps the data as ${next} left it.`
          : t`Its plan backs up its volumes first; undoing it keeps the data as ${next} left it.`}
    </Callout>
  );
}

/** Whether Dockwell backs a database up: its data is in a volume Dockwell made. */
export function backsUp(db: Database): boolean {
  return db.volume !== "" && !db.volumeExternal && db.volumeId !== "";
}

/** Why a database has no backups, and what to do about it. */
export function NoBackups({ db, className }: { db: Database; className?: string }) {
  const { t } = useLingui();
  const volume = db.volume;
  return (
    <Panel title={t`Backups`} actions={<StatusBadge status="stopped" label={t`None`} />} className={className}>
      <p className="database-line">
        {db.volumeExternal ? (
          <Trans>
            Its data is in <code>{volume}</code>, a volume Dockwell didn't make, which it doesn't back up.
          </Trans>
        ) : (
          <Trans>Its data isn't in a volume, so it goes with its container.</Trans>
        )}
      </p>
      {db.volumeExternal ? null : (
        <div className="databases-panel-actions dw-card-footer">
          <RouterButtonLink to="/apps/$serviceId/settings" params={{ serviceId: db.id }} size="sm">
            <Trans>Give it a volume</Trans>
          </RouterButtonLink>
        </div>
      )}
    </Panel>
  );
}

/** Why the database said nothing of itself, with what follows from it. */
function UnreadNote({ db, reading }: { db: Database; reading: DatabaseReading }) {
  const { t } = useLingui();
  const engine = engineName(db.engine);
  return (
    <Callout tone="warning" title={t`${engine} didn't say how it is`}>
      {/* What Docker or the database said can run to a whole command line: its first clause, and the rest on asking. */}
      <span className="database-said">{gist(reading.unread)}</span>
      {gist(reading.unread) !== reading.unread ? (
        <details className="database-details">
          <summary>
            <Trans context="what a database or Docker said when it couldn't be read">What it said</Trans>
          </summary>
          <span className="database-said">{reading.unread}</span>
        </details>
      ) : null}
    </Callout>
  );
}

/**
 * The first sentence of what a database or Docker said, or its first clause, as what
 * follows can be a whole command line, with its first letter a capital.
 */
export function gist(said: string): string {
  const clause = said.split(/(?<=\.) |: /, 1)[0] ?? said;
  const short = clause.replace(/\.$/, "");
  return short.charAt(0).toUpperCase() + short.slice(1);
}

/** Its four numbers, as it said them a moment ago. Nothing keeps their history yet, so there's no trend. */
function Numbers({
  db,
  reading,
  read,
}: {
  db: Database;
  reading: DatabaseReading | undefined;
  read: DatabaseReading | undefined;
}) {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const unreadWord = useUnreadWord();
  const phone = useMediaQuery(PHONE_QUERY);
  const locale = i18n.locale;
  const none = "—";
  const unreadCaption = read ? undefined : unreadWord(reading?.unread);
  const sql = speaksSQL(db.engine);
  const connections = Number(read?.connections ?? 0n);
  const most = Number(read?.maxConnections ?? 0n);
  const [size, unit] = read ? splitBytes(formatBytes(read.sizeBytes, locale)) : [none, ""];
  const databases = read?.databases.length ?? 0;
  const keys = Number(read?.keys ?? 0n);
  const up = read ? time.span(Number(read.uptimeSeconds) * 1000) : "";
  const started = read?.readTime
    ? formatDateTime(new Date(timestampMs(read.readTime) - Number(read.uptimeSeconds) * 1000), locale)
    : "";
  const mostWords = most.toLocaleString(locale);
  const metrics: (MetricProps & { id: string })[] = [
    {
      id: "connections",
      label: t`Connections`,
      value: read ? connections.toLocaleString(locale) : none,
      unit: read && most > 0 ? t`of ${mostWords}` : undefined,
      caption: read ? (most > 0 ? t`The most it takes is ${mostWords}` : t`It sets no most`) : unreadCaption,
    },
    {
      id: "size",
      label: t({ message: "Size", context: "how much data" }),
      value: size,
      unit,
      caption: read
        ? sql
          ? plural(databases, { one: "On disk, in # database", other: "On disk, across # databases" })
          : plural(keys, { one: "In memory, with # key", other: "In memory, with # keys" })
        : unreadCaption,
    },
    {
      id: "cache",
      label: t`Cache hit`,
      value: read?.cacheHit !== undefined ? formatFixed(read.cacheHit * 100, 1, locale) : none,
      unit: read?.cacheHit !== undefined ? "%" : undefined,
      caption: read
        ? read.cacheHit !== undefined
          ? sql
            ? t`Reads served from memory`
            : t`Keys asked for that were there`
          : t`Nothing read yet`
        : unreadCaption,
    },
    {
      id: "up",
      label: t`Up for`,
      value: read ? up : none,
      caption: read ? (started ? t`Since ${started}` : undefined) : unreadCaption,
    },
  ];
  return (
    <div className={cx("metrics", phone && "is-compact")}>
      {metrics.map(({ id, caption, ...metric }) => (
        <div key={id} className="dw-panel metrics-panel">
          <Metric {...metric} caption={phone ? undefined : caption} />
        </div>
      ))}
    </div>
  );
}

/** "4.8" and "GB" of "4.8 GB", as a metric shows a number and its unit apart. */
function splitBytes(words: string): [string, string] {
  const i = words.lastIndexOf(" ");
  return i < 0 ? [words, ""] : [words.slice(0, i), words.slice(i + 1)];
}

/** How the database is backed up, and the way to take one now. */
function BackupsPanel({ db }: { db: Database }) {
  const { t, i18n } = useLingui();
  const policies = usePolicies().data?.policies ?? [];
  const destinations = useDestinations().data?.destinations ?? [];
  const backUpDaily = useBackUpDaily();
  const method = useMethodWord();
  const schedule = useScheduleWords();
  const time = useTimeWords();
  const { fleet } = useFleetView();
  const [taking, setTaking] = useState(false);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { environment: db.resource?.scope ?? "" });
  const whole = useRestoreWhole();
  const restore = (backup: Backup) => whole.restore(backup);
  const engine = engineName(db.engine);
  const how = useBackupHow(db);
  const dumpOf = how.database || engine;
  const policy = policiesOf(db, policies)[0];
  const newest = db.newestBackup;
  const latest = db.latestBackup;
  const latestError = latest?.error ?? "";
  const servers = db.readings.map((r) => r.serverName).join(", ");
  const destination = policy?.spec?.destination ?? "";
  if (!backsUp(db)) return <NoBackups db={db} className="database-backups" />;
  const took =
    newest?.startedAt && newest.finishedAt
      ? time.span(timestampMs(newest.finishedAt) - timestampMs(newest.startedAt))
      : "";
  return (
    <Panel
      title={t`Backups`}
      actions={
        policy ? (
          <StatusBadge status="healthy" label={t`On schedule`} />
        ) : (
          <StatusBadge status="stopped" label={t`Only by hand`} />
        )
      }
      className="database-backups"
    >
      <dl className="databases-facts">
        <div className="databases-fact">
          <dt>{t`How`}</dt>
          <dd>
            {!how.known ? (
              how.failed ? (
                t`Its server didn't say`
              ) : (
                <Skeleton width={200} inline />
              )
            ) : how.dump ? (
              t`${dumpOf}'s own dump, while it runs: nothing stops`
            ) : (
              t`A copy of its files, which stops it a moment`
            )}
          </dd>
        </div>
        <div className="databases-fact">
          <dt>{t`When`}</dt>
          <dd className="databases-fact-inline">
            {policy?.spec ? (
              schedule(policy.spec)
            ) : (
              <>
                <span>{t`Not scheduled`}</span>
                <span className="databases-muted" aria-hidden="true">
                  ·
                </span>
                <button
                  type="button"
                  className="databases-inline-act"
                  disabled={!mayPlan.allowed}
                  title={mayPlan.reason || undefined}
                  onClick={() =>
                    backUpDaily(
                      [{ id: db.volumeId, name: db.volume, environment: db.resource?.scope ?? "" }],
                      destinations,
                    )
                  }
                >
                  <Trans>Back up daily</Trans>
                </button>
              </>
            )}
          </dd>
        </div>
        <div className="databases-fact">
          <dt>{t`Newest`}</dt>
          <dd>
            {newest?.startedAt
              ? [
                  formatDateTime(timestampDate(newest.startedAt), i18n.locale),
                  method(newest.method, newest.database),
                  formatBytes(newest.sizeBytes, i18n.locale),
                  took,
                ]
                  .filter(Boolean)
                  .join(" · ")
              : t`None yet`}
          </dd>
        </div>
        {latest ? (
          <div className="databases-fact">
            <dt>{t`Since`}</dt>
            <dd>{latestError ? t`The last one failed: ${latestError}` : t`One is running now`}</dd>
          </div>
        ) : null}
        <div className="databases-fact">
          <dt>{t`Kept on`}</dt>
          <dd>
            {destination
              ? t`${servers}, and sealed on ${destination}`
              : fleet
                ? t`${servers}, unencrypted`
                : t`This server's disk, unencrypted`}
          </dd>
        </div>
      </dl>
      <div className="databases-panel-actions dw-card-footer">
        <Button size="sm" icon="archive" onClick={() => setTaking(true)}>
          <Trans>Back up now</Trans>
        </Button>
        {newest?.onServer ? (
          <Button
            size="sm"
            variant="ghost"
            icon="undo"
            onClick={() => restore(newest)}
            {...may(BackupService.method.restoreBackup, { environment: newest.environmentId }).off}
          >
            <Trans>Restore the newest</Trans>
          </Button>
        ) : null}
        <RouterButtonLink to="/databases/$serviceId/backups" params={{ serviceId: db.id }} variant="ghost" size="sm">
          <Trans>All its backups</Trans>
        </RouterButtonLink>
      </div>
      {taking ? (
        <BackUpSheet
          volumes={[{ id: db.volumeId, name: db.volume, environment: db.resource?.scope ?? "" }]}
          chosen={db.volumeId}
          onClose={() => setTaking(false)}
        />
      ) : null}
      {whole.confirming}
    </Panel>
  );
}

/**
 * Everything an app needs to connect, to copy rather than read: where it answers, as
 * whom, and the password, masked until it's revealed with a passkey (ADR 0022), with a
 * connection string that holds them all.
 */
function ConnectionPanel({ db, read }: { db: Database; read: DatabaseReading | undefined }) {
  const { t } = useLingui();
  const places = usePlaces();
  const service = useServices().data?.find((s) => s.id === db.id);
  const environment = db.resource?.scope ?? "";
  const place = places.label(environment);
  const published = (service ? appOf(service) : undefined)?.ports.find((p) => p.containerPort === db.port);
  const secret = db.passwordSecret;
  const variables = useQuery(
    VariableService.method.listVariables,
    { environment },
    { enabled: secret !== "" && environment !== "" },
  );
  const secretId = variables.data?.variables.find((v) => v.secret && v.name === secret)?.id ?? "";
  const reveals = useReveals();
  const shown = secretId ? reveals.get(secretId) : {};
  const may = useMay();
  const where = { environment };
  const revealOff = may(VariableService.method.revealSecret, where).off.off;
  const sql = speaksSQL(db.engine);
  const sheet = useChangeSheet();
  const transport = useTransport();
  // A new password, generated by the control plane, in the database and its secret at once (ADR 0038).
  const changePassword = () =>
    sheet.open({
      kind: "plan",
      make: () => createClient(DatabaseService, transport).changeDatabasePassword({ id: db.id }),
      onClose: () => {
        if (secretId) reveals.hide(secretId);
      },
    });
  const items: ConnectionItem[] = [
    {
      label: t`Host`,
      value: db.host,
      hint: place ? t`From apps in ${place}` : t`From apps beside it`,
    },
    { label: t`Port`, value: String(db.port) },
    // One that checks no one has no user to sign in as.
    ...(sql && firstUser(db, read) && !read?.checksNoOne ? [{ label: t`User`, value: firstUser(db, read) }] : []),
    ...(published
      ? [
          {
            label: t`On the server`,
            value: `${published.hostIp && published.hostIp !== "0.0.0.0" ? published.hostIp : "127.0.0.1"}:${published.hostPort}`,
            hint:
              published.hostIp === "127.0.0.1" ? t`From this server alone` : t`From anywhere that reaches this server`,
          },
        ]
      : []),
  ];
  return (
    <ConnectionCard
      className="database-connection"
      items={items}
      secret={
        secret && !read?.checksNoOne
          ? {
              label: t`Password`,
              value: shown.value,
              busy: shown.busy ?? false,
              problem: shown.problem ?? (variables.error ? sentence(variables.error) : undefined),
              hint: (
                <Trans>
                  Kept in the secret <code>{secret}</code>
                </Trans>
              ),
              revealOff,
              // MongoDB keeps its root user's password in its data, which a new secret doesn't change.
              action:
                db.engine === DatabaseEngine.MONGODB ? undefined : (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={changePassword}
                    {...may(DatabaseService.method.changeDatabasePassword, where).off}
                  >
                    <Trans>Change password</Trans>
                  </Button>
                ),
              onReveal: () => {
                if (secretId) reveals.reveal(secretId);
              },
              onHide: () => {
                if (secretId) reveals.hide(secretId);
              },
            }
          : undefined
      }
      note={
        read?.checksNoOne
          ? t`It takes no password, as it checks no one.`
          : !secret && sql
            ? t`Its password is in its own environment, sealed, so it has no secret to reveal.`
            : undefined
      }
      connectionString={{ label: t`Connection string`, build: (password) => connectionURL(db, read, password) }}
    />
  );
}
