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
import { useMemo, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { useControlPlaneNow } from "../servers/useFleet";
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

/** Places on most servers worth a click, as the mock has them. */
function usePlaces(): { label: string; path: string; icon: IconName }[] {
  const { t } = useLingui();
  return [
    { label: t`Websites`, path: "/var/www", icon: "browser" },
    { label: t`Logs`, path: "/var/log", icon: "file" },
    { label: t`Docker volumes`, path: "/var/lib/docker/volumes", icon: "layers" },
    { label: t`Server root`, path: "/", icon: "server" },
  ];
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
      {(server) =>
        reading && file ? (
          <FileViewer serverId={serverId} path={file} folder={folder} key={file} />
        ) : (
          <Files server={server} folder={folder} file={file} />
        )
      }
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

function Files({ server, folder, file }: { server: Server; folder: string; file: string | undefined }) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const now = useControlPlaneNow();
  const words = useTimeWords();
  const places = usePlaces();
  const [filter, setFilter] = useState("");
  const serverId = server.id;
  const query = useQuery(FileService.method.listFiles, { serverId, path: folder });
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
      <div className="files-panes">
        <Panel flush className="files-places">
          <nav aria-label={t`Places`}>
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
              >
                {place.label}
              </RouterButtonLink>
            ))}
          </nav>
        </Panel>
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
        {file ? <FileViewer serverId={serverId} path={file} folder={shown} key={file} /> : null}
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
