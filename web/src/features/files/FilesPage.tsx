// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { skipToken, useQuery } from "@connectrpc/connect-query";
import {
  type ArchiveJob,
  ArchiveJobKind,
  ArchiveJobState,
  downloadFileURL,
  type FileEntry,
  FileService,
  FileType,
  type ListFilesResponse,
  type ReadFileResponse,
  type Server,
  StepUpAction,
} from "@dockwell/api";
import {
  Button,
  ButtonLink,
  Callout,
  ContextMenu,
  type ContextMenuItem,
  Icon,
  type IconName,
  Panel,
  PHONE_QUERY,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type KeyboardEvent, type PointerEvent, useCallback, useEffect, useId, useMemo, useRef, useState } from "react";

import { isTransient } from "../../app/api";
import { isRoleRefusal, reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import { CopyButton } from "../servers/CopyButton";
import { ServerFrame } from "../servers/ServerFrame";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { FileDetailsPane } from "./FileDetailsPane";
import { type FileRow, FileTable, type MenuTrigger, type SortDir, type SortField } from "./FileTable";
import { FileViewer } from "./FileViewer";
import { FolderTree, type TreeFolderTarget } from "./FolderTree";
import {
  CompressSheet,
  type DeletedResult,
  DeletedSheet,
  DeleteSheet,
  type FileTarget,
  MoveSheet,
  NameSheet,
  type Naming,
  targetOf,
  useExtract,
  useFiles,
  useNameProblem,
  useRestore,
  useRestoreProblem,
} from "./manage";
import { crumbs, join, nameOf, parentOf } from "./paths";
import {
  type ColumnWidths,
  DEFAULT_TREE_WIDTH,
  imageMimeOf,
  MAX_TREE_WIDTH,
  MIN_TREE_WIDTH,
  type ResizableColumn,
  readColumnWidths,
  readTreeWidth,
  writeColumnWidths,
  writeTreeWidth,
} from "./preferences";
import { archiveKind } from "./transfer";
import { type Chosen, carriesFiles, chosenFrom, UploadSheet } from "./upload";
import "./FilesPage.css";

/** Where you are on a server, from the URL. */
export interface FilesSearch {
  /** The folder on show. */
  path?: string;
  /** The file open beside it. */
  file?: string;
  /** The line of it to bring into view, from 1. */
  line?: number;
}

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

function placeOf(folder: string, places: readonly Place[]): string | undefined {
  let best: Place | undefined;
  for (const place of places) {
    const holds = place.path === "/" || folder === place.path || folder.startsWith(`${place.path}/`);
    if (holds && (!best || place.path.length > best.path.length)) best = place;
  }
  return best?.path;
}

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
      {(server) => <Files server={server} folder={folder} file={file} line={search.line} reading={reading} />}
    </ServerFrame>
  );
}

function opensAsFolder(entry: FileEntry): boolean {
  return (
    entry.type === FileType.DIRECTORY || (entry.type === FileType.SYMLINK && entry.linkType === FileType.DIRECTORY)
  );
}

function compareRows(a: FileRow, b: FileRow, field: SortField, dir: SortDir, byNameAlone: boolean): number {
  if (byNameAlone) return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
  // Keep folders grouped ahead of files so browsing feels familiar.
  const folderDiff = Number(b.folder) - Number(a.folder);
  if (folderDiff !== 0) return folderDiff;
  let cmp = 0;
  switch (field) {
    case "name":
      cmp = a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
      break;
    case "size":
      cmp = a.entry.sizeBytes < b.entry.sizeBytes ? -1 : a.entry.sizeBytes > b.entry.sizeBytes ? 1 : 0;
      break;
    case "modified": {
      const ta = a.entry.modifyTime ? timestampDate(a.entry.modifyTime).getTime() : 0;
      const tb = b.entry.modifyTime ? timestampDate(b.entry.modifyTime).getTime() : 0;
      cmp = ta - tb;
      break;
    }
    case "mode":
      cmp = a.entry.mode.localeCompare(b.entry.mode);
      break;
    case "owner":
      cmp = a.entry.owner.localeCompare(b.entry.owner);
      break;
  }
  if (cmp === 0) cmp = a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
  return dir === "asc" ? cmp : -cmp;
}

function changeableEntry(entry: FileEntry): boolean {
  return !entry.refused && !entry.unreadableName;
}

function downloadableEntry(entry: FileEntry): boolean {
  return changeableEntry(entry) && (entry.type === FileType.FILE || entry.type === FileType.DIRECTORY);
}

/** What sheet is open over the listing, if anything. */
type OpenSheet =
  | { kind: "name"; folder: string; naming: Naming }
  | { kind: "move"; folder: string; targets: readonly FileTarget[] }
  | { kind: "compress"; folder: string; targets: readonly FileTarget[] }
  | { kind: "delete"; targets: readonly FileTarget[] }
  | { kind: "deleted" }
  | { kind: "upload"; folder: string; chosen?: Chosen };

/** Active floating context menu state. */
type OpenMenu =
  | { kind: "row"; row: FileRow; trigger: MenuTrigger }
  | { kind: "empty"; position: { x: number; y: number } }
  | { kind: "tree"; target: TreeFolderTarget; position: { x: number; y: number } };

interface BannerNotice {
  tone: "info" | "warning" | "critical";
  title: string;
  body: string;
  action?: {
    label: string;
    icon?: IconName;
    onClick: () => void;
  };
}

/** Client-side cap on multi-path GET download links before offering Compress instead (L1). */
export const MAX_MULTI_DOWNLOAD_COUNT = 1000;
export const MAX_MULTI_DOWNLOAD_URL_LENGTH = 8000;

interface FilesProps {
  server: Server;
  folder: string;
  file: string | undefined;
  line: number | undefined;
  reading: boolean;
}

const BESIDE_FOLDERS = "(min-width: 860px)";

function copyToClipboard(text: string): void {
  void navigator.clipboard?.writeText(text).catch(() => {});
}

function Files({ server, folder, file, line, reading }: FilesProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const besideFolders = useMediaQuery(BESIDE_FOLDERS);
  const navigate = useNavigate();
  const viewerId = useSignedInUser()?.id ?? "";
  const places = usePlaces();
  const treeHead = useId();
  const current = placeOf(folder, [...places.common, ...places.server]);

  const [filter, setFilter] = useState("");
  const [sortField, setSortField] = useState<SortField>("name");
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const [treeWidth, setTreeWidth] = useState<number>(() => readTreeWidth(viewerId));
  const [columnWidths, setColumnWidths] = useState<ColumnWidths>(() => readColumnWidths(viewerId));
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => new Set());
  const [anchor, setAnchor] = useState<string>();
  const [sheet, setSheet] = useState<OpenSheet>();
  const [menu, setMenu] = useState<OpenMenu>();
  const [deleted, setDeleted] = useState<readonly DeletedResult[]>();
  const [restoring, setRestoring] = useState(false);
  const [restoreFailure, setRestoreFailure] = useState<string>();
  const [notice, setNotice] = useState<BannerNotice>();
  const [archiveJobs, setArchiveJobs] = useState<readonly ArchiveJob[]>([]);
  const [archiveReconnecting, setArchiveReconnecting] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [previewFullWindow, setPreviewFullWindow] = useState(false);
  const [editorFullWindow, setEditorFullWindow] = useState(false);
  const [openTabs, setOpenTabs] = useState<string[]>(() => (file ? [file] : []));
  const [renaming, setRenaming] = useState<string>();
  const [renameError, setRenameError] = useState<string>();
  const [renameBusy, setRenameBusy] = useState(false);

  // Sync URL active file into the open tabs list.
  useEffect(() => {
    if (!file) {
      setEditorFullWindow(false);
      return;
    }
    setOpenTabs((was) => (was.includes(file) ? was : [...was, file]));
  }, [file]);

  // Cache of previewed text reads so opening the editor right after preview doesn't double-read.
  const previewReads = useRef(new Map<string, ReadFileResponse>());
  const onPreviewRead = useCallback((path: string, response: ReadFileResponse) => {
    previewReads.current.set(path, response);
  }, []);
  const onConsumedInitialRead = useCallback((path: string) => {
    previewReads.current.delete(path);
  }, []);

  useEffect(() => {
    setTreeWidth(readTreeWidth(viewerId));
    setColumnWidths(readColumnWidths(viewerId));
  }, [viewerId]);

  const serverId = server.id;
  const { transport, client, changed, problem } = useFiles(serverId);
  const nameProblem = useNameProblem();
  const putBack = useRestore(serverId);
  const restoreProblem = useRestoreProblem(serverId);
  const { extract } = useExtract(serverId);

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

  const shownRef = useRef(shown);
  shownRef.current = shown;

  const presentArchiveOutcome = useCallback(
    (job: ArchiveJob) => {
      if (job.state === ArchiveJobState.DONE) {
        if (job.kind === ArchiveJobKind.COMPRESS) {
          const archiveName = nameOf(job.path);
          if (parentOf(job.path) === shownRef.current) {
            setSelected(new Set([archiveName]));
            setAnchor(archiveName);
          }
          const packed = plural(Number(job.entries), {
            one: "Packed # item into the archive.",
            other: "Packed # items into the archive.",
          });
          const leftOutNote =
            job.leftOut > 0n
              ? ` ${plural(Number(job.leftOut), {
                  one: "# item was left out.",
                  other: "# items were left out.",
                })}`
              : "";
          setNotice({
            tone: "info",
            title: t`Compressed ${archiveName}`,
            body: `${packed}${leftOutNote}`,
          });
        } else {
          const into = nameOf(job.unpackedTo || job.path);
          const unpacked = plural(Number(job.entries), {
            one: `Unpacked # item into ${into}.`,
            other: `Unpacked # items into ${into}.`,
          });
          const skippedNote =
            job.skippedLinks > 0n
              ? ` ${plural(Number(job.skippedLinks), {
                  one: "# link wasn't made.",
                  other: "# links weren't made.",
                })}`
              : "";
          setNotice({
            tone: "info",
            title: t`Extracted ${nameOf(job.path)}`,
            body: `${unpacked}${skippedNote}`,
          });
        }
      } else if (job.state === ArchiveJobState.FAILED) {
        const msg = job.error ? sentence(new Error(job.error)) : t`Something went wrong on the server.`;
        setNotice({
          tone: "critical",
          title:
            job.kind === ArchiveJobKind.COMPRESS
              ? t`Couldn't compress ${nameOf(job.path)}`
              : t`Couldn't extract ${nameOf(job.path)}`,
          body: msg,
        });
      }
    },
    [t],
  );

  const trackArchiveJob = useCallback(
    (job: ArchiveJob) => {
      if (job.state === ArchiveJobState.RUNNING) {
        setArchiveJobs((was) => {
          const idx = was.findIndex((j) => j.jobId === job.jobId);
          if (idx >= 0) {
            const next = [...was];
            next[idx] = job;
            return next;
          }
          return [...was, job];
        });
        return;
      }
      setArchiveJobs((was) => was.filter((j) => j.jobId !== job.jobId));
      void changed();
      presentArchiveOutcome(job);
    },
    [changed, presentArchiveOutcome],
  );

  // Restore any running archive jobs on this server after a reload or reconnect.
  useEffect(() => {
    const abort = new AbortController();
    setArchiveJobs([]);
    setArchiveReconnecting(false);
    void (async () => {
      try {
        const res = await client.listArchiveJobs({ serverId }, { signal: abort.signal });
        if (abort.signal.aborted) return;
        const running = res.jobs.filter((j) => j.state === ArchiveJobState.RUNNING);
        if (running.length > 0) {
          setArchiveJobs((was) => {
            const byId = new Map(was.map((j) => [j.jobId, j]));
            for (const job of running) byId.set(job.jobId, job);
            return [...byId.values()];
          });
        }
      } catch {
        // Older agents or offline servers simply have no background jobs to restore.
      }
    })();
    return () => abort.abort();
  }, [client, serverId]);

  const archiveJobsRef = useRef(archiveJobs);
  archiveJobsRef.current = archiveJobs;

  const runningJobIds = useMemo(
    () =>
      archiveJobs
        .filter((j) => j.state === ArchiveJobState.RUNNING)
        .map((j) => j.jobId)
        .join(","),
    [archiveJobs],
  );

  // Poll running archive jobs one pass at a time; transient proxy cuts or 504s never abort tracking or falsely claim failure.
  useEffect(() => {
    if (!runningJobIds) {
      setArchiveReconnecting(false);
      return;
    }
    const ids = runningJobIds.split(",");
    let cancelled = false;
    let consecutiveErrors = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      let sawError = false;
      for (const jobId of ids) {
        if (cancelled) return;
        try {
          const res = await client.getArchiveJob({ serverId, jobId });
          if (cancelled || !res.job) continue;
          consecutiveErrors = 0;
          setArchiveReconnecting(false);
          trackArchiveJob(res.job);
        } catch (err) {
          if (cancelled) return;
          if (reasonOf(err) === "not_found" || ConnectError.from(err).code === Code.NotFound) {
            const lost = archiveJobsRef.current.find((j) => j.jobId === jobId);
            setArchiveJobs((was) => was.filter((j) => j.jobId !== jobId));
            void changed();
            if (lost) {
              const label = nameOf(lost.path);
              setNotice({
                tone: "warning",
                title: t`Stopped when the agent restarted`,
                body:
                  lost.kind === ArchiveJobKind.COMPRESS
                    ? t`Compressing ${label} stopped before it finished. Start it again.`
                    : t`Extracting ${label} stopped before it finished. Part of it may already be in ${nameOf(lost.unpackedTo || lost.path)}.`,
              });
            }
          } else {
            sawError = true;
          }
        }
      }
      if (sawError && !cancelled) {
        consecutiveErrors++;
        if (consecutiveErrors >= 2) setArchiveReconnecting(true);
      }
    };
    const schedule = () => {
      if (cancelled) return;
      timer = setTimeout(() => {
        void poll().finally(schedule);
      }, 500);
    };
    schedule();
    return () => {
      cancelled = true;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [client, serverId, runningJobIds, trackArchiveJob, changed, t]);

  // Clear selection, inline rename, and preview expansion when changing folders.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset per-folder transient state whenever the folder changes
  useEffect(() => {
    setSelected(new Set());
    setAnchor(undefined);
    setRenaming(undefined);
    setRenameError(undefined);
    setPreviewFullWindow(false);
  }, [shown]);

  const big = Boolean(listing?.nextPage);
  const wanted = useSettled(filter.trim(), 300);
  const searching = big && wanted !== "";
  const search = useQuery(
    FileService.method.listFiles,
    searching ? { serverId, path: folder, match: wanted } : skipToken,
    { retry: retryListing, refetchOnWindowFocus: false },
  );
  const first = searching ? search.data : listing;
  const pagesOf = `${folder}\0${searching ? wanted : ""}\0${first?.nextPage ?? ""}`;
  const [after, setAfter] = useState<{ of?: string; pages: ListFilesResponse[]; problem?: string }>({ pages: [] });
  const more = after.of === pagesOf ? after.pages : [];
  const moreProblem = after.of === pagesOf ? after.problem : undefined;
  const [loadingMore, setLoadingMore] = useState(false);
  const nextPage = (more.at(-1) ?? first)?.nextPage ?? "";
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

  const rows = useMemo<FileRow[]>(() => {
    const typed = filter.trim().toLowerCase();
    const all: FileRow[] = [first, ...more]
      .flatMap((page) => page?.entries ?? [])
      .map((entry) => ({ name: entry.name, entry, folder: opensAsFolder(entry) }))
      .sort((a, b) => compareRows(a, b, sortField, sortDir, big));
    return typed ? all.filter((row) => row.name.toLowerCase().includes(typed)) : all;
  }, [first, more, filter, sortField, sortDir, big]);

  const selectedRows = useMemo(() => rows.filter((r) => selected.has(r.name)), [rows, selected]);
  const singleSelected = selectedRows.length === 1 ? selectedRows[0] : undefined;

  const go = (next: FilesSearch) =>
    void navigate({ to: "/servers/$serverId/files", params: { serverId }, search: next });

  const away = (from: string, to?: string) => {
    setOpenTabs((was) => {
      if (!was.includes(from)) return was;
      return to ? was.map((p) => (p === from ? to : p)) : was.filter((p) => p !== from);
    });
    if (file !== from) return;
    if (to) {
      go({ path: parentOf(to), file: to });
    } else {
      const remaining = openTabs.filter((p) => p !== from);
      go(remaining.length > 0 ? { path: shown, file: remaining[remaining.length - 1] } : { path: shown });
    }
  };

  const closeTab = (tabPath: string) => {
    const remaining = openTabs.filter((p) => p !== tabPath);
    setOpenTabs(remaining);
    previewReads.current.delete(tabPath);
    if (tabPath === file) {
      const idx = openTabs.indexOf(tabPath);
      const next = remaining[Math.min(idx, remaining.length - 1)];
      if (!next) setEditorFullWindow(false);
      go(next ? { path: shown, file: next } : { path: shown });
    }
  };

  const openRow = (row: FileRow) => {
    if (row.entry.unreadableName) return;
    const path = join(shown, row.name);
    if (row.folder) {
      go({ path });
      return;
    }
    // Image files open in the preview pane (and full-window if already selected in preview on desktop).
    if (!phone && imageMimeOf(row.name) && row.entry.type === FileType.FILE && !row.entry.refused) {
      setSelected(new Set([row.name]));
      setAnchor(row.name);
      if (file) go({ path: shown });
      else setPreviewFullWindow(true);
      return;
    }
    go({ path: shown, file: path });
  };

  const handleSort = (field: SortField) => {
    if (big) return;
    if (sortField === field) {
      setSortDir((was) => (was === "asc" ? "desc" : "asc"));
    } else {
      setSortField(field);
      setSortDir("asc");
    }
  };

  const handleColumnWidthChange = (column: ResizableColumn, width: number) => {
    setColumnWidths((was) => {
      const next = { ...was, [column]: width };
      writeColumnWidths(viewerId, next);
      return next;
    });
  };

  const startTreeResize = (event: PointerEvent<HTMLDivElement>) => {
    event.preventDefault();
    const startX = event.clientX;
    const startW = treeWidth;
    const handle = event.currentTarget;
    handle.setPointerCapture?.(event.pointerId);
    const onMove = (moveEvent: globalThis.PointerEvent) => {
      const next = Math.max(
        MIN_TREE_WIDTH,
        Math.min(MAX_TREE_WIDTH, Math.round(startW + (moveEvent.clientX - startX))),
      );
      setTreeWidth(next);
      writeTreeWidth(viewerId, next);
    };
    const onUp = () => {
      handle.releasePointerCapture?.(event.pointerId);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
  };

  const onTreeSplitterKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    let next: number | undefined;
    if (event.key === "ArrowLeft") next = Math.max(MIN_TREE_WIDTH, treeWidth - 16);
    else if (event.key === "ArrowRight") next = Math.min(MAX_TREE_WIDTH, treeWidth + 16);
    else if (event.key === "Home") next = MIN_TREE_WIDTH;
    else if (event.key === "End") next = MAX_TREE_WIDTH;
    if (next !== undefined) {
      event.preventDefault();
      setTreeWidth(next);
      writeTreeWidth(viewerId, next);
    }
  };

  const commitInlineRename = async (row: FileRow, nextName: string) => {
    const wrong = nameProblem(nextName);
    if (wrong) {
      setRenameError(wrong);
      return;
    }
    if (nextName === row.name) {
      setRenaming(undefined);
      setRenameError(undefined);
      return;
    }
    const from = join(shown, row.name);
    const to = join(shown, nextName);
    setRenameBusy(true);
    setRenameError(undefined);
    try {
      const moved = await withStepUpIfAsked(
        transport,
        StepUpAction.MOVE_WAY_BACK_IN,
        `${serverId}:${from}\n${to}`,
        (proof) => client.moveFile({ serverId, from, to, proof }),
      );
      await changed();
      setRenaming(undefined);
      setRenameError(undefined);
      setSelected(new Set([nextName]));
      setAnchor(nextName);
      away(from, moved.to);
    } catch (err) {
      if (reasonOf(err) === "file_exists") {
        setRenameError(t`Something called ${nextName} is here already.`);
      } else {
        setRenameError(problem(err, to));
      }
    } finally {
      setRenameBusy(false);
    }
  };

  const runExtract = async (archivePath: string) => {
    setNotice(undefined);
    try {
      const job = await extract(archivePath);
      trackArchiveJob(job);
    } catch (err) {
      setNotice({
        tone: "critical",
        title: t`Couldn't extract ${nameOf(archivePath)}`,
        body: problem(err, archivePath),
      });
    }
  };

  const restoreSingle = async (d: DeletedResult) => {
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

  // Multi-selection actions
  const multiDownloadPaths = useMemo(
    () => selectedRows.filter((r) => downloadableEntry(r.entry)).map((r) => join(shown, r.name)),
    [selectedRows, shown],
  );
  const multiDownloadUrl = useMemo(
    () => (multiDownloadPaths.length > 0 ? downloadFileURL({ serverId, paths: multiDownloadPaths }) : ""),
    [serverId, multiDownloadPaths],
  );
  const multiDownloadTooLong =
    multiDownloadPaths.length > MAX_MULTI_DOWNLOAD_COUNT || multiDownloadUrl.length > MAX_MULTI_DOWNLOAD_URL_LENGTH;
  const multiChangeableTargets = useMemo(
    () => selectedRows.filter((r) => changeableEntry(r.entry)).map((r) => targetOf(shown, r.entry)),
    [selectedRows, shown],
  );
  const multiCompressPaths = useMemo(
    () => selectedRows.filter((r) => downloadableEntry(r.entry)).map((r) => targetOf(shown, r.entry)),
    [selectedRows, shown],
  );

  const warnTooManyToDownload = () => {
    setNotice({
      tone: "warning",
      title: t`Too many items to download at once`,
      body: t`Selecting this many items makes the download link too long. Compress them into a .zip on the server, then download that archive.`,
      action:
        multiCompressPaths.length > 0
          ? {
              label: t`Compress`,
              icon: "archive",
              onClick: () => {
                setNotice(undefined);
                setSheet({ kind: "compress", folder: shown, targets: multiCompressPaths });
              },
            }
          : undefined,
    });
  };

  // Build context menu items for the active menu target.
  const menuItems: ContextMenuItem[] = (() => {
    if (!menu) return [];
    if (menu.kind === "empty") {
      return [
        {
          id: "new-folder",
          label: t`New folder`,
          icon: "folder",
          disabled: !listing,
          onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "folder" } }),
        },
        {
          id: "new-file",
          label: t`New file`,
          icon: "file",
          disabled: !listing,
          onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "file" } }),
        },
        {
          id: "upload",
          label: t`Upload`,
          icon: "upload",
          disabled: !listing,
          onSelect: () => setSheet({ kind: "upload", folder: shown }),
        },
        { kind: "separator", id: "sep-1" },
        {
          id: "copy-path",
          label: t`Copy path`,
          icon: "copy",
          onSelect: () => copyToClipboard(shown),
        },
        {
          id: "refresh",
          label: t`Refresh`,
          icon: "refresh",
          onSelect: () => void query.refetch(),
        },
      ];
    }
    if (menu.kind === "tree") {
      const target = menu.target;
      const isRoot = target.path === "/";
      const items: ContextMenuItem[] = [
        {
          id: "open",
          label: t`Open`,
          icon: "folder",
          onSelect: () => go({ path: target.path }),
        },
      ];
      if (!target.refused) {
        items.push({
          id: "download",
          label: t`Download as a .zip`,
          icon: "download",
          href: downloadFileURL({ serverId, path: target.path }),
          download: true,
        });
      }
      items.push(
        { kind: "separator", id: "sep-tree-1" },
        {
          id: "new-folder",
          label: t`New folder`,
          icon: "folder",
          disabled: target.refused,
          onSelect: () => setSheet({ kind: "name", folder: target.path, naming: { kind: "folder" } }),
        },
        {
          id: "new-file",
          label: t`New file`,
          icon: "file",
          disabled: target.refused,
          onSelect: () => setSheet({ kind: "name", folder: target.path, naming: { kind: "file" } }),
        },
        {
          id: "copy-path",
          label: t`Copy path`,
          icon: "copy",
          onSelect: () => copyToClipboard(target.path),
        },
      );
      if (!isRoot && !target.refused) {
        const folderTarget: FileTarget = { path: target.path, name: target.name, type: FileType.DIRECTORY };
        items.push(
          { kind: "separator", id: "sep-tree-2" },
          {
            id: "rename",
            label: t`Rename`,
            icon: "edit",
            onSelect: () =>
              setSheet({
                kind: "name",
                folder: parentOf(target.path),
                naming: { kind: "rename", target: folderTarget },
              }),
          },
          {
            id: "move",
            label: t`Move`,
            icon: "arrow-right",
            onSelect: () => setSheet({ kind: "move", folder: parentOf(target.path), targets: [folderTarget] }),
          },
          {
            id: "compress",
            label: t`Compress`,
            icon: "archive",
            onSelect: () => setSheet({ kind: "compress", folder: parentOf(target.path), targets: [folderTarget] }),
          },
          { kind: "separator", id: "sep-tree-3" },
          {
            id: "delete",
            label: t`Delete`,
            icon: "trash",
            danger: true,
            onSelect: () => setSheet({ kind: "delete", targets: [folderTarget] }),
          },
        );
      }
      return items;
    }

    // Row context menu (applies to multi-selection if the clicked row is part of a multi-selection).
    const row = menu.row;
    const isMulti = selected.has(row.name) && selectedRows.length > 1;
    if (isMulti) {
      const count = selectedRows.length;
      const items: ContextMenuItem[] = [];
      if (multiDownloadPaths.length > 0) {
        items.push(
          multiDownloadTooLong
            ? {
                id: "download",
                label: t`Download ${count} items as a .zip`,
                icon: "download",
                onSelect: warnTooManyToDownload,
              }
            : {
                id: "download",
                label: t`Download ${count} items as a .zip`,
                icon: "download",
                href: multiDownloadUrl,
                download: true,
              },
        );
      }
      if (multiCompressPaths.length > 0) {
        items.push({
          id: "compress",
          label: t`Compress`,
          icon: "archive",
          onSelect: () => setSheet({ kind: "compress", folder: shown, targets: multiCompressPaths }),
        });
      }
      if (multiChangeableTargets.length > 0) {
        items.push({
          id: "move",
          label: t`Move`,
          icon: "arrow-right",
          onSelect: () => setSheet({ kind: "move", folder: shown, targets: multiChangeableTargets }),
        });
      }
      items.push({
        id: "copy-paths",
        label: t`Copy paths`,
        icon: "copy",
        onSelect: () => copyToClipboard(selectedRows.map((r) => join(shown, r.name)).join("\n")),
      });
      if (multiChangeableTargets.length > 0) {
        items.push(
          { kind: "separator", id: "sep-multi-del" },
          {
            id: "delete",
            label: t`Delete`,
            icon: "trash",
            danger: true,
            onSelect: () => setSheet({ kind: "delete", targets: multiChangeableTargets }),
          },
        );
      }
      return items;
    }

    const path = join(shown, row.name);
    const canChange = changeableEntry(row.entry);
    const canDownload = downloadableEntry(row.entry);
    const isArchive = canChange && row.entry.type === FileType.FILE && archiveKind(row.name) !== undefined;
    const items: ContextMenuItem[] = [
      {
        id: "open",
        label: t`Open`,
        icon: row.folder ? "folder" : "file",
        disabled: row.entry.unreadableName,
        onSelect: () => openRow(row),
      },
    ];
    if (canDownload) {
      items.push({
        id: "download",
        label: row.entry.type === FileType.DIRECTORY ? t`Download as a .zip` : t`Download`,
        icon: "download",
        href: downloadFileURL({ serverId, path }),
        download: true,
      });
    }
    if (canChange) {
      const target = targetOf(shown, row.entry);
      items.push(
        { kind: "separator", id: "sep-row-1" },
        {
          id: "rename",
          label: t`Rename`,
          icon: "edit",
          shortcut: phone ? undefined : "F2",
          onSelect: () => {
            setRenaming(row.name);
            setRenameError(undefined);
          },
        },
        {
          id: "move",
          label: t`Move`,
          icon: "arrow-right",
          onSelect: () => setSheet({ kind: "move", folder: shown, targets: [target] }),
        },
        {
          id: "copy-path",
          label: t`Copy path`,
          icon: "copy",
          onSelect: () => copyToClipboard(path),
        },
      );
      if (isArchive) {
        items.push({
          id: "extract",
          label: t`Extract`,
          icon: "archive",
          onSelect: () => void runExtract(path),
        });
      } else if (canDownload) {
        items.push({
          id: "compress",
          label: t`Compress`,
          icon: "archive",
          onSelect: () => setSheet({ kind: "compress", folder: shown, targets: [target] }),
        });
      }
      items.push(
        { kind: "separator", id: "sep-row-2" },
        {
          id: "delete",
          label: t`Delete`,
          icon: "trash",
          danger: true,
          onSelect: () => setSheet({ kind: "delete", targets: [target] }),
        },
      );
    } else {
      items.push({
        id: "copy-path",
        label: t`Copy path`,
        icon: "copy",
        onSelect: () => copyToClipboard(path),
      });
    }
    return items;
  })();

  const newMenuActions = useMemo<MenuAction[]>(
    () => [
      {
        id: "new-folder",
        label: t`New folder`,
        icon: "folder",
        disabled: !listing,
        onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "folder" } }),
      },
      {
        id: "new-file",
        label: t`New file`,
        icon: "file",
        disabled: !listing,
        onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "file" } }),
      },
    ],
    [listing, shown, t],
  );

  const phoneFolderActions = useMemo<MenuAction[]>(
    () => [
      {
        id: "upload",
        label: t`Upload`,
        icon: "upload",
        disabled: !listing,
        onSelect: () => setSheet({ kind: "upload", folder: shown }),
      },
      {
        id: "new-folder",
        label: t`New folder`,
        icon: "folder",
        disabled: !listing,
        onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "folder" } }),
      },
      {
        id: "new-file",
        label: t`New file`,
        icon: "file",
        disabled: !listing,
        onSelect: () => setSheet({ kind: "name", folder: shown, naming: { kind: "file" } }),
      },
      {
        id: "deleted",
        label: t`Recently deleted`,
        icon: "trash",
        onSelect: () => setSheet({ kind: "deleted" }),
      },
    ],
    [listing, shown, t],
  );

  const count = listing?.entries.length ?? 0;
  const many = (n: number) => n.toLocaleString(i18n.locale);
  const countWords = many(count);
  const shownWords = many(rows.length);
  const owner = listing?.directory?.owner;
  const hasRightPane = file !== undefined || (!phone && singleSelected !== undefined);
  const singleDeleted = deleted?.length === 1 ? deleted[0] : undefined;

  return (
    <div className="files">
      {reading ? null : (
        <div className="files-header">
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
          <div className="files-toolbar">
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
            {phone ? (
              <ActionMenu label={t`Folder actions`} actions={phoneFolderActions} />
            ) : (
              <div className="files-tools">
                <Button
                  size="sm"
                  variant="primary"
                  icon="upload"
                  disabled={!listing}
                  onClick={() => setSheet({ kind: "upload", folder: shown })}
                >
                  <Trans>Upload</Trans>
                </Button>
                <ActionMenu
                  label={t`New file or folder`}
                  text={<Trans>New</Trans>}
                  icon="plus"
                  actions={newMenuActions}
                />
                <Button
                  size="sm"
                  variant="ghost"
                  icon="trash"
                  aria-label={t`Recently deleted`}
                  title={t`Recently deleted`}
                  onClick={() => setSheet({ kind: "deleted" })}
                />
              </div>
            )}
          </div>
        </div>
      )}

      {!reading && archiveJobs.length > 0 ? (
        <div className="files-archive-jobs" role="status" aria-live="polite">
          {archiveJobs.map((job) => {
            const compressing = job.kind === ArchiveJobKind.COMPRESS;
            const entries = Number(job.entries);
            const total = Number(job.totalEntries);
            const bytes = formatBytes(job.bytes, i18n.locale);
            const progressText =
              total > 0
                ? t`${many(entries)} of ${many(total)} items · ${bytes}`
                : entries > 0
                  ? plural(entries, {
                      one: `# item · ${bytes}`,
                      other: `# items · ${bytes}`,
                    })
                  : bytes;
            return (
              <div key={job.jobId} className="files-archive-job">
                <div className="files-archive-job-head">
                  <span className="files-archive-job-title">
                    <Icon name="archive" size={15} />
                    <span>{compressing ? t`Compressing ${nameOf(job.path)}` : t`Extracting ${nameOf(job.path)}`}</span>
                  </span>
                  <span className="files-archive-job-meta">
                    {archiveReconnecting ? t`Reconnecting to check progress` : progressText}
                  </span>
                </div>
                <progress
                  className="files-progress files-archive-progress"
                  value={total > 0 ? Math.min(entries, total) : undefined}
                  max={total > 0 ? total : undefined}
                  aria-label={compressing ? t`Compressing ${nameOf(job.path)}` : t`Extracting ${nameOf(job.path)}`}
                />
              </div>
            );
          })}
        </div>
      ) : null}

      {!reading && notice ? (
        <Callout
          tone={notice.tone}
          title={notice.title}
          actions={
            <>
              {notice.action ? (
                <Button size="sm" variant="secondary" icon={notice.action.icon} onClick={notice.action.onClick}>
                  {notice.action.label}
                </Button>
              ) : null}
              <Button size="sm" variant="ghost" onClick={() => setNotice(undefined)}>
                <Trans>Dismiss</Trans>
              </Button>
            </>
          }
        >
          {notice.body}
        </Callout>
      ) : null}

      {!reading && deleted && deleted.length > 0 ? (
        <Callout
          tone={restoreFailure ? "critical" : "info"}
          title={
            singleDeleted
              ? singleDeleted.kept
                ? t`Deleted ${nameOf(singleDeleted.path)}`
                : t`Deleted ${nameOf(singleDeleted.path)} for good`
              : plural(deleted.length, { one: "Deleted # item", other: "Deleted # items" })
          }
          actions={
            <>
              {singleDeleted?.kept ? (
                <Button size="sm" icon="undo" loading={restoring} onClick={() => void restoreSingle(singleDeleted)}>
                  <Trans context="what was deleted">Put back</Trans>
                </Button>
              ) : deleted.some((d) => d.kept) ? (
                <Button size="sm" icon="trash" onClick={() => setSheet({ kind: "deleted" })}>
                  <Trans>Recently deleted</Trans>
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
            (singleDeleted ? (
              singleDeleted.kept ? (
                <Trans>It's in Recently deleted for 7 days, where you can put it back.</Trans>
              ) : (
                <Trans>Nothing brings it back.</Trans>
              )
            ) : deleted.some((d) => d.kept) ? (
              <Trans>They're in Recently deleted for 7 days, where you can put them back.</Trans>
            ) : (
              <Trans>Nothing brings them back.</Trans>
            ))}
        </Callout>
      ) : null}

      <div className="files-panes">
        {reading ? null : (
          <div className="files-sidebar-wrap" style={besideFolders ? { flexBasis: treeWidth } : undefined}>
            <Panel flush className="files-places">
              <nav aria-label={t`Places`}>
                <PlaceGroup title={t`Common places`} places={places.common} current={current} serverId={serverId} />
                <div className="files-places-rule" aria-hidden="true" />
                {phone ? (
                  <PlaceGroup title={t`Server`} places={places.server} current={current} serverId={serverId} />
                ) : (
                  // biome-ignore lint/a11y/useSemanticElements: a fieldset is for a form's fields, and these are links
                  <div role="group" aria-labelledby={treeHead} className="files-places-group">
                    <div id={treeHead} className="files-places-head">
                      {t`Server`}
                    </div>
                    <FolderTree
                      serverId={serverId}
                      current={shown}
                      retry={retryListing}
                      onFolderContextMenu={(target, position) => setMenu({ kind: "tree", target, position })}
                    />
                  </div>
                )}
              </nav>
            </Panel>
            {besideFolders ? (
              // biome-ignore lint/a11y/useSemanticElements: interactive resize handle between tree and list
              <div
                role="separator"
                aria-orientation="vertical"
                aria-label={t`Resize folder tree`}
                aria-valuemin={MIN_TREE_WIDTH}
                aria-valuemax={MAX_TREE_WIDTH}
                aria-valuenow={treeWidth}
                tabIndex={0}
                className="files-tree-splitter"
                onPointerDown={startTreeResize}
                onKeyDown={onTreeSplitterKeyDown}
                onDoubleClick={() => {
                  setTreeWidth(DEFAULT_TREE_WIDTH);
                  writeTreeWidth(viewerId, DEFAULT_TREE_WIDTH);
                }}
              />
            ) : null}
          </div>
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
              setSheet({ kind: "upload", folder: shown, chosen: chosenFrom(event.dataTransfer) });
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
                {selectedRows.length > 1 ? (
                  <div className="files-selection-bar" role="toolbar" aria-label={t`Selection actions`}>
                    <span className="files-selection-count">
                      {plural(selectedRows.length, { one: "# selected", other: "# selected" })}
                    </span>
                    <div className="files-selection-actions">
                      {multiDownloadPaths.length > 0 ? (
                        multiDownloadTooLong ? (
                          <Button size="sm" variant="secondary" icon="download" onClick={warnTooManyToDownload}>
                            <Trans>Download</Trans>
                          </Button>
                        ) : (
                          <ButtonLink size="sm" variant="secondary" icon="download" href={multiDownloadUrl} download>
                            <Trans>Download</Trans>
                          </ButtonLink>
                        )
                      ) : null}
                      {multiCompressPaths.length > 0 ? (
                        <Button
                          size="sm"
                          variant="ghost"
                          icon="archive"
                          onClick={() => setSheet({ kind: "compress", folder: shown, targets: multiCompressPaths })}
                        >
                          <Trans>Compress</Trans>
                        </Button>
                      ) : null}
                      {multiChangeableTargets.length > 0 ? (
                        <>
                          <Button
                            size="sm"
                            variant="ghost"
                            icon="arrow-right"
                            onClick={() => setSheet({ kind: "move", folder: shown, targets: multiChangeableTargets })}
                          >
                            <Trans>Move</Trans>
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            icon="trash"
                            className="files-details-danger"
                            onClick={() => setSheet({ kind: "delete", targets: multiChangeableTargets })}
                          >
                            <Trans>Delete</Trans>
                          </Button>
                        </>
                      ) : null}
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => {
                          setSelected(new Set());
                          setAnchor(undefined);
                        }}
                      >
                        <Trans>Clear</Trans>
                      </Button>
                    </div>
                  </div>
                ) : null}
                <FileTable
                  rows={rows}
                  caption={t`Files in ${shown}`}
                  phone={phone}
                  tight={hasRightPane}
                  sortable={!big}
                  sortField={sortField}
                  sortDir={sortDir}
                  onSort={handleSort}
                  columnWidths={columnWidths}
                  onColumnWidthChange={handleColumnWidthChange}
                  selected={selected}
                  activeFileName={file ? nameOf(file) : undefined}
                  onSelectChange={(next, nextAnchor) => {
                    setSelected(next);
                    setAnchor(nextAnchor);
                    if (next.size !== 1) setPreviewFullWindow(false);
                  }}
                  anchor={anchor}
                  onOpen={openRow}
                  onRowContextMenu={(row, trigger) => setMenu({ kind: "row", row, trigger })}
                  onEmptyContextMenu={(position) => setMenu({ kind: "empty", position })}
                  renaming={renaming}
                  renameError={renameError}
                  renameBusy={renameBusy}
                  onStartRename={(row) => {
                    setRenaming(row.name);
                    setRenameError(undefined);
                  }}
                  onCommitRename={(row, nextName) => void commitInlineRename(row, nextName)}
                  onCancelRename={() => {
                    setRenaming(undefined);
                    setRenameError(undefined);
                  }}
                  loading={query.isPending || (searching && search.isPending)}
                  loadingLabel={
                    searching && search.isPending ? t`Searching every name in this folder` : t`Reading this folder`
                  }
                  maxHeight={phone || besideFolders ? undefined : 640}
                  empty={
                    searching && search.error ? (
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
          <FileViewer
            serverId={serverId}
            tabs={openTabs.includes(file) ? openTabs : [...openTabs, file]}
            activePath={file}
            line={line}
            fullWindow={editorFullWindow}
            onToggleFullWindow={() => setEditorFullWindow((was) => !was)}
            onSelectTab={(tabPath) => go({ path: shown, file: tabPath })}
            onCloseTab={closeTab}
            initialReads={previewReads.current}
            onConsumedInitialRead={onConsumedInitialRead}
          />
        ) : singleSelected ? (
          <FileDetailsPane
            key={join(shown, singleSelected.name)}
            serverId={serverId}
            entry={singleSelected.entry}
            path={join(shown, singleSelected.name)}
            isFolder={singleSelected.folder}
            downloadUrl={
              downloadableEntry(singleSelected.entry)
                ? downloadFileURL({ serverId, path: join(shown, singleSelected.name) })
                : undefined
            }
            fullWindow={previewFullWindow}
            onToggleFullWindow={() => setPreviewFullWindow((was) => !was)}
            onOpen={() => openRow(singleSelected)}
            onRename={() => {
              setRenaming(singleSelected.name);
              setRenameError(undefined);
            }}
            onMove={() => setSheet({ kind: "move", folder: shown, targets: [targetOf(shown, singleSelected.entry)] })}
            onCompress={() =>
              setSheet({ kind: "compress", folder: shown, targets: [targetOf(shown, singleSelected.entry)] })
            }
            onExtract={
              archiveKind(singleSelected.name) ? () => void runExtract(join(shown, singleSelected.name)) : undefined
            }
            onDelete={() => setSheet({ kind: "delete", targets: [targetOf(shown, singleSelected.entry)] })}
            onClose={() => {
              setSelected(new Set());
              setAnchor(undefined);
              setPreviewFullWindow(false);
            }}
            onPreviewRead={onPreviewRead}
            cachedRead={previewReads.current.get(join(shown, singleSelected.name))}
          />
        ) : (
          <div className="files-idle">
            <EmptyState icon="file" title={<Trans>No file selected</Trans>}>
              <p>
                <Trans>Select a file to inspect it, or double-click to open it.</Trans>
              </p>
            </EmptyState>
          </div>
        )}
      </div>

      <ContextMenu
        open={menu !== undefined}
        label={
          menu?.kind === "row"
            ? t`Actions for ${menu.row.name}`
            : menu?.kind === "tree"
              ? t`Actions for ${menu.target.name}`
              : t`Folder actions`
        }
        items={menuItems}
        position={menu?.kind === "row" ? menu.trigger.position : menu?.position}
        anchor={menu?.kind === "row" ? menu.trigger.anchor : undefined}
        onClose={() => setMenu(undefined)}
      />

      {sheet?.kind === "name" ? (
        <NameSheet
          serverId={serverId}
          folder={sheet.folder}
          naming={sheet.naming}
          onClose={() => setSheet(undefined)}
          onDone={(path, kind) => {
            setSheet(undefined);
            if (sheet.naming.kind === "rename") {
              away(sheet.naming.target.path, path);
              if (shown === sheet.naming.target.path) go({ path });
            } else if (kind === FileType.FILE) {
              go({ path: sheet.folder, file: path });
            }
          }}
        />
      ) : null}

      {sheet?.kind === "move" ? (
        <MoveSheet
          serverId={serverId}
          folder={sheet.folder}
          targets={sheet.targets}
          onClose={() => setSheet(undefined)}
          onDone={(moved) => {
            setSheet(undefined);
            setSelected(new Set());
            setAnchor(undefined);
            for (const item of moved) away(item.from, item.to);
          }}
        />
      ) : null}

      {sheet?.kind === "compress" ? (
        <CompressSheet
          serverId={serverId}
          folder={sheet.folder}
          targets={sheet.targets}
          onClose={() => setSheet(undefined)}
          onDone={(job) => {
            setSheet(undefined);
            setNotice(undefined);
            trackArchiveJob(job);
          }}
        />
      ) : null}

      {sheet?.kind === "delete" ? (
        <DeleteSheet
          serverId={serverId}
          targets={sheet.targets}
          onClose={() => setSheet(undefined)}
          onDone={(results) => {
            setSheet(undefined);
            setRestoreFailure(undefined);
            setDeleted(results);
            setSelected(new Set());
            setAnchor(undefined);
            for (const item of results) {
              away(item.path);
              if (shown === item.path) go({ path: parentOf(item.path) });
            }
          }}
        />
      ) : null}

      {sheet?.kind === "upload" ? (
        <UploadSheet
          serverId={serverId}
          folder={sheet.folder}
          chosen={sheet.chosen}
          onClose={() => setSheet(undefined)}
        />
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
