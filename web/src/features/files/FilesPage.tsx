// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { skipToken, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type DeletedFile,
  downloadFileURL,
  type FileEntry,
  FileService,
  FileType,
  type ListFilesResponse,
  type Server,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import { isTransient } from "../../app/api";
import { isRoleRefusal, reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { useControlPlaneNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { FileViewer } from "./FileViewer";
import { FolderTree } from "./FolderTree";
import {
  ActionsSheet,
  DeletedSheet,
  DeleteSheet,
  MoveSheet,
  NameSheet,
  type Naming,
  RowActions,
  useRestore,
  useRestoreProblem,
} from "./manage";
import { crumbs, join, nameOf, parentOf } from "./paths";
import { type Chosen, carriesFiles, chosenFrom, UploadSheet } from "./upload";
import "./FilesPage.css";

/** Where you are on a server, from the URL. */
export interface FilesSearch {
  /** The folder on show. */
  path?: string;
  /** The file open beside it. */
  file?: string;
}

/** A path that isn't answering won't by the next try, and each try costs someone the wait. */
/** value once it has stayed the same for ms: what a search asks the server for, not each key typed. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

function retryListing(failures: number, error: unknown): boolean {
  return failures < 2 && isTransient(error) && reasonOf(error) !== "path_not_answering";
}

interface Place {
  label: string;
  path: string;
  icon: IconName;
}

/**
 * Places worth a click on most servers, as the mock has them, then the server's
 * own root. They're the same on every server, and one may not be there; pinning
 * your own is the file manager's to come (#243).
 */
function usePlaces(): { common: Place[]; server: Place[] } {
  const { t } = useLingui();
  return {
    common: [
      { label: t`Websites`, path: "/var/www", icon: "browser" },
      { label: t`Logs`, path: "/var/log", icon: "file" },
      { label: t`Docker volumes`, path: "/var/lib/docker/volumes", icon: "layers" },
    ],
    server: [{ label: t`Server root`, path: "/", icon: "server" }],
  };
}

/** The place a folder is in: the deepest one whose path holds it, the root when none does. */
function placeOf(folder: string, places: readonly Place[]): string | undefined {
  let best: Place | undefined;
  for (const place of places) {
    const holds = place.path === "/" || folder === place.path || folder.startsWith(`${place.path}/`);
    if (holds && (!best || place.path.length > best.path.length)) best = place;
  }
  return best?.path;
}

/** One group of places, named by its heading, with the one you're in marked. */
function PlaceGroup({
  title,
  places,
  current,
  serverId,
}: {
  title: string;
  places: readonly Place[];
  current: string | undefined;
  serverId: string;
}) {
  const id = useId();
  return (
    // biome-ignore lint/a11y/useSemanticElements: a fieldset is for a form's fields, and these are links
    <div role="group" aria-labelledby={id} className="files-places-group">
      <div id={id} className="files-places-head">
        {title}
      </div>
      {places.map((place) => (
        <RouterButtonLink
          key={place.path}
          variant="ghost"
          size="sm"
          icon={place.icon}
          to="/servers/$serverId/files"
          params={{ serverId }}
          search={{ path: place.path }}
          title={place.path}
          className={place.path === current ? "is-current" : undefined}
          // Inside a place it's where you are; on the place itself the router says it's the page.
          aria-current={place.path === current ? "location" : undefined}
        >
          {place.label}
        </RouterButtonLink>
      ))}
    </div>
  );
}

/** A file manager on a server's own page, through its agent. */
export function FilesPage({ serverId, search }: { serverId: string; search: FilesSearch }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const file = search.file;
  const folder = search.path ?? (file ? parentOf(file) : "/");
  // On a phone an open file takes the whole screen, under a header of its own.
  const reading = phone && file !== undefined;
  const { t } = useLingui();
  return (
    <ServerFrame
      id={serverId}
      noTabs={reading}
      phoneHead={
        reading && file
          ? {
              phoneTitle: nameOf(file),
              phoneMachineTitle: true,
              phoneSub: parentOf(file),
              backLink: (
                <Link
                  to="/servers/$serverId/files"
                  params={{ serverId }}
                  search={{ path: folder }}
                  className="mobile-header-button"
                  aria-label={t`Back to ${folder}`}
                >
                  <Icon name="arrow-left" size={20} />
                </Link>
              ),
            }
          : undefined
      }
    >
      {(server) => <Files server={server} folder={folder} file={file} reading={reading} />}
    </ServerFrame>
  );
}

interface Row {
  name: string;
  entry: FileEntry;
  folder: boolean;
}

/** Whether an entry opens as a folder, itself or through a link. */
function opensAsFolder(entry: FileEntry): boolean {
  return (
    entry.type === FileType.DIRECTORY || (entry.type === FileType.SYMLINK && entry.linkType === FileType.DIRECTORY)
  );
}

/** Folders first, then by name, as the mock has them. */
/** The rows of a listing: folders first, then by name; or by name alone, for a folder read a page at a time. */
function rowsOf(entries: readonly FileEntry[], byNameAlone = false): Row[] {
  return entries
    .map((entry) => ({ name: entry.name, entry, folder: opensAsFolder(entry) }))
    .sort(
      (a, b) =>
        (byNameAlone ? 0 : Number(b.folder) - Number(a.folder)) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0),
    );
}

/** What's open over the listing, if anything. */
type Open =
  | { kind: "actions"; entry: FileEntry }
  | { kind: "name"; naming: Naming }
  | { kind: "move"; entry: FileEntry }
  | { kind: "delete"; entry: FileEntry }
  | { kind: "deleted" }
  | { kind: "upload"; chosen?: Chosen };

/** What a delete left to say, with a way to put it back. */
interface Deleted {
  path: string;
  kept?: DeletedFile;
}

/** Whether a row can be renamed, moved or deleted: not what's refused, nor a name that isn't text. */
function changeable(row: Row): boolean {
  return !row.entry.refused && !row.entry.unreadableName;
}

/** Whether a row downloads: a file, or a folder as a .zip. A link is neither. */
function downloads(row: Row): boolean {
  return changeable(row) && (row.entry.type === FileType.FILE || row.entry.type === FileType.DIRECTORY);
}

function iconOf(row: Row): IconName {
  if (row.entry.type === FileType.SYMLINK) return "link";
  return row.folder ? "folder" : "file";
}

interface FilesProps {
  server: Server;
  folder: string;
  file: string | undefined;
  /** An open file has the screen to itself. It stays where it is in the tree, so turning a phone keeps what you typed. */
  reading: boolean;
}

function Files({ server, folder, file, reading }: FilesProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const now = useControlPlaneNow();
  const words = useTimeWords();
  const places = usePlaces();
  const treeHead = useId();
  const current = placeOf(folder, [...places.common, ...places.server]);
  const [filter, setFilter] = useState("");
  const [sheet, setSheet] = useState<Open>();
  const [deleted, setDeleted] = useState<Deleted>();
  const [restoring, setRestoring] = useState(false);
  const [restoreFailure, setRestoreFailure] = useState<string>();
  // Files dragged over the listing, which a drop uploads here.
  const [dragging, setDragging] = useState(false);
  const serverId = server.id;
  const putBack = useRestore(serverId);
  const restoreProblem = useRestoreProblem(serverId);
  // A refused listing is on record, so it isn't asked again each time the window takes focus; nor is a
  // folder larger than a page, as its agent reads every name of it for each page.
  const query = useQuery(
    FileService.method.listFiles,
    { serverId, path: folder },
    {
      retry: retryListing,
      refetchOnWindowFocus: (q) => q.state.status !== "error" && !q.state.data?.nextPage,
    },
  );
  const listing = query.data;
  const shown = listing?.path ?? folder;
  // A folder that holds more than a listing carries is searched by its agent, through every name; a smaller
  // one, here, as you type.
  const big = Boolean(listing?.nextPage);
  const wanted = useSettled(filter.trim(), 300);
  const searching = big && wanted !== "";
  const search = useQuery(
    FileService.method.listFiles,
    searching ? { serverId, path: folder, match: wanted } : skipToken,
    { retry: retryListing, refetchOnWindowFocus: false },
  );
  const first = searching ? search.data : listing;
  // The pages after the first, in name order, as Show more brings them. They belong to the folder, the search
  // and where the first page ended, so a first page read again that ends where it did keeps them, and another
  // folder or search starts over.
  const pagesOf = `${folder}\0${searching ? wanted : ""}\0${first?.nextPage ?? ""}`;
  const [after, setAfter] = useState<{ of?: string; pages: ListFilesResponse[]; problem?: string }>({ pages: [] });
  const more = after.of === pagesOf ? after.pages : [];
  const moreProblem = after.of === pagesOf ? after.problem : undefined;
  const [loadingMore, setLoadingMore] = useState(false);
  const nextPage = (more.at(-1) ?? first)?.nextPage ?? "";
  const transport = useTransport();
  // A page asked for as the person leaves isn't read to its end for nobody.
  const asking = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => asking.current?.abort(), []);
  const showMore = async () => {
    const of = pagesOf;
    asking.current?.abort();
    const ask = new AbortController();
    asking.current = ask;
    setLoadingMore(true);
    try {
      const page = await createClient(FileService, transport).listFiles(
        { serverId, path: folder, match: searching ? wanted : "", page: nextPage },
        { signal: ask.signal },
      );
      setAfter((was) => ({ of, pages: [...(was.of === of ? was.pages : []), page] }));
    } catch (err) {
      if (ask.signal.aborted) return;
      setAfter((was) => ({ of, pages: was.of === of ? was.pages : [], problem: sentence(err) }));
    } finally {
      if (asking.current === ask) setLoadingMore(false);
    }
  };

  const rows = useMemo(() => {
    const typed = filter.trim().toLowerCase();
    // A folder read a page at a time lists by name alone, so a page brought later goes below what's shown.
    const all = rowsOf(
      [first, ...more].flatMap((page) => page?.entries ?? []),
      big,
    );
    return typed ? all.filter((row) => row.name.toLowerCase().includes(typed)) : all;
  }, [first, more, filter, big]);

  const open = (row: Row) => {
    if (row.entry.unreadableName) return;
    const path = join(shown, row.name);
    void navigate({
      to: "/servers/$serverId/files",
      params: { serverId },
      search: row.folder ? { path } : { path: shown, file: path },
    });
  };

  // Beside an open file, as on a phone, a row's actions are behind one button, so
  // its name keeps its room.
  const oneButton = phone || file !== undefined;
  // Beside an open file the list is about 530 px wide at 1440, so the columns of facts close up to what they
  // hold, as Server-Files draws them, and a name keeps twice the room.
  const tight = file !== undefined;
  const columns = useMemo<TableColumn<Row>[]>(() => {
    const name: TableColumn<Row> = {
      key: "name",
      header: t`Name`,
      render: (row) => (
        <span className="files-name">
          <span className="files-icon">
            <Icon name={iconOf(row)} size={15} />
          </span>
          <span className="files-entry-name" title={row.name}>
            {row.name}
          </span>
          {row.entry.linkTarget ? <span className="files-link-target">→ {row.entry.linkTarget}</span> : null}
          {row.entry.refused ? (
            <span className="files-icon" title={t`Dockwell doesn't open this`}>
              <Icon name="lock" size={13} />
              <span className="dw-sr">
                <Trans>Dockwell doesn't open this</Trans>
              </span>
            </span>
          ) : null}
        </span>
      ),
    };
    const size: TableColumn<Row> = {
      key: "size",
      header: t`Size`,
      width: tight ? 68 : 84,
      align: "right",
      muted: true,
      // What a link's own size says is how long its target is, which no one asks.
      render: (row) =>
        row.folder || row.entry.type === FileType.SYMLINK ? "—" : formatBytes(row.entry.sizeBytes, i18n.locale),
    };
    const actions: TableColumn<Row> = {
      key: "actions",
      header: (
        <span className="dw-sr">
          <Trans>Actions</Trans>
        </span>
      ),
      width: oneButton ? 44 : 140,
      align: "right",
      render: (row) =>
        !changeable(row) ? null : oneButton ? (
          <Button
            size="sm"
            variant="ghost"
            icon="more"
            aria-label={t`Actions for ${row.name}`}
            onClick={() => setSheet({ kind: "actions", entry: row.entry })}
          />
        ) : (
          <RowActions
            entry={row.entry}
            download={downloads(row) ? downloadFileURL({ serverId, path: join(shown, row.name) }) : undefined}
            onRename={() => setSheet({ kind: "name", naming: { kind: "rename", entry: row.entry } })}
            onMove={() => setSheet({ kind: "move", entry: row.entry })}
            onDelete={() => setSheet({ kind: "delete", entry: row.entry })}
          />
        ),
    };
    if (phone) return [name, size, actions];
    return [
      name,
      size,
      {
        key: "modified",
        header: t`Modified`,
        width: tight ? 78 : 92,
        muted: true,
        render: (row) => (row.entry.modifyTime ? words.ago(timestampDate(row.entry.modifyTime), now) : "—"),
      },
      {
        key: "mode",
        header: t`Permissions`,
        width: tight ? 100 : 116,
        mono: true,
        muted: true,
        render: (row) => row.entry.mode,
      },
      {
        key: "owner",
        header: t`Owner`,
        width: tight ? 80 : 96,
        mono: true,
        muted: true,
        render: (row) => row.entry.owner,
      },
      actions,
    ];
  }, [phone, oneButton, tight, t, i18n.locale, words, now, serverId, shown]);

  const go = (search: FilesSearch) => void navigate({ to: "/servers/$serverId/files", params: { serverId }, search });
  /** Where an open file went, as a move or a delete took it. */
  const away = (from: string, to?: string) => {
    if (file !== from) return;
    go(to ? { path: parentOf(to), file: to } : { path: shown });
  };
  const restore = async (d: Deleted) => {
    if (!d.kept) return;
    setRestoring(true);
    setRestoreFailure(undefined);
    try {
      await putBack(d.kept);
      setDeleted(undefined);
    } catch (err) {
      setRestoreFailure(restoreProblem(err, d.path));
    } finally {
      setRestoring(false);
    }
  };

  const count = listing?.entries.length ?? 0;
  const many = (n: number) => n.toLocaleString(i18n.locale);
  const countWords = many(count);
  const shownWords = many(rows.length);
  const owner = listing?.directory?.owner;
  return (
    <div className="files">
      {reading ? null : (
        <div className="files-bar">
          <div className="files-where">
            <nav aria-label={t`Path`} className="files-crumbs">
              {crumbs(shown).map((crumb, i, all) =>
                i === all.length - 1 ? (
                  <span key={crumb.path} aria-current="page" className="files-crumb-here">
                    {crumb.name}
                  </span>
                ) : (
                  <span key={crumb.path} className="files-crumb">
                    <RouterButtonLink
                      variant="ghost"
                      size="sm"
                      to="/servers/$serverId/files"
                      params={{ serverId }}
                      search={{ path: crumb.path }}
                    >
                      {crumb.name}
                    </RouterButtonLink>
                    {i > 0 ? <span aria-hidden="true">/</span> : null}
                  </span>
                ),
              )}
            </nav>
            <CopyButton text={shown} label={t`Copy path ${shown}`} />
            {listing ? (
              <span className="files-count">
                {big ? t`More than ${countWords} items` : plural(count, { one: "# item", other: "# items" })}
                {owner ? (
                  <>
                    {" · "}
                    <Trans>
                      owned by <code>{owner}</code>
                    </Trans>
                  </>
                ) : null}
              </span>
            ) : null}
          </div>
          <div className="files-tools">
            <div className="files-filter">
              <TextField
                size="sm"
                icon="search"
                placeholder={t`Search in ${nameOf(shown)}`}
                aria-label={t`Search in ${nameOf(shown)}`}
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
              />
            </div>
            <Button
              size="sm"
              icon="folder"
              disabled={!listing}
              onClick={() => setSheet({ kind: "name", naming: { kind: "folder" } })}
            >
              <Trans>New folder</Trans>
            </Button>
            <Button
              size="sm"
              icon="file"
              disabled={!listing}
              onClick={() => setSheet({ kind: "name", naming: { kind: "file" } })}
            >
              <Trans>New file</Trans>
            </Button>
            <Button size="sm" icon="upload" disabled={!listing} onClick={() => setSheet({ kind: "upload" })}>
              <Trans>Upload</Trans>
            </Button>
            <Button size="sm" variant="ghost" icon="trash" onClick={() => setSheet({ kind: "deleted" })}>
              <Trans>Recently deleted</Trans>
            </Button>
          </div>
        </div>
      )}
      {!reading && deleted ? (
        <Callout
          tone={restoreFailure ? "critical" : "info"}
          title={deleted.kept ? t`Deleted ${nameOf(deleted.path)}` : t`Deleted ${nameOf(deleted.path)} for good`}
          actions={
            <>
              {deleted.kept ? (
                <Button size="sm" icon="undo" loading={restoring} onClick={() => void restore(deleted)}>
                  <Trans context="what was deleted">Put back</Trans>
                </Button>
              ) : null}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => {
                  setDeleted(undefined);
                  setRestoreFailure(undefined);
                }}
              >
                <Trans>Dismiss</Trans>
              </Button>
            </>
          }
        >
          {restoreFailure ??
            (deleted.kept ? (
              <Trans>It's in Recently deleted for 7 days, where you can put it back.</Trans>
            ) : (
              <Trans>Nothing brings it back.</Trans>
            ))}
        </Callout>
      ) : null}
      <div className="files-panes">
        {reading ? null : (
          <Panel flush className="files-places">
            <nav aria-label={t`Places`}>
              {/* As Server-Files groups them: the places worth a click, then the server's own root. */}
              <PlaceGroup title={t`Common places`} places={places.common} current={current} serverId={serverId} />
              <div className="files-places-rule" aria-hidden="true" />
              {phone ? (
                <PlaceGroup title={t`Server`} places={places.server} current={current} serverId={serverId} />
              ) : (
                // As Server-Files has it: the server's folders as a tree, open down to where you are.
                // biome-ignore lint/a11y/useSemanticElements: a fieldset is for a form's fields, and these are links
                <div role="group" aria-labelledby={treeHead} className="files-places-group">
                  <div id={treeHead} className="files-places-head">
                    {t`Server`}
                  </div>
                  <FolderTree serverId={serverId} current={shown} retry={retryListing} />
                </div>
              )}
            </nav>
          </Panel>
        )}
        {reading ? null : (
          // biome-ignore lint/a11y/noStaticElementInteractions: a drop target; Upload does the same from the keyboard
          <div
            className="files-list"
            data-dropping={dragging || undefined}
            onDragOver={(event) => {
              if (!listing || !carriesFiles(event)) return;
              event.preventDefault();
              setDragging(true);
            }}
            onDragLeave={(event) => {
              if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setDragging(false);
            }}
            onDrop={(event) => {
              if (!listing || !carriesFiles(event)) return;
              event.preventDefault();
              setDragging(false);
              setSheet({ kind: "upload", chosen: chosenFrom(event.dataTransfer) });
            }}
          >
            {query.error ? (
              <FolderProblem error={query.error} folder={folder} serverId={serverId} />
            ) : (
              <>
                {listing?.truncated && !big ? (
                  <Callout tone="warning" title={t`This folder holds more than a listing shows`}>
                    <Trans>
                      Only {count} of its items are here. Update the agent on {server.name} to search and page through
                      them all.
                    </Trans>
                  </Callout>
                ) : null}
                <Table
                  columns={columns}
                  rows={rows}
                  rowKey="name"
                  density="compact"
                  maxHeight={phone ? undefined : 640}
                  selectedKey={file ? nameOf(file) : null}
                  caption={t`Files in ${shown}`}
                  onRowClick={open}
                  empty={
                    query.isPending ? (
                      <Trans>Reading this folder</Trans>
                    ) : searching && search.isPending ? (
                      <Trans>Searching every name in this folder</Trans>
                    ) : searching && search.error ? (
                      sentence(search.error)
                    ) : filter ? (
                      <Trans>Nothing here matches your search.</Trans>
                    ) : (
                      <Trans>This folder is empty.</Trans>
                    )
                  }
                />
                {nextPage || moreProblem ? (
                  <div className="files-more">
                    <span className="files-more-count">
                      {searching ? t`${shownWords} found so far, by name` : t`${shownWords} shown, by name`}
                    </span>
                    {moreProblem ? (
                      <span className="files-more-problem" role="alert">
                        {moreProblem}
                      </span>
                    ) : null}
                    {nextPage ? (
                      <Button size="sm" variant="secondary" loading={loadingMore} onClick={() => void showMore()}>
                        {t({ message: "Show more", context: "the next page of a long list" })}
                      </Button>
                    ) : null}
                  </div>
                ) : null}
              </>
            )}
          </div>
        )}
        {file ? (
          <FileViewer serverId={serverId} path={file} folder={shown} key={file} />
        ) : (
          // On a wide screen the viewer's place stays, so the list keeps the width it has with a file open and a
          // name doesn't sit far from its size. Below 1600px the stylesheet hides it, as the list needs the room.
          <div className="files-idle">
            <EmptyState icon="file" title={<Trans>No file open</Trans>}>
              <p>
                <Trans>Choose a file in the list to read or edit it here.</Trans>
              </p>
            </EmptyState>
          </div>
        )}
      </div>
      {sheet?.kind === "actions" ? (
        <ActionsSheet
          folder={shown}
          entry={sheet.entry}
          download={
            sheet.entry.type === FileType.FILE || sheet.entry.type === FileType.DIRECTORY
              ? downloadFileURL({ serverId, path: join(shown, sheet.entry.name) })
              : undefined
          }
          onClose={() => setSheet(undefined)}
          onRename={() => setSheet({ kind: "name", naming: { kind: "rename", entry: sheet.entry } })}
          onMove={() => setSheet({ kind: "move", entry: sheet.entry })}
          onDelete={() => setSheet({ kind: "delete", entry: sheet.entry })}
        />
      ) : null}
      {sheet?.kind === "name" ? (
        <NameSheet
          serverId={serverId}
          folder={shown}
          naming={sheet.naming}
          onClose={() => setSheet(undefined)}
          onDone={(path, kind) => {
            setSheet(undefined);
            if (sheet.naming.kind === "rename") away(join(shown, sheet.naming.entry.name), path);
            else if (kind === FileType.FILE) go({ path: shown, file: path });
          }}
        />
      ) : null}
      {sheet?.kind === "move" ? (
        <MoveSheet
          serverId={serverId}
          folder={shown}
          entry={sheet.entry}
          onClose={() => setSheet(undefined)}
          onDone={(to) => {
            setSheet(undefined);
            away(join(shown, sheet.entry.name), to);
          }}
        />
      ) : null}
      {sheet?.kind === "delete" ? (
        <DeleteSheet
          serverId={serverId}
          folder={shown}
          entry={sheet.entry}
          onClose={() => setSheet(undefined)}
          onDone={(path, kept) => {
            setSheet(undefined);
            setRestoreFailure(undefined);
            setDeleted({ path, kept });
            away(join(shown, sheet.entry.name));
          }}
        />
      ) : null}
      {sheet?.kind === "upload" ? (
        <UploadSheet serverId={serverId} folder={shown} chosen={sheet.chosen} onClose={() => setSheet(undefined)} />
      ) : null}
      {sheet?.kind === "deleted" ? (
        <DeletedSheet
          serverId={serverId}
          onClose={() => setSheet(undefined)}
          onRestored={(path) => {
            setSheet(undefined);
            setDeleted(undefined);
            go({ path: parentOf(path) });
          }}
        />
      ) : null}
    </div>
  );
}

/** Why a folder can't be shown, with the server's own words, and a way up. */
function FolderProblem({ error, folder, serverId }: { error: ConnectError; folder: string; serverId: string }) {
  const { t } = useLingui();
  const reason = reasonOf(error);
  const title = isRoleRefusal(error)
    ? t`Your role doesn't cover this`
    : reason === "file_refused"
      ? t`Dockwell doesn't open this folder`
      : reason === "not_found"
        ? t`Nothing at this path`
        : reason === "path_not_answering"
          ? t`This path isn't answering`
          : reason === "server_offline" || ConnectError.from(error).code === Code.Unavailable
            ? t`Can't reach this server`
            : t`Can't read this folder`;
  return (
    <Callout
      tone={reason === "file_refused" || reason === "not_found" || isRoleRefusal(error) ? "warning" : "critical"}
      title={title}
    >
      <p>{sentence(error)}</p>
      {folder === "/" ? null : (
        <p>
          <Link to="/servers/$serverId/files" params={{ serverId }} search={{ path: parentOf(folder) }}>
            <Trans>Go up to {parentOf(folder)}</Trans>
          </Link>
        </p>
      )}
    </Callout>
  );
}
