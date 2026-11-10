// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { BackupService, type Database, DatabaseEngine, type DatabaseReading } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Metric,
  type MetricProps,
  Panel,
  PHONE_QUERY,
  StatusBadge,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { formatBytes, formatDateTime, formatFixed } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { appOf } from "../apps/app";
import { usePlaces, useServices } from "../apps/useServices";
import { BackUpSheet } from "../backups/BackUpSheet";
import { usePolicies } from "../backups/PoliciesPanel";
import { useMethodWord, useScheduleWords } from "../backups/words";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { CopyButton } from "../system/CopyCommand";
import { useThisDatabase } from "./DatabaseLayout";
import "../servers/ServerMetrics.css";
import { SchemasSection, UsersSection } from "./DatabaseUsers";
import {
  engineName,
  firstReading,
  internalURL,
  policiesOf,
  readOf,
  speaksSQL,
  useBackupHow,
  useOwnInstall,
} from "./useDatabases";

/**
 * A database at a glance (Database-Detail): its numbers as it last said them, how it's
 * backed up, how its services reach it, and who and what is in it.
 */
export function DatabaseOverview() {
  const db = useThisDatabase();
  const reading = firstReading(db);
  const read = readOf(reading);
  return (
    <div className="database-overview">
      {reading && !read ? <UnreadNote db={db} reading={reading} /> : null}
      {read?.checksNoOne ? <ChecksNoOne db={db} /> : null}
      <FromServer db={db} reading={reading} />
      <UpgradeNote db={db} />
      {db.engine === DatabaseEngine.MONGODB && read ? <MongoLine read={read} /> : null}
      <Numbers db={db} read={read} />
      <div className="database-panels">
        <BackupsPanel db={db} />
        <ConnectionPanel db={db} read={read} />
      </div>
      {speaksSQL(db.engine) && read ? (
        <>
          <UsersSection db={db} read={read} />
          <SchemasSection db={db} read={read} />
        </>
      ) : null}
    </div>
  );
}

/**
 * A MongoDB that runs without --auth, as its image does with no root user set:
 * anything that reaches its port signs in as anyone.
 */
function ChecksNoOne({ db }: { db: Database }) {
  const { t } = useLingui();
  const engine = engineName(db.engine);
  return (
    <Callout tone="warning" title={t`${engine} checks no one`}>
      <Trans>
        It runs without --auth, so anything that reaches its port signs in as anyone, with no password. To close it,
        make a user with the root role in it, then set <code>MONGO_INITDB_ROOT_USERNAME</code> and{" "}
        <code>MONGO_INITDB_ROOT_PASSWORD</code> on its app to that user's, which turns --auth on.
      </Trans>
    </Callout>
  );
}

/**
 * What MongoDB says of its upgrades and its backups: the features its data may use,
 * which the release before can still run, and whether a backup is one moment.
 */
function MongoLine({ read }: { read: DatabaseReading }) {
  const { t } = useLingui();
  const version = read.version;
  const features = read.featureCompatibility;
  const release = version.split(".").slice(0, 2).join(".");
  const lines: string[] = [];
  if (features && features !== release) {
    lines.push(
      t`It runs ${version} with ${features}'s features, so ${features} can still run its data. Its next upgrade raises them first.`,
    );
  }
  const set = read.replicaSet;
  lines.push(
    set
      ? t`It's a member of the replica set ${set}, so each backup is one moment.`
      : t`It runs alone, without a replica set, so a backup isn't one moment if something writes while it runs.`,
  );
  return <p className="database-line">{lines.join(" ")}</p>;
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
function UpgradeNote({ db }: { db: Database }) {
  const { t } = useLingui();
  const install = useOwnInstall(db);
  const how = useBackupHow(db);
  const next = install?.upgrade?.version ?? "";
  if (!install?.upgrade) return null;
  const engine = engineName(db.engine);
  const dumpOf = how.database || engine;
  const now = install.pin?.version ?? "";
  return (
    <Callout
      tone="info"
      title={next === now ? t`A change to how ${engine} ${now} runs is out` : t`${engine} ${next} is out`}
    >
      {!how.known ? (
        <Trans>
          Upgrading is a plan that backs up its volumes before anything else, and says which backup holds each. Undoing
          it puts this version back, and leaves the data as the new one left it.
        </Trans>
      ) : how.dump ? (
        <Trans>
          Upgrading is a plan that backs up its data with {dumpOf}'s own dump before anything else, and says which
          backup holds it. Undoing it puts this version back, and leaves the data as the new one left it.
        </Trans>
      ) : (
        <Trans>
          Upgrading is a plan that backs up its volumes before anything else, with a copy of their files, which stops{" "}
          {engine} for a moment, and says which backup holds each. Undoing it puts this version back, and leaves the
          data as the new one left it.
        </Trans>
      )}
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
            Its data is in <code>{volume}</code>, a volume Dockwell didn't make, so Dockwell doesn't back it up. Back it
            up where it came from, or move the data into a volume of the app's.
          </Trans>
        ) : (
          <Trans>
            Its data isn't in a volume, so it goes when its container is replaced, and there's nothing to back up. Give
            it a volume on its app's page.
          </Trans>
        )}
      </p>
    </Panel>
  );
}

/** Why the database said nothing of itself, with what follows from it. */
function UnreadNote({ db, reading }: { db: Database; reading: DatabaseReading }) {
  const { t } = useLingui();
  const engine = engineName(db.engine);
  return (
    <Callout tone="warning" title={t`${engine} didn't say how it is`}>
      <span className="database-said">{reading.unread}.</span>{" "}
      <Trans>Its numbers, databases and users show again once it does.</Trans>
    </Callout>
  );
}

/** Its four numbers, as it said them a moment ago. Nothing keeps their history yet, so there's no trend. */
function Numbers({ db, read }: { db: Database; read: DatabaseReading | undefined }) {
  const { t, i18n } = useLingui();
  const time = useTimeWords();
  const phone = useMediaQuery(PHONE_QUERY);
  const locale = i18n.locale;
  const none = "–";
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
      caption: read ? (most > 0 ? t`The most it takes is ${mostWords}` : t`It sets no most`) : undefined,
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
        : undefined,
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
        : undefined,
    },
    {
      id: "up",
      label: t`Up for`,
      value: read ? up : none,
      caption: started ? t`Since ${started}` : undefined,
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
  const method = useMethodWord();
  const schedule = useScheduleWords();
  const time = useTimeWords();
  const { fleet } = useFleetView();
  const [taking, setTaking] = useState(false);
  const sheet = useChangeSheet();
  const transport = useTransport();
  const restore = (id: string) =>
    sheet.open({ kind: "plan", make: () => createClient(BackupService, transport).restoreBackup({ id }) });
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
            {!how.known
              ? how.failed
                ? t`Its server didn't say`
                : t`Asking its server`
              : how.dump
                ? t`${dumpOf}'s own dump, while it runs: nothing stops`
                : t`A copy of its files, which stops it for an exact copy unless you choose to copy it running`}
          </dd>
        </div>
        <div className="databases-fact">
          <dt>{t`When`}</dt>
          <dd>{policy?.spec ? schedule(policy.spec) : t`When you ask`}</dd>
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
      <div className="databases-panel-actions">
        <Button size="sm" icon="archive" onClick={() => setTaking(true)}>
          <Trans>Back up now</Trans>
        </Button>
        {newest?.onServer ? (
          <Button size="sm" variant="ghost" icon="undo" onClick={() => restore(newest.id)}>
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
    </Panel>
  );
}

/** How the other services in its environment reach it, with the password masked. */
function ConnectionPanel({ db, read }: { db: Database; read: DatabaseReading | undefined }) {
  const { t } = useLingui();
  const places = usePlaces();
  const service = useServices().data?.find((s) => s.id === db.id);
  const place = places.label(db.resource?.scope ?? "");
  const published = (service ? appOf(service) : undefined)?.ports.find((p) => p.containerPort === db.port);
  const url = internalURL(db, read);
  const bare = internalURL(db, read, false);
  const port = String(db.port);
  const host = db.host;
  const where = published ? (published.hostIp ? published.hostIp : t`every address`) : "";
  const serverPort = published ? String(published.hostPort) : "";
  const secret = db.passwordSecret;
  return (
    <Panel
      title={t`Connection`}
      actions={
        <span className="databases-muted">
          <Trans>
            Port <code>{port}</code>
          </Trans>
        </span>
      }
      className="database-connection"
    >
      <div className="database-connection-body">
        <TextField
          label={t`Internal URL`}
          value={url}
          readOnly
          mono
          action={
            <CopyButton
              text={bare}
              label={t`Copy the internal URL, without its password`}
              copiedLabel={t`Copied, without its password`}
              iconOnly
              variant="ghost"
            />
          }
          hint={
            place ? (
              <Trans>
                Services in {place} reach it as <code>{host}</code>
              </Trans>
            ) : (
              <Trans>
                Services beside it reach it as <code>{host}</code>
              </Trans>
            )
          }
        />
        <p className="database-line">
          {db.passwordSecret ? (
            <Trans>
              The password is in the secret <code>{secret}</code>, which you reveal with your passkey on{" "}
              <Link to="/variables">Variables and secrets</Link>.
            </Trans>
          ) : db.engine === DatabaseEngine.REDIS || db.engine === DatabaseEngine.VALKEY ? (
            <Trans>It takes no password Dockwell keeps.</Trans>
          ) : (
            <Trans>The password is in its own environment, which Dockwell keeps sealed and never shows.</Trans>
          )}
        </p>
        <div className="database-rule" />
        <Switch
          label={t`External access`}
          checked={published !== undefined}
          disabled
          description={
            published ? (
              <Trans>
                On. Port {serverPort} of its server reaches it, on {where}. Change it on its app's page.
              </Trans>
            ) : (
              <Trans>Off. Only services on its server's networks can connect. Add a port on its app's page.</Trans>
            )
          }
        />
      </div>
    </Panel>
  );
}
