// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import {
  type Backup,
  BackupState,
  CatalogService,
  type Database,
  type Entry,
  type Event,
  EventKind,
  EventService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  Timeline,
  type TimelineEvent,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueries } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { happenedAt, useDescribe } from "../activity/describe";
import { HealthBadge, useHealthWords } from "../apps/Health";
import { type HealthView, useHealth } from "../apps/health";
import { usePlaces, useServicesNamed } from "../apps/useServices";
import { BackUpSheet } from "../backups/BackUpSheet";
import { usePolicies } from "../backups/PoliciesPanel";
import type { VolumeChoice } from "../backups/useBackups";
import { useMethodWord, useScheduleWords } from "../backups/words";
import { InstallSheet } from "../catalog/InstallSheet";
import { useCatalog } from "../catalog/useCatalog";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { Meter } from "./Meter";
import {
  engineAndVersion,
  engineName,
  firstReading,
  policiesOf,
  readOf,
  type Stranger,
  useDatabases,
  useStrangers,
} from "./useDatabases";
import "./Databases.css";

/** How many events the recent activity shows. */
const RECENT = 4;

/** What the Databases page's URL holds: the engine a new database is being made from. */
export interface DatabasesSearch {
  new?: string;
}

/**
 * Every database Dockwell runs or has taken on, with how each is and what it holds,
 * the way to make another, how they're backed up, and what happened to them lately
 * (Databases-List). With one server, where each runs is left out. New database
 * opens here, from an engine picked on the page or in the sheet (Catalog-Install).
 */
export function DatabasesPage({
  search = {},
  onSearch = () => {},
}: {
  search?: DatabasesSearch;
  onSearch?: (next: DatabasesSearch) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const view = useFleetView();
  const query = useDatabases();
  const strangers = useStrangers();
  const databases = query.data?.databases ?? [];
  const count = databases.length;
  const where = view.fleet
    ? plural(view.servers?.length ?? 0, { one: "# server", other: "# servers" })
    : (view.only?.name ?? "");
  const lead = query.isPending || !where ? undefined : plural(count, { one: `# on ${where}`, other: `# on ${where}` });
  const engines = useEngines();
  const firstEngine = engines.find((e) => e.id === "postgres")?.id ?? engines[0]?.id;
  const newDatabase = firstEngine ? (
    <RouterButtonLink to="/databases" search={{ new: firstEngine }} variant="primary" icon="plus">
      <Trans>New database</Trans>
    </RouterButtonLink>
  ) : null;
  return (
    <Page
      title={t`Databases`}
      lead={lead}
      phoneSub={lead}
      back="/more"
      actions={count > 0 ? newDatabase : null}
      className="databases-page"
    >
      {query.error ? (
        <Callout tone="critical" title={t`Dockwell can't read its databases`}>
          {sentence(query.error)} <Trans>Nothing changed on the servers. The page tries again by itself.</Trans>
        </Callout>
      ) : null}
      <Strangers strangers={strangers} />
      {query.isPending ? (
        <DatabasesSkeleton fleet={view.fleet} />
      ) : count === 0 && !query.error ? (
        <EmptyState icon="database" title={t`No databases yet`} action={newDatabase}>
          <Trans>PostgreSQL, MySQL, MariaDB, Redis or Valkey, from the catalog or already running on a server.</Trans>
        </EmptyState>
      ) : phone ? (
        <PhoneList databases={databases} fleet={view.fleet} />
      ) : count > 0 ? (
        <DatabasesTable databases={databases} fleet={view.fleet} />
      ) : null}
      <Engines engines={engines} />
      {search.new ? (
        <InstallSheet
          id={search.new}
          engines={{ entries: engines, onPick: (id) => onSearch({ new: id }) }}
          onClose={() => onSearch({})}
        />
      ) : null}
      {count > 0 ? (
        <div className="databases-panels">
          <BackupsPanel databases={databases} />
          <RecentActivity databases={databases} />
        </div>
      ) : null}
    </Page>
  );
}

/** The databases on a server that Dockwell didn't make, each a link to where it's taken on. */
function Strangers({ strangers }: { strangers: readonly Stranger[] }) {
  const { fleet } = useFleetView();
  if (strangers.length === 0) return null;
  const count = strangers.length;
  return (
    <Callout
      tone="info"
      title={plural(count, {
        one: "# database runs in a container Dockwell didn't make",
        other: "# databases run in containers Dockwell didn't make",
      })}
    >
      <p className="databases-strangers-line">
        <Trans>Take one on from its page: it keeps running, with no restart.</Trans>
      </p>
      <ul className="databases-strangers">
        {strangers.map((s) => (
          <li key={`${s.serverId}/${s.container.id}`}>
            <Link
              to="/servers/$serverId/containers/$name"
              params={{ serverId: s.serverId, name: s.container.name }}
              className="databases-stranger"
            >
              <code>{s.container.name}</code>
            </Link>
            <span className="databases-muted">
              <EngineOn engine={engineName(s.engine)} server={fleet ? s.serverName : ""} />
            </span>
          </li>
        ))}
      </ul>
    </Callout>
  );
}

/** An engine, and where it runs once there's a choice. */
function EngineOn({ engine, server }: { engine: string; server: string }) {
  const { t } = useLingui();
  return <>{server ? t`${engine} on ${server}` : engine}</>;
}

/** What its backups are, in a few words: how often, and how long ago the newest. */
function useBackupWords() {
  const { t } = useLingui();
  const schedule = useScheduleWords();
  const time = useTimeWords();
  const policies = usePolicies().data?.policies ?? [];
  return (db: Database, now: number): { how: string; when: string } => {
    if (!db.volume) return { how: t`No volume`, when: "" };
    if (db.volumeExternal) return { how: t`In a volume Dockwell didn't make`, when: "" };
    const policy = policiesOf(db, policies)[0];
    const how = policy?.spec ? schedule(policy.spec) : t`By hand`;
    const failed = db.latestBackup?.state === BackupState.FAILED ? db.latestBackup.startedAt : undefined;
    if (failed) {
      const ago = time.ago(timestampDate(failed), now);
      return { how, when: t`the last failed, ${ago}` };
    }
    const newest = db.newestBackup?.startedAt;
    return { how, when: newest ? time.ago(timestampDate(newest), now) : t`none yet` };
  };
}

/** The columns' heads, which the list and its skeleton share so nothing moves when it lands. */
function useColumnHeads(fleet: boolean) {
  const { t } = useLingui();
  return useMemo(() => {
    const heads: { key: string; header: string; width?: number; align?: "right" }[] = [
      { key: "name", header: t`Database` },
      { key: "status", header: t`Status`, width: 128 },
      { key: "size", header: t({ message: "Size", context: "how much data" }), width: 96, align: "right" },
      { key: "connections", header: t`Connections`, width: 176 },
      { key: "backups", header: t`Backups`, width: 200 },
    ];
    if (fleet) heads.push({ key: "server", header: t`Runs on`, width: 128 });
    heads.push({ key: "beside", header: t`Beside it`, width: 220 });
    return heads;
  }, [t, fleet]);
}

function DatabasesTable({ databases, fleet }: { databases: readonly Database[]; fleet: boolean }) {
  const { t, i18n } = useLingui();
  const navigate = useNavigate();
  const health = useHealth();
  const heads = useColumnHeads(fleet);
  const backups = useBackupWords();
  const now = useAgoNow(undefined);
  const places = usePlaces();
  const several = new Set(databases.map((d) => d.resource?.scope)).size > 1;
  const cells: Record<string, (db: Database) => ReactNode> = {
    name: (db) => (
      <div className="dw-cell">
        <span className="dw-cell-title databases-name">{db.resource?.name}</span>
        <span className="databases-sub">
          {[engineAndVersion(db), several ? places.label(db.resource?.scope ?? "") : ""].filter(Boolean).join(" · ")}
        </span>
      </div>
    ),
    status: (db) => <DatabaseHealth db={db} health={health} />,
    size: (db) => {
      const read = readOf(firstReading(db));
      return read ? formatBytes(read.sizeBytes, i18n.locale) : <Unread db={db} />;
    },
    connections: (db) => {
      const read = readOf(firstReading(db));
      return read ? (
        <Meter used={Number(read.connections)} of={Number(read.maxConnections)} locale={i18n.locale} />
      ) : (
        <Unread db={db} />
      );
    },
    backups: (db) => {
      const { how, when } = backups(db, now);
      return (
        <span>
          {how}
          {when ? <span className="databases-muted">{` · ${when}`}</span> : null}
        </span>
      );
    },
    server: (db) => (
      <code className="databases-servers">
        {db.readings.map((r, i) => (
          <span key={r.serverId}>
            {i > 0 ? ", " : null}
            <RefLink kind="server" id={r.serverId} look="plain">
              {r.serverName}
            </RefLink>
          </span>
        ))}
      </code>
    ),
    beside: (db) => <Beside names={db.beside} environment={db.resource?.scope} />,
  };
  const columns: TableColumn<Database>[] = heads.map((head) => ({
    ...head,
    ...(head.key === "size" ? { muted: false } : {}),
    render: cells[head.key] ?? (() => null),
  }));
  return (
    <Table<Database>
      className="databases-table"
      columns={columns}
      rows={databases}
      caption={t`Databases`}
      onRowClick={(db) => void navigate({ to: "/databases/$serviceId", params: { serviceId: db.id } })}
    />
  );
}

/** How a database runs, as its health checks say. */
function DatabaseHealth({ db, health }: { db: Database; health: HealthView }) {
  return <HealthBadge health={health.of({ id: db.id })} />;
}

/** A dash for a number its database didn't say, which says why when hovered. */
function Unread({ db }: { db: Database }) {
  const why = firstReading(db)?.unread ?? "";
  return (
    <span className="databases-muted" title={why}>
      –<span className="dw-sr">{why}</span>
    </span>
  );
}

/** The services beside a database, each a link to its page, as many as fit at a glance. */
function Beside({ names, environment }: { names: readonly string[]; environment: string | undefined }) {
  const { t } = useLingui();
  const named = useServicesNamed(environment);
  if (names.length === 0) return <span className="databases-muted">{t`Nothing`}</span>;
  const shown = names.slice(0, 3);
  const left = names.length - shown.length;
  return (
    <span className="databases-tags">
      {shown.map((name) => {
        const app = named.get(name);
        return app ? (
          <RefLink key={name} kind="app" id={app.id}>
            {name}
          </RefLink>
        ) : (
          <Tag key={name}>{name}</Tag>
        );
      })}
      {left > 0 ? <span className="databases-muted">{t`+${left}`}</span> : null}
    </span>
  );
}

/** The list on a phone: stacked rows of a name and a line of what it is, as M-Apps draws them. */
function PhoneList({ databases, fleet }: { databases: readonly Database[]; fleet: boolean }) {
  const { i18n } = useLingui();
  const health = useHealth();
  const words = useHealthWords();
  return (
    <Panel flush className="databases-phone">
      {databases.map((db) => {
        const runs = health.of({ id: db.id });
        const read = readOf(firstReading(db));
        const tone = `dw-tone-${STATUS[runs.status].tone}`;
        return (
          <Link key={db.id} to="/databases/$serviceId" params={{ serviceId: db.id }} className="databases-phone-row">
            <span className={cx("databases-phone-glyph", tone)}>
              <Glyph shape={STATUS[runs.status].shape} size={10} />
            </span>
            <span className="databases-phone-text">
              <span className="databases-phone-name">{db.resource?.name}</span>
              <span className="databases-phone-sub">
                <span className={cx("databases-phone-word", tone)}>{words.word(runs)}</span>
                <span aria-hidden="true">·</span>
                <span>{engineAndVersion(db)}</span>
                {read ? (
                  <>
                    <span aria-hidden="true">·</span>
                    <span>{formatBytes(read.sizeBytes, i18n.locale)}</span>
                  </>
                ) : null}
                {fleet ? (
                  <>
                    <span aria-hidden="true">·</span>
                    <code>{db.readings.map((r) => r.serverName).join(", ")}</code>
                  </>
                ) : null}
              </span>
            </span>
            <Icon name="chevron-right" size={16} className="databases-phone-chevron" />
          </Link>
        );
      })}
    </Panel>
  );
}

/** While the list is on its way: the table with its heads, so nothing moves when it lands. */
function DatabasesSkeleton({ fleet }: { fleet: boolean }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const heads = useColumnHeads(fleet);
  const columns = useMemo<TableColumn<{ id: string }>[]>(
    () =>
      heads.map((head) => ({
        ...head,
        render: () => (
          <span className="databases-skeleton-bar" style={{ width: head.key === "name" ? "44%" : "64%" }} />
        ),
      })),
    [heads],
  );
  const label = t`Loading the databases`;
  if (phone) {
    return (
      <Panel flush className="databases-phone" aria-busy="true">
        <span className="dw-sr">{label}</span>
        {["44%", "32%"].map((width) => (
          <div key={width} className="databases-skeleton-line" aria-hidden="true">
            <span className="databases-skeleton-bar" style={{ width }} />
            <span className="databases-skeleton-bar is-short" />
          </div>
        ))}
      </Panel>
    );
  }
  return (
    <div aria-busy="true">
      <span className="dw-sr">{label}</span>
      <Table
        className="databases-table databases-skeleton"
        columns={columns}
        rows={[{ id: "1" }, { id: "2" }]}
        rowClassName={() => "databases-skeleton-row"}
      />
    </div>
  );
}

/**
 * The catalog's databases, as engines a new database is made from: its database
 * entries that install one, which leaves out the tools that only manage one, such
 * as Adminer. Each entry's parts say, and the install sheet reads them too.
 */
function useEngines(): Entry[] {
  const transport = useTransport();
  const entries = (useCatalog().data?.entries ?? []).filter((e) => e.category === "databases");
  const reads = useQueries({
    queries: entries.map((entry) => ({
      ...createQueryOptions(CatalogService.method.getCatalogEntry, { id: entry.id }, { transport }),
      staleTime: 60_000,
    })),
  });
  return entries.filter((_, i) => reads[i]?.data?.parts.some((part) => part.database));
}

/** The engines, each a way to make a database. */
function Engines({ engines: entries }: { engines: readonly Entry[] }) {
  const { t } = useLingui();
  if (entries.length === 0) return null;
  return (
    <section className="databases-engines" aria-labelledby="databases-create">
      <div className="databases-section-head">
        <h2 id="databases-create" className="dw-text-heading">
          <Trans>Create a database</Trans>
        </h2>
        <span className="databases-muted">
          <Trans>From the catalog, with its password kept as a secret</Trans>
        </span>
      </div>
      <ul aria-label={t`Engines`} className="databases-engine-grid">
        {entries.map((entry) => (
          <li key={entry.id}>
            <Link
              to="/databases"
              search={{ new: entry.id }}
              className="dw-btn dw-btn-secondary dw-btn-md databases-engine"
            >
              <span className="databases-engine-plus" aria-hidden="true">
                <Icon name="plus" size={14} />
              </span>
              <span>{entry.name}</span>
              <span className="databases-engine-fill" />
              <span className="databases-engine-version">{entry.version}</span>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}

/** How the databases are backed up, with the way to take one now. */
function BackupsPanel({ databases }: { databases: readonly Database[] }) {
  const { t, i18n } = useLingui();
  const policies = usePolicies().data?.policies ?? [];
  const method = useMethodWord();
  const schedule = useScheduleWords();
  const time = useTimeWords();
  const [taking, setTaking] = useState(false);
  const withVolume = databases.filter((db) => db.volumeId !== "");
  const scheduled = withVolume.filter((db) => policiesOf(db, policies).length > 0);
  // The newest run of any of them, whole or not: one that failed since the last whole
  // one is what "last run" has to say.
  const newest = withVolume
    .flatMap((db) => [db.latestBackup, db.newestBackup])
    .filter((b): b is Backup => b !== undefined && b.state !== BackupState.RUNNING)
    .sort((a, b) => startedMs(b) - startedMs(a))[0];
  const newestFailed = newest?.state === BackupState.FAILED;
  const failedAt = newest?.startedAt ? formatDateTime(timestampDate(newest.startedAt), i18n.locale) : "";
  const failedWhy = newest?.error ?? "";
  const destinations = [
    ...new Set(
      withVolume.flatMap((db) => policiesOf(db, policies).map((p) => p.spec?.destination ?? "")).filter(Boolean),
    ),
  ];
  const volumes: VolumeChoice[] = withVolume.map((db) => ({
    id: db.volumeId,
    name: db.volume,
    environment: db.resource?.scope ?? "",
  }));
  const status =
    withVolume.length > 0 && scheduled.length === withVolume.length ? (
      <StatusBadge status="healthy" label={t`On schedule`} />
    ) : scheduled.length > 0 ? (
      <StatusBadge status="degraded" label={t`Some on schedule`} />
    ) : (
      <StatusBadge status="stopped" label={t`Only by hand`} />
    );
  const took = (b: Backup) =>
    b.startedAt && b.finishedAt ? time.span(timestampMs(b.finishedAt) - timestampMs(b.startedAt)) : "";
  return (
    <Panel title={t`Backups`} actions={status} className="databases-backups">
      <dl className="databases-facts">
        {databases.map((db) => {
          const policy = policiesOf(db, policies)[0];
          const how = !db.volume
            ? t`No volume, so its data goes with its container`
            : db.volumeExternal
              ? t`In a volume Dockwell didn't make`
              : [
                  method(db.newestBackup?.method ?? 0, db.newestBackup?.database ?? "") || engineName(db.engine),
                  policy?.spec ? schedule(policy.spec) : t`by hand`,
                ].join(", ");
          return (
            <div key={db.id} className="databases-fact">
              <dt>
                <code className="databases-fact-name">{db.resource?.name}</code>
              </dt>
              <dd>{how}</dd>
            </div>
          );
        })}
        <div className="databases-fact">
          <dt>{t`Last run`}</dt>
          <dd>
            {newest?.startedAt && newestFailed
              ? t`${failedAt}, which failed: ${failedWhy}`
              : newest?.startedAt
                ? [
                    formatDateTime(timestampDate(newest.startedAt), i18n.locale),
                    formatBytes(newest.sizeBytes, i18n.locale),
                    took(newest),
                  ]
                    .filter(Boolean)
                    .join(" · ")
                : t`None yet`}
          </dd>
        </div>
        <div className="databases-fact">
          <dt>{t`Destination`}</dt>
          <dd>{destinations.length > 0 ? destinations.join(", ") : t`Each server's own disk`}</dd>
        </div>
      </dl>
      <div className="databases-panel-actions">
        {volumes.length > 0 ? (
          <Button size="sm" icon="archive" onClick={() => setTaking(true)}>
            <Trans>Back up now</Trans>
          </Button>
        ) : null}
        <RouterButtonLink to="/backups" variant="ghost" size="sm">
          <Trans>Restore</Trans>
        </RouterButtonLink>
      </div>
      {taking ? <BackUpSheet volumes={volumes} onClose={() => setTaking(false)} /> : null}
    </Panel>
  );
}

/** When a backup started, or never. */
function startedMs(b: Backup): number {
  return b.startedAt ? timestampMs(b.startedAt) : 0;
}

/** How many databases' timelines the recent activity reads, each a request of its own. */
const RECENT_OF = 6;

/**
 * What happened to the databases lately: changes to them, and backups of their
 * volumes, as the timeline filters each by what it's about.
 */
function RecentActivity({ databases }: { databases: readonly Database[] }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const describe = useDescribe();
  const time = useTimeWords();
  const about = databases.slice(0, RECENT_OF).flatMap((db) => [db.id, db.volumeId].filter((id) => id !== ""));
  const reads = useQueries({
    queries: about.map((resourceId) => ({
      ...createQueryOptions(
        EventService.method.listEvents,
        { kinds: [EventKind.CHANGE, EventKind.BACKUP], resourceId, pageSize: RECENT },
        { transport },
      ),
      refetchInterval: 30_000,
    })),
  });
  const seen = new Set<string>();
  const list = reads
    .flatMap((read) => read.data?.events ?? [])
    .filter((event) => {
      if (seen.has(event.id)) return false;
      seen.add(event.id);
      return true;
    })
    .sort((a, b) => happenedAt(b).getTime() - happenedAt(a).getTime())
    .slice(0, RECENT);
  const pending = reads.some((read) => read.isPending);
  const newest = list[0];
  const now = useAgoNow(newest ? happenedAt(newest) : undefined);
  const items: TimelineEvent[] = list.map((event) => {
    const when = happenedAt(event);
    const d = describe(event, new Map());
    return {
      id: event.id,
      kind: event.kind === EventKind.BACKUP ? "backup" : "config",
      icon: d.icon,
      ...(d.tone === "critical"
        ? { tone: "critical" as const }
        : event.kind === EventKind.BACKUP
          ? { tone: "healthy" as const }
          : {}),
      title: d.title,
      meta: event.backup ? backupMeta(event, i18n.locale) : "",
      time: time.ago(when, now),
      datetime: when.toISOString(),
    };
  });
  return (
    <Panel
      title={t`Recent activity`}
      actions={
        <Link to="/activity" className="databases-panel-link">
          <Trans>Activity</Trans>
        </Link>
      }
    >
      {items.length > 0 ? (
        <Timeline events={items} />
      ) : (
        <p className="databases-muted databases-quiet">
          {pending ? <Trans>Reading what happened</Trans> : <Trans>Nothing has happened to them lately</Trans>}
        </p>
      )}
    </Panel>
  );
}

/** What a backup's event says beside its title: how large, and where. */
function backupMeta(event: Event, locale: string): string {
  const b = event.backup;
  if (!b) return "";
  return [b.sizeBytes > 0n ? formatBytes(b.sizeBytes, locale) : "", b.serverName].filter(Boolean).join(" · ");
}
