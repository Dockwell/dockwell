// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import {
  type Backup,
  BackupService,
  BackupState,
  ChangeService,
  type Database,
  Engine,
  type Event,
  EventKind,
  EventService,
  type ServiceInfo,
  StepUpAction,
  TerminalService,
  VariableService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  Skeleton,
  SkeletonRows,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  Tag,
  Timeline,
  type TimelineEvent,
  Toast,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueries } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { type CSSProperties, type ReactNode, useEffect, useLayoutEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { happenedAt, useDescribe } from "../activity/describe";
import { HealthBadge, useHealthWords } from "../apps/Health";
import { type HealthView, useHealth } from "../apps/health";
import { MoveSheet } from "../apps/MoveSheet";
import { useShownName } from "../apps/names";
import { PrepareServiceDelete, PreparingServiceAction, useServiceRowActions } from "../apps/ServiceRowActions";
import { usePlaces, useServices, useServicesNamed } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { BackUpSheet } from "../backups/BackUpSheet";
import { useDestinations } from "../backups/DestinationsPanel";
import { usePolicies } from "../backups/PoliciesPanel";
import { useBackUpDaily } from "../backups/PolicySheet";
import { useStorage } from "../backups/storage";
import type { VolumeChoice } from "../backups/useBackups";
import { useMethodWord, useScheduleWords } from "../backups/words";
import { type EngineGroup, useEngineGroups } from "../catalog/engines";
import { InstallSheet } from "../catalog/InstallSheet";
import { AppIcon } from "../catalog/media";
import { ConnectionSheet } from "../connections/ConnectionSheet";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  SortHeader,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { REVEALED_FOR } from "../variables/useReveal";
import { backsUp } from "./DatabaseOverview";
import { Meter } from "./Meter";
import {
  connectionURL,
  consoleTarget,
  engineAndVersion,
  engineName,
  externalConnectionURL,
  firstReading,
  makesUsers,
  policiesOf,
  readOf,
  type Stranger,
  useDatabaseDeleteUsers,
  useDatabases,
  useStrangers,
  useUnreadWord,
} from "./useDatabases";
import "../catalog/Catalog.css";
import "./Databases.css";

/** How many events the recent activity shows. */
const RECENT = 4;

/** What the Databases page's URL holds: where its list is, and the engine a new database is being made from. */
export interface DatabasesSearch extends ListSearch {
  new?: string;
  engine?: string;
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["name", "size"] as const;

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
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const manage = useServiceRowActions((db: Database) => db.resource?.scope ?? "");
  const rowTools = useDatabaseRowTools(manage.actionsFor);
  const actionsFor = rowTools.actionsFor;
  const shownName = useShownName();
  const engineKinds = useMemo(
    () => [...new Map(databases.map((db) => [engineName(db.engine), db.engine])).entries()],
    [databases],
  );
  const shown = useMemo(() => {
    const q = place.query;
    const filtered = databases.filter(
      (db) =>
        (!search.engine || engineName(db.engine) === search.engine) &&
        (q === "" ||
          shownName(db, "database").title.toLowerCase().includes(q) ||
          engineAndVersion(db).toLowerCase().includes(q)),
    );
    return sortedRows(filtered, place.sort, (db, key) =>
      key === "size"
        ? readOf(firstReading(db))
          ? Number(readOf(firstReading(db))?.sizeBytes ?? 0n)
          : null
        : shownName(db, "database").title,
    );
  }, [databases, search.engine, place.query, place.sort, shownName]);
  const page = pagedRows(shown, place.page);
  const where = view.fleet
    ? plural(view.servers?.length ?? 0, { one: "# server", other: "# servers" })
    : (view.only?.name ?? "");
  const lead = query.isPending || !where ? undefined : plural(count, { one: `# on ${where}`, other: `# on ${where}` });
  const engines = useEngineGroups();
  // PostgreSQL first, where the catalog makes it, as the database most apps take.
  const firstEngine = (engines.groups.find((g) => g.engine === Engine.POSTGRESQL) ?? engines.groups[0])?.entries[0]?.id;
  const newDatabase = firstEngine ? (
    <RouterButtonLink to="/databases" search={{ ...search, new: firstEngine }} variant="primary" icon="plus">
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
      {count > 0 ? (
        <div className="list-bar">
          <ListSearchField
            label={t`Filter databases`}
            placeholder={t`Filter by name or engine`}
            size={phone ? "lg" : "sm"}
            value={place.typed}
            onChange={place.setTyped}
          />
          {phone ? <ListSortField place={place} columns={{ name: t`Database`, size: t`Size` }} /> : null}
          {engineKinds.length > 1 ? (
            <SelectField
              aria-label={t`Engine`}
              size="sm"
              value={search.engine ?? ""}
              options={[
                { value: "", label: t`Every engine` },
                ...engineKinds.map(([name]) => ({ value: name, label: name })),
              ]}
              onChange={(event) => onSearch({ ...search, page: undefined, engine: event.target.value || undefined })}
            />
          ) : null}
        </div>
      ) : null}
      {query.isPending ? (
        <DatabasesSkeleton fleet={view.fleet} />
      ) : count === 0 && !query.error ? (
        <EmptyState
          icon="database"
          title={t`No databases yet`}
          className="databases-empty"
          action={engines.groups.length === 0 && !engines.pending ? <MoreInCatalog /> : undefined}
          choices={
            engines.groups.length > 0 || engines.pending ? (
              <>
                <EngineTiles groups={engines.groups} pending={engines.pending} />
                <p className="databases-more">
                  <MoreInCatalog />
                </p>
              </>
            ) : undefined
          }
        >
          <Trans>From the catalog, with its password kept as a secret</Trans>
        </EmptyState>
      ) : shown.length === 0 && !query.error ? (
        <EmptyState
          icon="search"
          title={t`Nothing matches`}
          action={
            <Button
              size="lg"
              onClick={() => {
                place.clearQuery();
                onSearch({ ...search, q: undefined, page: undefined, engine: undefined });
              }}
            >
              <Trans>Clear the filter</Trans>
            </Button>
          }
        >
          <p>
            {place.query ? (
              <Trans>
                No database here has <code>{place.typed.trim()}</code> in its name or engine.
              </Trans>
            ) : (
              <Trans>No database of this engine here.</Trans>
            )}
          </p>
        </EmptyState>
      ) : phone ? (
        <>
          <PhoneList databases={page.rows} fleet={view.fleet} actionsFor={actionsFor} />
          <ListPager page={page} onPage={place.setPage} />
        </>
      ) : count > 0 ? (
        <>
          <DatabasesTable databases={page.rows} fleet={view.fleet} place={place} actionsFor={actionsFor} />
          <ListPager page={page} onPage={place.setPage} />
        </>
      ) : null}
      {count > 0 ? <Engines groups={engines.groups} pending={engines.pending} /> : null}
      {search.new ? (
        <InstallSheet
          id={search.new}
          engines={{ groups: engines.groups, onPick: (id) => onSearch({ ...search, new: id }) }}
          onClose={() => onSearch({ ...search, new: undefined })}
        />
      ) : null}
      {count > 0 ? (
        <div className="databases-panels">
          <BackupsPanel databases={databases} />
          <RecentActivity databases={databases} />
        </div>
      ) : null}
      {manage.selected ? (
        <DatabaseRowPlan db={manage.selected.service} action={manage.selected.action} onClose={manage.close} />
      ) : null}
      {rowTools.overlays}
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
  return (db: Database, now: number): { how: string; when: string; unscheduled: boolean } => {
    if (!db.volume) return { how: t`No volume`, when: "", unscheduled: false };
    if (db.volumeExternal) return { how: t`In a volume Dockwell didn't make`, when: "", unscheduled: false };
    const policy = policiesOf(db, policies)[0];
    const how = policy?.spec ? schedule(policy.spec) : t`Not scheduled`;
    const unscheduled = !policy;
    const failed = db.latestBackup?.state === BackupState.FAILED ? db.latestBackup.startedAt : undefined;
    if (failed) {
      const ago = time.ago(timestampDate(failed), now);
      return { how, when: t`the last failed, ${ago}`, unscheduled };
    }
    const newest = db.newestBackup?.startedAt;
    // An unscheduled database with no backups yet pairs directly with "Back up daily" on one line.
    return {
      how,
      when: newest ? time.ago(timestampDate(newest), now) : unscheduled ? "" : t`none yet`,
      unscheduled,
    };
  };
}

/** The columns' heads, which the list and its skeleton share so nothing moves when it lands. */
function useColumnHeads(fleet: boolean) {
  const { t } = useLingui();
  return useMemo(() => {
    const heads: { key: string; header: string; width?: number; align?: "right" }[] = [
      { key: "name", header: t`Database` },
      { key: "status", header: t`Status`, width: 120 },
      { key: "size", header: t({ message: "Size", context: "how much data" }), width: 96, align: "right" },
      { key: "connections", header: t`Connections`, width: 176 },
      { key: "backups", header: t`Backups`, width: 268 },
    ];
    if (fleet) heads.push({ key: "server", header: t`Runs on`, width: 128 });
    heads.push({ key: "beside", header: t`Beside it`, width: 168 });
    return heads;
  }, [t, fleet]);
}

interface CopyFeedback {
  tone: "healthy" | "info" | "critical";
  title: string;
  description?: string;
  /** Held only when the browser blocked clipboard write after an async passkey prompt; cleared after REVEALED_FOR. */
  pendingText?: string;
}

/**
 * A database's row menu: its page, console, back up now, connection string copy
 * (internal, and external only when external access is on; passwords copied only
 * after a step-up, as Reveal does), users and backups.
 */
function useDatabaseRowTools(manage: (db: Database) => MenuAction[]) {
  const { t } = useLingui();
  const navigate = useNavigate();
  const transport = useTransport();
  const explain = useAuthError();
  const health = useHealth();
  const { servers } = useFleetView();
  const may = useMay();
  const mayConsole = may(TerminalService.method.openTerminal);
  const [backingUp, setBackingUp] = useState<Database>();
  const [feedback, setFeedback] = useState<CopyFeedback>();

  // Any revealed value held for a manual clipboard fallback expires with the same 60s window as Reveal.
  useEffect(() => {
    if (!feedback?.pendingText) return;
    const timer = setTimeout(() => setFeedback(undefined), REVEALED_FOR);
    return () => clearTimeout(timer);
  }, [feedback?.pendingText]);

  const externalAddressOf = (db: Database): string => {
    const reading = firstReading(db);
    const server = servers?.find((s) => s.id === reading?.serverId);
    return server?.address || reading?.serverName || "";
  };

  const copyConnection = async (db: Database, mode: "internal" | "external") => {
    const read = readOf(firstReading(db));
    const environment = db.resource?.scope ?? "";
    const externalHost = mode === "external" ? externalAddressOf(db) : "";
    const build = (password: string | undefined) =>
      mode === "external"
        ? externalConnectionURL(db, read, externalHost, password)
        : connectionURL(db, read, password);
    const hasSecret = db.passwordSecret !== "" && !read?.checksNoOne;
    const mayReveal = may(VariableService.method.revealSecret, { environment });

    let password: string | undefined;
    let omittedPassword = false;
    if (hasSecret) {
      if (!mayReveal.allowed) {
        omittedPassword = true;
      } else {
        try {
          const client = createClient(VariableService, transport);
          const listed = await client.listVariables({ environment });
          const secret = listed.variables.find((v) => v.secret && v.name === db.passwordSecret);
          if (!secret) {
            omittedPassword = true;
          } else {
            const revealed = await withStepUpIfAsked(transport, StepUpAction.REVEAL_SECRET, secret.id, (proof) =>
              client.revealSecret({ id: secret.id, proof }),
            );
            password = revealed.value;
          }
        } catch (err) {
          setFeedback({
            tone: "critical",
            title: t`Connection string wasn't copied`,
            description: explain(err),
          });
          return;
        }
      }
    }

    const url = build(password);
    if (!url) return;
    const copiedTitle =
      mode === "external"
        ? omittedPassword
          ? t`External connection string copied without the password`
          : t`External connection string copied`
        : omittedPassword
          ? t`Connection string copied without the password`
          : t`Connection string copied`;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("no clipboard");
      await navigator.clipboard.writeText(url);
      setFeedback({ tone: omittedPassword ? "info" : "healthy", title: copiedTitle });
    } catch {
      setFeedback({
        tone: "info",
        title: t`Ready to copy`,
        description: t`Your browser blocked copying after confirmation. Click Copy to put it on your clipboard.`,
        pendingText: url,
      });
    }
  };

  const actionsFor = (db: Database): MenuAction[] => {
    const environment = db.resource?.scope ?? "";
    const target = consoleTarget(db, health.of({ id: db.id }));
    const mayBackUp = may(BackupService.method.createBackup, { environment });
    const canBackUp = backsUp(db);
    const externalHost = db.externalAccess ? externalAddressOf(db) : "";
    return [
      {
        id: "open",
        icon: "database",
        label: t`Open`,
        onSelect: () => void navigate({ to: "/databases/$serviceId", params: { serviceId: db.id } }),
      },
      ...(mayConsole.allowed
        ? [
            {
              id: "console",
              icon: "terminal" as const,
              label: t`Open console`,
              off: target ? undefined : t`The database container isn't running on a server yet`,
              onSelect: () => {
                if (!target) return;
                void navigate({
                  to: "/servers/$serverId/terminal",
                  params: { serverId: target.serverId },
                  search: { container: target.container, console: true },
                });
              },
            },
          ]
        : []),
      {
        id: "backup",
        icon: "archive",
        label: t`Back up now`,
        off: !canBackUp
          ? db.volumeExternal
            ? t`Its volume isn't one Dockwell backs up`
            : t`It has no volume to back up`
          : mayBackUp.allowed
            ? undefined
            : mayBackUp.reason,
        onSelect: () => setBackingUp(db),
      },
      {
        id: "copy-connection",
        icon: "copy",
        label: t`Copy connection string`,
        onSelect: () => void copyConnection(db, "internal"),
      },
      ...(db.externalAccess && externalHost
        ? [
            {
              id: "copy-external-connection",
              icon: "copy" as const,
              label: t`Copy external connection string`,
              onSelect: () => void copyConnection(db, "external"),
            },
          ]
        : []),
      ...(makesUsers(db.engine)
        ? [
            {
              id: "databases",
              icon: "database" as const,
              label: t`Databases`,
              onSelect: () => void navigate({ to: "/databases/$serviceId/databases", params: { serviceId: db.id } }),
            },
          ]
        : []),
      {
        id: "users",
        icon: "user",
        label: t`Users`,
        onSelect: () => void navigate({ to: "/databases/$serviceId/users", params: { serviceId: db.id } }),
      },
      {
        id: "backups",
        icon: "archive",
        label: t`Backups`,
        onSelect: () => void navigate({ to: "/databases/$serviceId/backups", params: { serviceId: db.id } }),
      },
      ...manage(db),
    ];
  };

  const overlays = (
    <>
      {backingUp ? (
        <BackUpSheet
          volumes={[{ id: backingUp.volumeId, name: backingUp.volume, environment: backingUp.resource?.scope ?? "" }]}
          chosen={backingUp.volumeId}
          onClose={() => setBackingUp(undefined)}
        />
      ) : null}
      {feedback ? (
        <div className="dw-toaster">
          <Toast
            tone={feedback.tone}
            title={feedback.title}
            description={feedback.description}
            duration={feedback.pendingText ? undefined : 6000}
            action={
              feedback.pendingText
                ? {
                    label: t`Copy`,
                    onClick: () => {
                      const text = feedback.pendingText ?? "";
                      void navigator.clipboard?.writeText(text).then(() => {
                        setFeedback({ tone: "healthy", title: t`Connection string copied` });
                      });
                    },
                  }
                : undefined
            }
            onClose={() => setFeedback(undefined)}
          />
        </div>
      ) : null}
    </>
  );

  return { actionsFor, overlays };
}

function DatabaseRowPlan({ db, action, onClose }: { db: Database; action: "move" | "delete"; onClose: () => void }) {
  const { t } = useLingui();
  const services = useServices();
  const service = services.data?.find((s) => s.id === db.id);
  if (!service) {
    const error = services.error ?? (services.data ? new Error(t`This database has no service to manage`) : undefined);
    return (
      <PreparingServiceAction
        title={t`Read the database service`}
        error={error}
        retry={() => void services.refetch()}
        onClose={onClose}
      />
    );
  }
  return action === "move" ? (
    <MoveSheet service={service} onClose={onClose} />
  ) : (
    <PrepareDatabaseDelete db={db} service={service} onClose={onClose} />
  );
}

function PrepareDatabaseDelete({ db, service, onClose }: { db: Database; service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const users = useDatabaseDeleteUsers(db);
  if (users.pending || users.error) {
    return (
      <PreparingServiceAction
        title={t`Read the database users`}
        error={users.error}
        retry={users.retry}
        onClose={onClose}
      />
    );
  }
  return <PrepareServiceDelete service={service} alongside={users.alongside} after="/databases" onClose={onClose} />;
}

function DatabasesTable({
  databases,
  fleet,
  place,
  actionsFor,
}: {
  databases: readonly Database[];
  fleet: boolean;
  place: ListPlace;
  actionsFor: (db: Database) => MenuAction[];
}) {
  const { t, i18n } = useLingui();
  const navigate = useNavigate();
  const health = useHealth();
  const heads = useColumnHeads(fleet);
  const backups = useBackupWords();
  const backUpDaily = useBackUpDaily();
  const destinations = useDestinations().data?.destinations ?? [];
  const mayPlan = useMay();
  const now = useAgoNow(undefined);
  const places = usePlaces();
  // A catalog install's database goes by the name the person gave the install (#796).
  const shownName = useShownName();
  const several = new Set(databases.map((d) => d.resource?.scope)).size > 1;
  const cells: Record<string, (db: Database) => ReactNode> = {
    name: (db) => (
      <div className="dw-cell">
        <span className="dw-cell-title databases-name">{shownName(db, "database").title}</span>
        <span className="databases-sub">
          {[
            engineAndVersion(db),
            shownName(db, "database").app || (several ? places.label(db.resource?.scope ?? "") : ""),
          ]
            .filter(Boolean)
            .join(" · ")}
        </span>
      </div>
    ),
    status: (db) => <DatabaseHealth db={db} health={health} />,
    size: (db) => {
      const read = readOf(firstReading(db));
      const why = firstReading(db)?.unread ?? "";
      return read ? (
        formatBytes(read.sizeBytes, i18n.locale)
      ) : (
        <span className="databases-muted" title={why || undefined} aria-hidden="true">
          —
        </span>
      );
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
      const { how, when, unscheduled } = backups(db, now);
      const may = mayPlan(ChangeService.method.proposeChange, { environment: db.resource?.scope ?? "" });
      return unscheduled ? (
        // A database with no schedule says so, with the one step that gives it one, on one even line (#793).
        <span className="databases-backup-off">
          <span>
            {how}
            {when ? <span className="databases-muted">{` · ${when}`}</span> : null}
          </span>
          <span className="databases-muted" aria-hidden="true">
            ·
          </span>
          <button
            type="button"
            className="databases-inline-act"
            disabled={!may.allowed}
            title={may.reason || undefined}
            onClick={(event) => {
              event.stopPropagation();
              backUpDaily([{ id: db.volumeId, name: db.volume, environment: db.resource?.scope ?? "" }], destinations);
            }}
          >
            <Trans>Back up daily</Trans>
          </button>
        </span>
      ) : (
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
  const menu = useRowMenu<Database>();
  const columns: TableColumn<Database>[] = [
    ...heads.map((head) => ({
      ...head,
      ...(head.key === "name" || head.key === "size"
        ? { header: <SortHeader label={head.header} sortKey={head.key} place={place} />, sort: place.sortOf(head.key) }
        : {}),
      ...(head.key === "size" ? { muted: false } : {}),
      render: cells[head.key] ?? (() => null),
    })),
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 48,
      align: "right",
      render: (db: Database) => (
        <ActionMenu label={t`Actions for ${shownName(db, "database").title}`} actions={actionsFor(db)} />
      ),
    },
  ];
  return (
    <>
      <Table<Database>
        className="databases-table"
        columns={columns}
        rows={databases}
        caption={t`Databases`}
        onRowClick={(db) => void navigate({ to: "/databases/$serviceId", params: { serviceId: db.id } })}
        onRowContextMenu={(db, at, el) =>
          menu.open(db, at, el, { label: t`Actions for ${shownName(db, "database").title}`, actions: actionsFor(db) })
        }
      />
      {menu.element}
    </>
  );
}

/** How a database runs, as its health checks say. */
function DatabaseHealth({ db, health }: { db: Database; health: HealthView }) {
  return <HealthBadge health={health.of({ id: db.id })} />;
}

/** A short muted state for a database the agent couldn't read, with the full reason on hover and for screen readers. */
function Unread({ db }: { db: Database }) {
  const unreadWord = useUnreadWord();
  const why = firstReading(db)?.unread ?? "";
  const short = unreadWord(why);
  return (
    <span className="databases-muted" title={why || undefined}>
      {short}
      {why ? <span className="dw-sr">{why}</span> : null}
    </span>
  );
}

/** The services beside a database, each a link to its page, as many as fit at a glance. */
function Beside({ names, environment }: { names: readonly string[]; environment: string | undefined }) {
  const { t } = useLingui();
  const named = useServicesNamed(environment);
  if (names.length === 0)
    return (
      <span className="databases-muted" aria-hidden="true">
        —
      </span>
    );
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
function PhoneList({
  databases,
  fleet,
  actionsFor,
}: {
  databases: readonly Database[];
  fleet: boolean;
  actionsFor: (db: Database) => MenuAction[];
}) {
  const { t, i18n } = useLingui();
  const health = useHealth();
  const words = useHealthWords();
  // Where each is, as the table says it, where they're in more than one place.
  const places = usePlaces();
  // A catalog install's database goes by the name the person gave the install (#796).
  const shownName = useShownName();
  const several = new Set(databases.map((d) => d.resource?.scope)).size > 1;
  return (
    <Panel flush className="databases-phone">
      {databases.map((db) => {
        const runs = health.of({ id: db.id });
        const read = readOf(firstReading(db));
        const tone = `dw-tone-${STATUS[runs.status].tone}`;
        return (
          <div key={db.id} className="databases-phone-item">
            <Link to="/databases/$serviceId" params={{ serviceId: db.id }} className="databases-phone-row">
              <span className={cx("databases-phone-glyph", tone)}>
                <Glyph shape={STATUS[runs.status].shape} size={10} />
              </span>
              <span className="databases-phone-text">
                <span className="databases-phone-name">{shownName(db, "database").title}</span>
                <span className="databases-phone-sub">
                  <span className={cx("databases-phone-word", tone)}>{words.word(runs)}</span>
                  <span aria-hidden="true">·</span>
                  <span>{engineAndVersion(db)}</span>
                  {shownName(db, "database").app || several ? (
                    <>
                      <span aria-hidden="true">·</span>
                      <span>{shownName(db, "database").app || places.label(db.resource?.scope ?? "")}</span>
                    </>
                  ) : null}
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
            <ActionMenu label={t`Actions for ${shownName(db, "database").title}`} actions={actionsFor(db)} />
          </div>
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
    () => heads.map((head) => ({ ...head, render: () => null, skeleton: head.key === "name" ? "titled" : undefined })),
    [heads],
  );
  const label = t`Loading the databases`;
  if (phone) {
    return (
      <Loading label={label}>
        <Panel flush className="databases-phone">
          <SkeletonRows rows={2} lead="mark" />
        </Panel>
      </Loading>
    );
  }
  return <Table className="databases-table" columns={columns} rows={[]} loading loadingRows={2} loadingLabel={label} />;
}

/**
 * How many tiles stand in for the engines while the catalog is read: as many as
 * there were last time, kept in this browser, so the grid keeps its size as they
 * arrive; Dockwell's own five before that.
 */
const PLACEHOLDERS = 5;
const SEEN_ENGINES = "dockwell.databases.engines";

function enginesSeen(): number {
  try {
    const n = Number(localStorage.getItem(SEEN_ENGINES));
    return Number.isInteger(n) && n > 0 && n <= 12 ? n : PLACEHOLDERS;
  } catch {
    return PLACEHOLDERS;
  }
}

function rememberEngines(n: number) {
  try {
    localStorage.setItem(SEEN_ENGINES, String(n));
  } catch {
    // A browser that keeps nothing shows Dockwell's own count next time.
  }
}

/** The narrowest an engine's tile reads well at, its logo, name and version on a line. */
const TILE_MIN = 160;

/**
 * How many columns count tiles take in a row width wide, with gap between them: as
 * many as fit, at least TILE_MIN each, and then as few as keep the rows even, so six
 * tiles four to a row are two rows of three, not four and two.
 */
export function balancedColumns(count: number, width: number, gap: number): number {
  if (count <= 0 || width <= 0) return 1;
  const fit = Math.max(1, Math.floor((width + gap) / (TILE_MIN + gap)));
  const rows = Math.ceil(count / Math.min(count, fit));
  return Math.ceil(count / rows);
}

/** The columns a grid of count tiles takes at its width, as it changes, and the ref that measures it. */
function useBalancedColumns(count: number): [(el: HTMLUListElement | null) => void, number | undefined] {
  const [el, setEl] = useState<HTMLUListElement | null>(null);
  const [columns, setColumns] = useState<number>();
  useLayoutEffect(() => {
    if (!el) return;
    const measure = () => {
      const gap = Number.parseFloat(getComputedStyle(el).columnGap) || 0;
      setColumns(balancedColumns(count, el.clientWidth, gap));
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const seen = new ResizeObserver(measure);
    seen.observe(el);
    return () => seen.disconnect();
  }, [el, count]);
  return [setEl, columns];
}

/** The catalog, narrowed to its databases, for an engine or a tool New database doesn't show. */
function MoreInCatalog() {
  return (
    <Link to="/catalog" search={{ category: "databases" }} className="databases-panel-link">
      <Trans>More in the catalog</Trans>
    </Link>
  );
}

/** The engines, each a way to make a database, with the catalog's other databases one step away. */
function Engines({ groups, pending }: { groups: readonly EngineGroup[]; pending: boolean }) {
  if (groups.length === 0 && !pending) return null;
  return (
    <section className="databases-engines" aria-labelledby="databases-create">
      <div className="databases-section-head">
        <h2 id="databases-create" className="dw-text-heading">
          <Trans>Create a database</Trans>
        </h2>
        <span className="databases-muted">
          <Trans>From the catalog, with its password kept as a secret</Trans>
        </span>
        <span className="database-section-action">
          <MoreInCatalog />
        </span>
      </div>
      <EngineTiles groups={groups} pending={pending} />
    </section>
  );
}

/**
 * One tile for each engine, with its logo and the version a new one takes unless
 * the sheet says otherwise, in a grid whose columns share the row evenly. The
 * catalog an engine comes from, and any other version, are picked in the sheet, so
 * the grid keeps its size however many catalogs Dockwell reads (#599).
 */
function EngineTiles({ groups, pending }: { groups: readonly EngineGroup[]; pending: boolean }) {
  const { t } = useLingui();
  const [placeholders] = useState(enginesSeen);
  const count = groups.length || placeholders;
  const [grid, columns] = useBalancedColumns(count);
  useEffect(() => {
    if (groups.length > 0) rememberEngines(groups.length);
  }, [groups.length]);
  const style = columns ? ({ "--databases-engine-columns": columns } as CSSProperties) : undefined;
  if (groups.length === 0) {
    return (
      <ul ref={grid} style={style} aria-label={t`Engines`} aria-busy="true" className="databases-engine-grid">
        {Array.from({ length: placeholders }, (_, k) => `placeholder-${k}`).map((k, index) => (
          <li key={k} className="databases-engine is-placeholder" aria-hidden="true">
            <Skeleton shape="block" width={32} height={32} />
            <span className="databases-engine-text">
              <Skeleton text="label" width={[72, 88, 64, 80, 96][index % 5]} />
              <Skeleton text="caption" width={40} />
            </span>
          </li>
        ))}
      </ul>
    );
  }
  return (
    <ul
      ref={grid}
      style={style}
      aria-label={t`Engines`}
      aria-busy={pending || undefined}
      className={cx("databases-engine-grid", count % (columns ?? count) !== 0 && "is-uneven")}
    >
      {groups.map((group) => {
        const first = group.entries[0];
        if (!first) return null;
        return (
          <li key={group.engine}>
            <Link to="/databases" search={{ new: first.id }} className="databases-engine">
              <AppIcon name={group.name} path={first.icon} />
              <span className="databases-engine-text">
                <span className="databases-engine-name">{group.name}</span>
                <span className="databases-engine-version">{first.version}</span>
              </span>
              <span className="databases-engine-plus" aria-hidden="true">
                <Icon name="plus" size={16} />
              </span>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** How the databases are backed up, with the way to take one now. */
function BackupsPanel({ databases }: { databases: readonly Database[] }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const policies = usePolicies().data?.policies ?? [];
  const method = useMethodWord();
  const schedule = useScheduleWords();
  const time = useTimeWords();
  const [taking, setTaking] = useState(false);
  const [addingStorage, setAddingStorage] = useState(false);
  const backUpDaily = useBackUpDaily();
  const may = useMay();
  const mayAddStorage = may(ChangeService.method.proposeChange, { install: true });
  const storage = useStorage();
  const allDestinations = storage.destinations;
  // Where each is, as its row says it, where they're in more than one place, as several can share a name such as db.
  const places = usePlaces();
  // A catalog install's database goes by the name the person gave the install (#796).
  const shownName = useShownName();
  const several = new Set(databases.map((d) => d.resource?.scope)).size > 1;
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
  const needsStorage = scheduled.length > 0 && destinations.length === 0 && storage.items.length === 0;
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
        <div className="databases-fact">
          <dt>{databases.length === 1 ? t`Database` : t`Databases`}</dt>
          <dd className="databases-fact-list">
            {databases.map((db) => {
              const policy = policiesOf(db, policies)[0];
              const isUnscheduled = db.volumeId !== "" && !db.volumeExternal && !policy;
              const mayPlan = may(ChangeService.method.proposeChange, { environment: db.resource?.scope ?? "" });
              const how = !db.volume
                ? t`No volume, so its data goes with its container`
                : db.volumeExternal
                  ? t`In a volume Dockwell didn't make`
                  : [
                      method(db.newestBackup?.method ?? 0, db.newestBackup?.database ?? "") || engineName(db.engine),
                      policy?.spec ? schedule(policy.spec) : t`not scheduled`,
                    ].join(", ");
              const place = shownName(db, "database").app || (several ? places.label(db.resource?.scope ?? "") : "");
              return (
                <div key={db.id} className="databases-fact-item">
                  <code className="databases-fact-name">{shownName(db, "database").title}</code>
                  {place ? <span className="databases-fact-place databases-muted">{place}</span> : null}
                  <span className="databases-muted" aria-hidden="true">
                    ·
                  </span>
                  <span>{how}</span>
                  {/* On desktop the table row holds "Back up daily"; on a phone there is no table, so offer it here. */}
                  {phone && isUnscheduled ? (
                    <>
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
                            allDestinations,
                          )
                        }
                      >
                        <Trans>Back up daily</Trans>
                      </button>
                    </>
                  ) : null}
                </div>
              );
            })}
          </dd>
        </div>
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
          <dd className="databases-fact-inline">
            <span>{destinations.length > 0 ? destinations.join(", ") : t`Each server's disk alone, unencrypted`}</span>
            {needsStorage ? (
              <>
                <span className="databases-muted" aria-hidden="true">
                  ·
                </span>
                <button
                  type="button"
                  className="databases-inline-act"
                  onClick={() => setAddingStorage(true)}
                  disabled={!mayAddStorage.allowed}
                  title={mayAddStorage.reason || undefined}
                >
                  <Trans>Add storage</Trans>
                </button>
              </>
            ) : null}
          </dd>
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
      {addingStorage ? (
        <ConnectionSheet kind="storage" existing={storage.allConnections} onClose={() => setAddingStorage(false)} />
      ) : null}
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
      className="databases-activity"
      actions={
        <Link to="/activity" className="databases-panel-link">
          <Trans>Activity</Trans>
        </Link>
      }
    >
      {items.length > 0 ? (
        <Timeline events={items} />
      ) : pending ? (
        <Loading label={t`Reading what happened`}>
          <SkeletonRows rows={2} lead="icon" dense className="databases-quiet-rows" />
        </Loading>
      ) : (
        <p className="databases-muted databases-quiet">
          <Trans>Nothing has happened to them lately</Trans>
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
