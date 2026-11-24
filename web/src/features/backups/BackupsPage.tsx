// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create } from "@bufbuild/protobuf";
import { timestampDate, timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import {
  type Backup,
  BackupCopy,
  BackupMethod,
  BackupPolicy_Drill,
  type BackupPolicyInfo,
  BackupPolicySchema,
  BackupService,
  BackupState,
  ChangeService,
  DrillResult,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Loading,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  SkeletonRows,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { BackUpSheet } from "./BackUpSheet";
import { DestinationsPanel, useDestinations } from "./DestinationsPanel";
import { PoliciesPanel, usePolicies } from "./PoliciesPanel";
import { PolicySheet } from "./PolicySheet";
import { useRestoreWhole } from "./RestoreWhole";
import { useBackupPages, useRefreshOlderBackup, useVolumes } from "./useBackups";
import { backupStatus, useMethodWord, useStateWord, useWhen } from "./words";
import "./Backups.css";
import { useMay } from "../auth/may";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import {
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  SortHeader,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { LIST_PAGE_SIZE, type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { RefLink } from "../shell/RefLink";

/** The list's place, in its URL: its filter, its sort, its page, and the state it shows. */
export interface BackupsSearch extends ListSearch {
  state?: string;
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["started", "volume", "size"] as const;

/** A backup's state as the address holds it. */
function stateKey(state: BackupState): string {
  return state === BackupState.DONE ? "done" : state === BackupState.FAILED ? "failed" : "running";
}

/**
 * Every backup, newest first: back one up, restore one through a plan, or delete
 * one. A server is a field here, as it is everywhere: with one, it's left out.
 */
export function BackupsPage({
  search = {},
  onSearch,
}: {
  search?: BackupsSearch;
  onSearch?: (next: BackupsSearch) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const volumes = useVolumes();
  const query = useBackupPages();
  const [taking, setTaking] = useState(false);
  const [scheduling, setScheduling] = useState<{ editing?: BackupPolicyInfo }>();
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const { backups, fetchNextPage, hasNextPage, isFetching, isFetchNextPageError } = query;
  const [searchingAll, setSearchingAll] = useState(false);
  const [nextPage, setNextPage] = useState<number>();
  const filtered = !!(place.query || place.sort || search.state);
  const states = hasNextPage ? ["done", "failed", "running"] : [...new Set(backups.map((b) => stateKey(b.state)))];
  const listed = useMemo(() => {
    const q = place.query;
    const filtered = backups.filter(
      (b) =>
        (!search.state || stateKey(b.state) === search.state) &&
        (q === "" ||
          b.volume.toLowerCase().includes(q) ||
          b.destination.toLowerCase().includes(q) ||
          b.serverName.toLowerCase().includes(q)),
    );
    return sortedRows(filtered, place.sort, (b, key) =>
      key === "started"
        ? b.startedAt
          ? timestampMs(b.startedAt)
          : null
        : key === "size"
          ? b.sizeBytes > 0n
            ? Number(b.sizeBytes)
            : null
          : b.volume,
    );
  }, [backups, search.state, place.query, place.sort]);
  const requested = nextPage ?? place.page;
  const needsMore =
    hasNextPage &&
    (searchingAll || backups.length === 0 || (requested > 0 && listed.length <= requested * LIST_PAGE_SIZE));
  useEffect(() => {
    if (needsMore && !isFetching && !isFetchNextPageError) void fetchNextPage();
  }, [needsMore, isFetching, isFetchNextPageError, fetchNextPage]);
  const page = pagedRows(listed, place.page);
  const waitingForPage = hasNextPage && place.page > page.page;
  const changePage = (target: number) => {
    if (target >= page.pages) {
      if (hasNextPage) setNextPage(target);
    } else {
      setNextPage(undefined);
      place.setPage(target);
    }
  };
  useEffect(() => {
    if (nextPage === undefined) return;
    if (listed.length > nextPage * LIST_PAGE_SIZE) {
      setNextPage(undefined);
      place.setPage(nextPage);
    } else if (!hasNextPage) {
      setNextPage(undefined);
    }
  }, [listed.length, nextPage, hasNextPage, place.setPage]);
  const loadingList = query.isPending || (needsMore && query.isFetchingNextPage && (waitingForPage || !backups.length));
  const cancelHistory = () => {
    setSearchingAll(false);
    setNextPage(undefined);
  };
  const policies = usePolicies().data?.policies ?? [];
  const destinationsRead = useDestinations();
  const destinations = destinationsRead.data?.destinations ?? [];
  const action =
    volumes.length > 0 ? (
      <Button variant="primary" icon="archive" onClick={() => setTaking(true)}>
        <Trans>Back up now</Trans>
      </Button>
    ) : null;

  const failed = backups.filter((b) => b.state === BackupState.FAILED).length;
  const shown = backups.length;
  const runs = [
    hasNextPage ? t`${shown} newest` : plural(shown, { one: "# backup", other: "# backups" }),
    failed > 0 ? plural(failed, { one: "# failed", other: "# failed" }) : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const where =
    destinations.length > 0
      ? t`On each server's disk, and sealed off it where a schedule says`
      : t`On each server's disk alone, unencrypted`;
  const recent = (
    <section className="backups-runs" aria-labelledby="backups-runs">
      <div className="backups-runs-head">
        <h2 className="dw-text-heading" id="backups-runs">
          <Trans>Recent runs</Trans>
        </h2>
        <span className="backups-runs-lead">{runs}</span>
        {phone ? action : null}
      </div>
      {backups.length > 0 ? (
        <div className="list-bar">
          <ListSearchField
            label={t`Filter backups`}
            placeholder={t`Filter by volume, destination or server`}
            size={phone ? "lg" : "sm"}
            value={place.typed}
            onChange={(value) => {
              cancelHistory();
              place.setTyped(value);
            }}
          />
          {phone ? (
            <ListSortField place={place} columns={{ started: t`Started`, volume: t`Volume`, size: t`Size` }} />
          ) : null}
          {states.length > 1 || search.state ? (
            <SelectField
              aria-label={t`Status`}
              size="sm"
              value={search.state ?? ""}
              options={[
                { value: "", label: t`Every state` },
                ...states.map((state) => ({
                  value: state,
                  label: state === "done" ? t`Done` : state === "failed" ? t`Failed` : t`Running`,
                })),
              ]}
              onChange={(event) => {
                cancelHistory();
                onSearch?.({ ...search, page: undefined, state: event.target.value || undefined });
              }}
            />
          ) : null}
        </div>
      ) : null}
      {filtered && hasNextPage ? (
        <div className="list-bar">
          <span className="backups-runs-lead">
            <Trans>Filter and sort use loaded backups.</Trans>
          </span>
          <Button
            size="sm"
            onClick={() => {
              setSearchingAll(true);
              if (isFetchNextPageError) void fetchNextPage();
            }}
            loading={searchingAll && !isFetchNextPageError}
          >
            <Trans>Search all backups</Trans>
          </Button>
        </div>
      ) : null}
      {waitingForPage ? (
        query.isFetchingNextPage ? (
          <Loading label={t`Loading backups`}>
            <Panel flush>
              <SkeletonRows rows={4} lead="icon" end="pill" />
            </Panel>
          </Loading>
        ) : null
      ) : listed.length === 0 ? (
        <EmptyState
          icon="search"
          title={t`Nothing matches`}
          action={
            <Button
              size="lg"
              onClick={() => {
                cancelHistory();
                place.clearQuery();
                onSearch?.({ ...search, q: undefined, page: undefined, state: undefined });
              }}
            >
              <Trans>Clear the filter</Trans>
            </Button>
          }
        ></EmptyState>
      ) : (
        <>
          <BackupRows backups={page.rows} place={place} />
          <ListPager
            page={page}
            onPage={changePage}
            more={hasNextPage}
            busy={nextPage !== undefined && query.isFetchingNextPage}
          />
        </>
      )}
    </section>
  );
  return (
    <Page title={t`Backups`} lead={where} phoneSub={where} back="/more" actions={backups.length > 0 ? action : null}>
      {query.error ? (
        <Callout
          tone="critical"
          title={t`Dockwell can't read its backups`}
          actions={
            <Button onClick={() => void (isFetchNextPageError ? fetchNextPage() : query.refetch())}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(query.error)}
        </Callout>
      ) : null}
      <div className="backups-columns">
        <div className="backups-main">
          <UncheckedSchedules policies={policies} />
          {volumes.length > 0 ? (
            <PoliciesPanel
              policies={policies}
              onAdd={() => setScheduling({})}
              onEdit={(policy) => setScheduling({ editing: policy })}
            />
          ) : null}
          {loadingList ? (
            <section className="backups-runs">
              <div className="backups-runs-head">
                <h2 className="dw-text-heading">
                  <Trans>Recent runs</Trans>
                </h2>
              </div>
              <Loading label={t`Loading backups`}>
                <Panel flush>
                  <SkeletonRows rows={4} lead="icon" end="pill" />
                </Panel>
              </Loading>
            </section>
          ) : backups.length === 0 && !hasNextPage && !query.error ? (
            volumes.length > 0 ? (
              <EmptyState icon="archive" title={t`No backups yet`} action={action}>
                <Trans>Back up a volume before you change what's in it. A database's backup stops nothing.</Trans>
              </EmptyState>
            ) : (
              <EmptyState
                icon="archive"
                title={t`Nothing to back up yet`}
                action={
                  // The two ways a volume comes: a database from the catalog, or an app that keeps one.
                  <>
                    <RouterButtonLink to="/catalog" search={{ category: "databases" }} icon="database">
                      <Trans>Install a database</Trans>
                    </RouterButtonLink>
                    <RouterButtonLink to="/apps/new" variant="ghost" icon="plus">
                      <Trans>New app</Trans>
                    </RouterButtonLink>
                  </>
                }
              >
                <Trans>A backup is of a volume, and no app keeps one yet.</Trans>
              </EmptyState>
            )
          ) : phone && backups.length > 0 ? (
            recent
          ) : null}
        </div>
        {/* Where they go, and how their restores test, in one column, as the Backups mock has them (#793). */}
        <div className="backups-side">
          <DestinationsPanel destinations={destinations} pending={destinationsRead.isPending} />
          <RestoreTests policies={policies} backups={backups} />
        </div>
      </div>
      {/* Under both columns on a wide screen, so each run's volume, state and reason have the page's width. */}
      {!phone && !loadingList && backups.length > 0 ? recent : null}
      {taking ? <BackUpSheet volumes={volumes} destinations={destinations} onClose={() => setTaking(false)} /> : null}
      {scheduling ? (
        <PolicySheet
          volumes={volumes}
          destinations={destinations}
          editing={scheduling.editing}
          onClose={() => setScheduling(undefined)}
        />
      ) : null}
    </Page>
  );
}

/**
 * What a backup's actions do, with the sheets they open and how they went: restore
 * through a plan, check it with a drill, fetch it back from a destination, and
 * delete it. A list of backups and a backup's own page share them.
 */
export function useBackupActions() {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(BackupService, transport), [transport]);
  const queries = useQueryClient();
  const [deleting, setDeleting] = useState<Backup>();
  const [choosing, setChoosing] = useState<Backup>();
  const [bringBackFailure, setBringBackFailure] = useState<string>();
  // A check in flight, and why one was refused, each on its own backup.
  const [checking, setChecking] = useState<ReadonlySet<string>>(new Set());
  const [refused, setRefused] = useState<ReadonlyMap<string, string>>(new Map());
  const refreshOlder = useRefreshOlderBackup();
  const listed = (id: string) =>
    Promise.all([
      refreshOlder(id),
      queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.listBackups, cardinality: undefined }),
      }),
      queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.getBackup, cardinality: "finite" }),
      }),
    ]);
  // A drill runs on its server and says how it went on the backup (ADR 0068).
  const check = async (backup: Backup) => {
    if (checking.has(backup.id)) return;
    setChecking((was) => new Set(was).add(backup.id));
    setRefused((was) => {
      const next = new Map(was);
      next.delete(backup.id);
      return next;
    });
    try {
      await client.drillBackup({ id: backup.id });
      await listed(backup.id);
    } catch (error) {
      setRefused((was) => new Map(was).set(backup.id, sentence(error)));
    } finally {
      setChecking((was) => {
        const next = new Set(was);
        next.delete(backup.id);
        return next;
      });
    }
  };
  const whole = useRestoreWhole();
  const restore = (backup: Backup) => whole.restore(backup);
  const bringBack = async (backup: Backup, server: string) => {
    setChoosing(undefined);
    setBringBackFailure(undefined);
    try {
      await client.fetchBackup({ id: backup.id, server });
      await listed(backup.id);
    } catch (error) {
      setBringBackFailure(sentence(error));
    }
  };
  const fetchBack = (b: Backup) => (b.fetchOnto.length > 1 ? setChoosing(b) : void bringBack(b, b.fetchOnto[0] ?? ""));
  const view = (
    <>
      {bringBackFailure ? (
        <Callout tone="critical" title={t`This backup didn't come back`}>
          {bringBackFailure}
        </Callout>
      ) : null}
      {deleting ? <DeleteSheet backup={deleting} client={client} onClose={() => setDeleting(undefined)} /> : null}
      {choosing ? (
        <FetchSheet
          backup={choosing}
          onFetch={(server) => void bringBack(choosing, server)}
          onClose={() => setChoosing(undefined)}
        />
      ) : null}
      {whole.confirming}
    </>
  );
  return { restore, check: (b: Backup) => void check(b), fetchBack, remove: setDeleting, checking, refused, view };
}

/**
 * Backups as a list, newest first, each of which restores through a plan, comes
 * back from a destination, or is deleted, with the sheets that do it. The Backups
 * page shows every volume's; a database's page and a volume's show their own.
 */
export function BackupRows({
  backups,
  caption,
  place,
}: {
  backups: readonly Backup[];
  caption?: string;
  /** Where the Backups page's list is, for its sortable heads; a volume's own list has none. */
  place?: ListPlace;
}) {
  const { t } = useLingui();
  const actions = useBackupActions();
  return (
    <>
      <BackupsList
        backups={backups}
        caption={caption ?? t`Backups, newest first`}
        onRestore={actions.restore}
        onFetch={actions.fetchBack}
        onDelete={actions.remove}
        onCheck={actions.check}
        checking={actions.checking}
        refused={actions.refused}
        place={place}
      />
      {actions.view}
    </>
  );
}

function BackupsList({
  backups,
  caption,
  onRestore,
  onFetch,
  onDelete,
  onCheck,
  checking,
  refused,
  place,
}: {
  backups: readonly Backup[];
  caption: string;
  onRestore: (backup: Backup) => void;
  onFetch: (backup: Backup) => void;
  onDelete: (backup: Backup) => void;
  onCheck: (backup: Backup) => void;
  checking: ReadonlySet<string>;
  refused: ReadonlyMap<string, string>;
  place?: ListPlace | undefined;
}) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const view = useFleetView();
  const places = usePlaces();
  const method = useMethodWord();
  const stateWord = useStateWord();
  const time = useTimeWords();
  const whenWords = useWhen();
  const now = useControlPlaneNow(30_000);
  const fleet = view.fleet;
  const several = new Set(backups.map((b) => b.environmentId)).size > 1;
  const where = (b: Backup) => {
    const place = several ? places.label(b.environmentId) : undefined;
    const server = fleet ? b.serverName : undefined;
    return [place, server].filter(Boolean).join(" · ");
  };
  // When it started, which opens the backup's own page.
  const when = (b: Backup) =>
    b.startedAt ? (
      // Relative within a day, with the whole date on hover (#793).
      <span title={formatDateTime(timestampDate(b.startedAt), i18n.locale)}>
        <RefLink kind="backup" id={b.id} look="plain">
          {whenWords(timestampDate(b.startedAt), now)}
        </RefLink>
      </span>
    ) : (
      ""
    );
  const took = (b: Backup) =>
    b.startedAt && b.finishedAt ? time.span(timestampMs(b.finishedAt) - timestampMs(b.startedAt)) : "";
  const state = (b: Backup) => (
    <div className="backup-state">
      <StatusBadge status={backupStatus(b)} label={stateWord(b)} labelIsState />
      {b.state === BackupState.FAILED ? (
        // Two lines at most in the list; the whole of it on hover and on the backup's own page.
        <span className="backup-why is-reason" title={b.error}>
          {b.error}
        </span>
      ) : null}
      {b.progress ? <span className="backup-why">{b.progress}</span> : null}
      {b.copy === BackupCopy.STORED ? (
        <span className="backup-why">{b.onServer ? t`Also on ${b.destination}` : t`On ${b.destination}`}</span>
      ) : null}
      {b.sealedEarlier ? <span className="backup-why">{t`Sealed with an earlier passphrase`}</span> : null}
      {b.state === BackupState.DONE && !b.onServer && b.copy !== BackupCopy.STORED ? (
        <span className="backup-why">{t`Its server can't restore this now: it's away, or no longer has the volume`}</span>
      ) : null}
      {b.copy === BackupCopy.STORED && !b.onServer && b.fetchOnto.length === 0 ? (
        <span className="backup-why">{t`To fetch it, put ${b.volume} on a connected server first`}</span>
      ) : null}
      {b.fetchError ? <span className="backup-why">{t`It couldn't be fetched: ${b.fetchError}`}</span> : null}
      <DrillLine
        backup={b}
        // Only a whole backup its server has now can be checked there.
        onCheck={b.onServer && b.state === BackupState.DONE ? () => onCheck(b) : undefined}
        pending={checking.has(b.id)}
      />
      {refused.get(b.id) ? (
        <span className="backup-why is-failed" role="alert">
          {t`Can't check it now: ${refused.get(b.id)}`}
        </span>
      ) : null}
      {b.copy === BackupCopy.PENDING ? <span className="backup-why">{t`Sending to ${b.destination}`}</span> : null}
      {b.copy === BackupCopy.FAILED ? (
        <span className="backup-why">{t`It didn't reach ${b.destination}: ${b.copyError}`}</span>
      ) : null}
      {b.filesChanged > 0n ? (
        <span className="backup-why">
          {plural(Number(b.filesChanged), {
            one: "# file changed while it copied",
            other: "# files changed while it copied",
          })}
        </span>
      ) : null}
    </div>
  );
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay();
  const navigate = useNavigate();
  const rowMenu = useRowMenu<Backup>();
  // A backup's menu: the same steps its row's buttons take, in one place, each as the reader may.
  const menuFor = (b: Backup): MenuAction[] => {
    if (b.state === BackupState.RUNNING) return [];
    const restore = may(BackupService.method.restoreBackup, { environment: b.environmentId });
    const fetch = may(BackupService.method.fetchBackup, { environment: b.environmentId });
    const remove = may(BackupService.method.deleteBackup, { environment: b.environmentId });
    const off = (offer: typeof restore) =>
      offer.allowed ? {} : { off: offer.reason || undefined, disabled: !offer.reason };
    return [
      {
        id: "open",
        icon: "archive",
        label: t`Open`,
        onSelect: () => void navigate({ to: "/backups/$backupId", params: { backupId: b.id } }),
      },
      ...(b.onServer
        ? [{ id: "restore", icon: "undo" as const, label: t`Restore`, ...off(restore), onSelect: () => onRestore(b) }]
        : []),
      ...(b.copy === BackupCopy.STORED && !b.onServer && b.fetchOnto.length > 0
        ? [{ id: "fetch", icon: "download" as const, label: t`Fetch`, ...off(fetch), onSelect: () => onFetch(b) }]
        : []),
      ...(b.onServer && b.state === BackupState.DONE
        ? [
            {
              id: "check",
              icon: "check" as const,
              label: t`Check it now`,
              ...(checking.has(b.id) ? { disabled: true } : {}),
              onSelect: () => onCheck(b),
            },
          ]
        : []),
      { id: "delete", icon: "trash", label: t`Delete`, danger: true, ...off(remove), onSelect: () => onDelete(b) },
    ];
  };
  const actions = (b: Backup) =>
    b.state === BackupState.RUNNING ? null : (
      <span className="backup-actions">
        {b.onServer ? (
          <Button
            size="sm"
            icon="undo"
            onClick={() => onRestore(b)}
            {...may(BackupService.method.restoreBackup, { environment: b.environmentId }).off}
          >
            <Trans>Restore</Trans>
          </Button>
        ) : null}
        {b.copy === BackupCopy.STORED && !b.onServer && b.fetchOnto.length > 0 ? (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => onFetch(b)}
            {...may(BackupService.method.fetchBackup, { environment: b.environmentId }).off}
          >
            <Trans>Fetch</Trans>
          </Button>
        ) : null}
        <Button
          size="sm"
          variant="ghost"
          icon="trash"
          aria-label={t`Delete this backup`}
          onClick={() => onDelete(b)}
          {...may(BackupService.method.deleteBackup, { environment: b.environmentId }).off}
        />
        <ActionMenu label={t`Actions for this backup of ${b.volume}`} actions={menuFor(b)} />
      </span>
    );
  const columns: TableColumn<Backup>[] = [
    {
      key: "started",
      header: place ? <SortHeader label={t`Started`} sortKey="started" place={place} /> : t`Started`,
      sort: place?.sortOf("started"),
      width: 148,
      render: when,
    },
    {
      // How it was taken under its volume, with where, as a phone's line has them, so the status has the room.
      key: "volume",
      header: place ? <SortHeader label={t`Volume`} sortKey="volume" place={place} /> : t`Volume`,
      sort: place?.sortOf("volume"),
      render: (b) => {
        const under = [method(b.method, b.database), where(b)].filter(Boolean).join(" · ");
        return (
          <div className="backup-state">
            <code>
              <VolumeName backup={b} />
            </code>
            {under ? <span className="backup-why">{under}</span> : null}
          </div>
        );
      },
    },
    { key: "state", header: t`Status`, render: state },
    {
      key: "size",
      header: place ? <SortHeader label={t`Size`} sortKey="size" place={place} /> : t`Size`,
      sort: place?.sortOf("size"),
      align: "right",
      width: 92,
      render: (b) => (b.sizeBytes > 0n ? formatBytes(b.sizeBytes, i18n.locale) : ""),
    },
    { key: "took", header: t`Took`, align: "right", width: 72, muted: true, render: took },
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 168,
      align: "right",
      render: actions,
    },
  ];
  // How it was taken, its size and where, after when it started on a phone's line.
  const details = (b: Backup) =>
    [method(b.method, b.database), b.sizeBytes > 0n ? formatBytes(b.sizeBytes, i18n.locale) : "", where(b)]
      .filter(Boolean)
      .join(" · ");
  if (phone) {
    return (
      <ul className="backup-phone">
        {backups.map((b) => (
          <li key={b.id}>
            <div className="backup-phone-head">
              <code>
                <VolumeName backup={b} />
              </code>
              {actions(b)}
            </div>
            {state(b)}
            <span className="backup-why">
              {when(b)}
              {b.startedAt && details(b) ? " · " : null}
              {details(b)}
            </span>
          </li>
        ))}
      </ul>
    );
  }
  return (
    <>
      <Table<Backup>
        className="backup-table"
        columns={columns}
        rows={backups}
        caption={caption}
        onRowClick={(b) => void navigate({ to: "/backups/$backupId", params: { backupId: b.id } })}
        onRowContextMenu={(b, at, el) =>
          rowMenu.open(b, at, el, { label: t`Actions for this backup of ${b.volume}`, actions: menuFor(b) })
        }
      />
      {rowMenu.element}
    </>
  );
}

/** A backup's volume, as a link to its page. */
function VolumeName({ backup }: { backup: Backup }) {
  return backup.volumeId ? (
    <RefLink kind="volume" id={backup.volumeId} look="plain">
      {backup.volume}
    </RefLink>
  ) : (
    backup.volume
  );
}

/** Asks which server to fetch a backup onto, where its volume is on several. */
function FetchSheet({
  backup,
  onFetch,
  onClose,
}: {
  backup: Backup;
  onFetch: (server: string) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const [server, setServer] = useState(backup.fetchOnto[0] ?? "");
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Fetch this backup of ${backup.volume}`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={() => onFetch(server)}>
            <Trans>Fetch</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        <SelectField
          label={t`Onto`}
          value={server}
          options={backup.fetchOnto.map((name) => ({ value: name, label: name }))}
          onChange={(event) => setServer(event.target.value)}
        />
      </div>
    </Sheet>
  );
}

/** Deleting a backup can't be taken back, so it asks. */
function DeleteSheet({
  backup,
  client,
  onClose,
}: {
  backup: Backup;
  client: ReturnType<typeof createClient<typeof BackupService>>;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const queries = useQueryClient();
  const refreshOlder = useRefreshOlderBackup();
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const failed = backup.state === BackupState.FAILED;
  const stored = backup.copy === BackupCopy.STORED;
  const destination = backup.destination;
  // A copy only an earlier passphrase opens is no way back, so the server's has to stay.
  const canKeep = stored && backup.onServer && !backup.sealedEarlier;
  const remove = async (serverOnly: boolean) => {
    setBusy(true);
    setFailure(undefined);
    try {
      await client.deleteBackup({ id: backup.id, serverOnly });
      await refreshOlder(backup.id, !serverOnly);
      await queries.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: BackupService.method.listBackups, cardinality: undefined }),
      });
      onClose();
    } catch (error) {
      setFailure(sentence(error));
      setBusy(false);
    }
  };
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Delete this backup of ${backup.volume}`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          {canKeep ? (
            <Button variant="secondary" loading={busy} onClick={() => void remove(true)}>
              <Trans>Remove from the server only</Trans>
            </Button>
          ) : null}
          <Button variant="danger" icon="trash" loading={busy} onClick={() => void remove(false)}>
            {stored && backup.onServer ? <Trans>Delete from both</Trans> : <Trans>Delete it</Trans>}
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        <p className="backup-sheet-line">
          {failed ? (
            <Trans>This backup failed, so nothing is on the server. This removes the record of it.</Trans>
          ) : canKeep ? (
            <Trans>
              This removes it from the server's disk and from {destination}. Nothing brings it back. Remove it from the
              server alone to keep it on {destination}, from where you can fetch it.
            </Trans>
          ) : stored && backup.onServer ? (
            <Trans>
              This removes it from the server's disk and from {destination}, where it was sealed with an earlier
              passphrase. Nothing brings it back.
            </Trans>
          ) : stored ? (
            <Trans>This removes it from {destination}, the last place that has it. Nothing brings it back.</Trans>
          ) : backup.method === BackupMethod.UNSPECIFIED ? null : (
            <Trans>This removes it from the server's disk. Nothing brings it back.</Trans>
          )}
        </p>
        {failure ? (
          <Callout tone="critical" title={t`It wasn't deleted`}>
            {failure}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

/**
 * How a backup's newest restore drill went, in a line (ADR 0068): restored and
 * checked, unpacked whole, failed with what failed, or not checked with why, which
 * says nothing of the backup.
 */
export function DrillLine({ backup, onCheck, pending }: { backup: Backup; onCheck?: () => void; pending?: boolean }) {
  const { t } = useLingui();
  const time = useTimeWords();
  // An action on what you can see stays where it is, off, with why (ADR 0059).
  const may = useMay();
  const drill = backup.drill;
  const check = (label: string) =>
    onCheck ? (
      <Button
        size="sm"
        variant="ghost"
        className="backup-drill-action"
        onClick={onCheck}
        loading={pending}
        {...may(BackupService.method.drillBackup, { environment: backup.environmentId }).off}
      >
        {label}
      </Button>
    ) : null;
  if (!drill) return onCheck ? <span className="backup-why backup-drill">{check(t`Test restore`)}</span> : null;
  const ago = drill.finishedAt ? time.ago(timestampDate(drill.finishedAt), Date.now()) : "";
  const why = drill.why.replace(/\.$/, "");
  const again = check(t`Test again`);
  // What it says is two lines at most, whole on hover, and its action stays in sight after it.
  const said = (text: string, failed = false) => (
    <span className={cx("backup-why", "backup-drill", failed && "is-failed")}>
      <span className="backup-drill-said" title={text}>
        {text}
      </span>
      {again}
    </span>
  );
  switch (drill.result) {
    case DrillResult.RUNNING:
      return <span className="backup-why backup-drill">{t`Testing a restore`}</span>;
    case DrillResult.PASSED:
      return said(drill.unpackedOnly ? t`Unpacked whole ${ago}` : t`Restore tested ${ago}`);
    case DrillResult.FAILED:
      return said(t`Restore test failed ${ago}: ${why}`, true);
  }
  return said(t`Not tested: ${why}`);
}

/**
 * Schedules made before restore drills check none (ADR 0068). One button plans
 * weekly checks for all of them, for a person to review and apply; nothing turns
 * them on by itself, as a check costs disk and time their owner didn't plan for.
 */
function UncheckedSchedules({ policies }: { policies: readonly BackupPolicyInfo[] }) {
  const sheet = useChangeSheet();
  const may = useMay();
  const unchecked = policies.filter((p) => p.spec && p.spec.drill === BackupPolicy_Drill.UNSPECIFIED);
  if (unchecked.length === 0) return null;
  const plan = () =>
    sheet.open({
      kind: "propose",
      summary: plural(unchecked.length, {
        one: "Test a restore of # schedule's backups weekly",
        other: "Test a restore of # schedules' backups weekly",
      }),
      edits: unchecked.map((p) => {
        // The whole policy as it is, but its check.
        const value = clone(BackupPolicySchema, p.spec ?? create(BackupPolicySchema));
        value.drill = BackupPolicy_Drill.WEEKLY;
        return {
          resource: { kind: "backup.policy", scope: p.environmentId, name: p.name },
          spec: { kind: { case: "backupPolicy" as const, value } },
        };
      }),
    });
  return (
    <Callout
      tone="info"
      title={plural(unchecked.length, {
        one: "# schedule never tests a restore",
        other: "# schedules never test a restore",
      })}
      actions={
        <Button
          size="sm"
          onClick={plan}
          {...may(ChangeService.method.proposeChange, { environment: unchecked[0]?.environmentId ?? "" }).off}
        >
          <Trans>Test weekly</Trans>
        </Button>
      }
    />
  );
}

/**
 * How each schedule's restores test, beside where its backups go, as the Backups mock
 * draws Restore drills: how often, and how the newest test of its volume went (#793).
 */
function RestoreTests({ policies, backups }: { policies: readonly BackupPolicyInfo[]; backups: readonly Backup[] }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const places = usePlaces();
  const several = new Set(policies.map((p) => p.environmentId)).size > 1;
  if (policies.length === 0) return null;
  return (
    <Panel title={t`Restore tests`} flush>
      <ul className="backup-policies">
        {policies.map((p) => {
          const volume = p.spec?.volume || p.spec?.link || "";
          const tested = backups.find(
            (b) => b.drill?.finishedAt && b.environmentId === p.environmentId && b.volume === p.spec?.volume,
          );
          const drill = tested?.drill;
          const ago = drill?.finishedAt ? time.ago(timestampDate(drill.finishedAt), now) : "";
          const cadence =
            p.spec?.drill === BackupPolicy_Drill.WEEKLY
              ? t`Weekly`
              : p.spec?.drill === BackupPolicy_Drill.MONTHLY
                ? t`Monthly`
                : t`Never`;
          return (
            <li key={p.id} className="backup-policy">
              <div className="backup-state">
                <span>
                  <code>{volume}</code>
                  {several ? <span className="backup-why backup-place"> · {places.label(p.environmentId)}</span> : null}
                </span>
                <span className="backup-why">{cadence}</span>
              </div>
              <div className="backup-state">
                {drill?.result === DrillResult.PASSED ? (
                  <StatusBadge status="healthy" label={t`Passed ${ago}`} labelIsState />
                ) : drill?.result === DrillResult.FAILED ? (
                  <StatusBadge status="down" label={t`Failed ${ago}`} labelIsState />
                ) : (
                  <StatusBadge status="unknown" label={t`Not tested yet`} labelIsState />
                )}
              </div>
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}
