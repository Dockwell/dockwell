// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType, type Server } from "@dockwell/api";
import {
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
import { useId, useMemo, useState } from "react";

import { isTransient } from "../../app/api";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { useControlPlaneNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { FileViewer } from "./FileViewer";
import { crumbs, join, nameOf, parentOf } from "./paths";
import "./FilesPage.css";

/** Where you are on a server, from the URL. */
export interface FilesSearch {
  /** The folder on show. */
  path?: string;
  /** The file open beside it. */
  file?: string;
}

/** A path that isn't answering won't by the next try, and each try costs someone the wait. */
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
function rowsOf(entries: readonly FileEntry[]): Row[] {
  return entries
    .map((entry) => ({ name: entry.name, entry, folder: opensAsFolder(entry) }))
    .sort((a, b) => Number(b.folder) - Number(a.folder) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
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
  const current = placeOf(folder, [...places.common, ...places.server]);
  const [filter, setFilter] = useState("");
  const serverId = server.id;
  // A refused listing is on record, so it isn't asked again each time the window takes focus.
  const query = useQuery(
    FileService.method.listFiles,
    { serverId, path: folder },
    { retry: retryListing, refetchOnWindowFocus: (q) => q.state.status !== "error" },
  );
  const listing = query.data;
  const shown = listing?.path ?? folder;

  const rows = useMemo(() => {
    const wanted = filter.trim().toLowerCase();
    const all = rowsOf(listing?.entries ?? []);
    return wanted ? all.filter((row) => row.name.toLowerCase().includes(wanted)) : all;
  }, [listing, filter]);

  const open = (row: Row) => {
    if (row.entry.unreadableName) return;
    const path = join(shown, row.name);
    void navigate({
      to: "/servers/$serverId/files",
      params: { serverId },
      search: row.folder ? { path } : { path: shown, file: path },
    });
  };

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
      width: 84,
      align: "right",
      muted: true,
      // What a link's own size says is how long its target is, which no one asks.
      render: (row) =>
        row.folder || row.entry.type === FileType.SYMLINK ? "—" : formatBytes(row.entry.sizeBytes, i18n.locale),
    };
    if (phone) return [name, size];
    return [
      name,
      size,
      {
        key: "modified",
        header: t`Modified`,
        width: 92,
        muted: true,
        render: (row) => (row.entry.modifyTime ? words.ago(timestampDate(row.entry.modifyTime), now) : "—"),
      },
      {
        key: "mode",
        header: t`Permissions`,
        width: 116,
        mono: true,
        muted: true,
        render: (row) => row.entry.mode,
      },
      { key: "owner", header: t`Owner`, width: 96, mono: true, muted: true, render: (row) => row.entry.owner },
    ];
  }, [phone, t, i18n.locale, words, now]);

  const count = listing?.entries.length ?? 0;
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
                {plural(count, { one: "# item", other: "# items" })}
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
        </div>
      )}
      <div className="files-panes">
        {reading ? null : (
          <Panel flush className="files-places">
            <nav aria-label={t`Places`}>
              {/* As Server-Files groups them: the places worth a click, then the server's own root. */}
              <PlaceGroup title={t`Common places`} places={places.common} current={current} serverId={serverId} />
              <div className="files-places-rule" aria-hidden="true" />
              <PlaceGroup title={t`Server`} places={places.server} current={current} serverId={serverId} />
            </nav>
          </Panel>
        )}
        {reading ? null : (
          <div className="files-list">
            {query.error ? (
              <FolderProblem error={query.error} folder={folder} serverId={serverId} />
            ) : (
              <>
                {listing?.truncated ? (
                  <Callout tone="warning" title={t`This folder holds more than a listing shows`}>
                    <Trans>
                      Only {count} of its items are here, so one you're after may be missing. Connect to the server over
                      SSH to see them all.
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
                    ) : filter ? (
                      <Trans>Nothing here matches your search.</Trans>
                    ) : (
                      <Trans>This folder is empty.</Trans>
                    )
                  }
                />
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
    </div>
  );
}

/** Why a folder can't be shown, with the server's own words, and a way up. */
function FolderProblem({ error, folder, serverId }: { error: ConnectError; folder: string; serverId: string }) {
  const { t } = useLingui();
  const reason = reasonOf(error);
  const title =
    reason === "file_refused"
      ? t`Dockwell doesn't open this folder`
      : reason === "not_found"
        ? t`Nothing at this path`
        : reason === "path_not_answering"
          ? t`This path isn't answering`
          : reason === "server_offline" || ConnectError.from(error).code === Code.Unavailable
            ? t`Can't reach this server`
            : t`Can't read this folder`;
  return (
    <Callout tone={reason === "file_refused" || reason === "not_found" ? "warning" : "critical"} title={title}>
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
