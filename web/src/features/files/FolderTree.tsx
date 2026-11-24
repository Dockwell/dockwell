// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType } from "@dockwell/api";
import { cx, Icon, Skeleton } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import { join } from "./paths";

/** How many folders one folder shows in the tree before it says how many more the list has. */
export const TREE_PAGE = 60;

/** Compact indent per tree depth level in pixels so deep trees stay legible. */
const TREE_INDENT_PX = 10;

/** Whether an entry opens as a folder, itself or through a link. */
function opensAsFolder(entry: FileEntry): boolean {
  return (
    entry.type === FileType.DIRECTORY || (entry.type === FileType.SYMLINK && entry.linkType === FileType.DIRECTORY)
  );
}

/** Whether a folder is the one you're in, or on the way to it. */
function leadsTo(path: string, current: string): boolean {
  return path === "/" || current === path || current.startsWith(`${path}/`);
}

export interface TreeFolderTarget {
  path: string;
  name: string;
  refused: boolean;
}

interface TreeState {
  serverId: string;
  /** The folder the list shows. */
  current: string;
  /** Folders opened by hand, and those on the way to the current one closed by hand. */
  opened: ReadonlySet<string>;
  closed: ReadonlySet<string>;
  toggle: (path: string, open: boolean) => void;
  /** Whether a listing that failed is worth another try, as the page decides it for its own. */
  retry: (failures: number, error: unknown) => boolean;
  onContextMenu?: ((target: TreeFolderTarget, position: { x: number; y: number }) => void) | undefined;
}

/**
 * The server's folders as a tree under Common places: the way to the folder
 * you're in opens automatically, deep levels scroll horizontally instead of
 * clipping every level with ellipses, and right-clicking a folder opens its
 * context menu.
 */
export function FolderTree({
  serverId,
  current,
  retry,
  onFolderContextMenu,
}: {
  serverId: string;
  current: string;
  retry: TreeState["retry"];
  onFolderContextMenu?: (target: TreeFolderTarget, position: { x: number; y: number }) => void;
}) {
  const [opened, setOpened] = useState<ReadonlySet<string>>(new Set());
  const [closed, setClosed] = useState<ReadonlySet<string>>(new Set());

  // Navigating to a folder reveals it in the tree even if an ancestor was previously closed.
  useEffect(() => {
    setClosed((was) => {
      if (was.size === 0) return was;
      let changed = false;
      const next = new Set(was);
      for (const p of was) {
        if (leadsTo(p, current)) {
          next.delete(p);
          changed = true;
        }
      }
      return changed ? next : was;
    });
  }, [current]);

  const toggle = (path: string, open: boolean) => {
    const set = (from: ReadonlySet<string>, has: boolean) => {
      const next = new Set(from);
      if (has) next.add(path);
      else next.delete(path);
      return next;
    };
    setOpened((was) => set(was, open));
    setClosed((was) => set(was, !open));
  };

  const state: TreeState = {
    serverId,
    current,
    opened,
    closed,
    toggle,
    retry,
    onContextMenu: onFolderContextMenu,
  };
  return (
    <ul className="files-tree">
      <Folder path="/" name="/" depth={0} state={state} />
    </ul>
  );
}

function Folder({
  path,
  name,
  depth,
  refused = false,
  state,
}: {
  path: string;
  name: string;
  depth: number;
  refused?: boolean;
  state: TreeState;
}) {
  const { t } = useLingui();
  const { serverId, current, opened, closed, toggle, onContextMenu } = state;
  const open = !refused && (opened.has(path) || (leadsTo(path, current) && !closed.has(path)));
  const here = path === current;
  const row = useRef<HTMLDivElement>(null);

  // Bring the current folder into view inside the tree (both vertically and horizontally).
  useEffect(() => {
    const el = row.current;
    const tree = el?.closest<HTMLElement>(".files-tree");
    if (!here || !el || !tree) return;
    const elRect = el.getBoundingClientRect();
    const treeRect = tree.getBoundingClientRect();
    const top = elRect.top - treeRect.top + tree.scrollTop;
    if (top < tree.scrollTop || top + el.offsetHeight > tree.scrollTop + tree.clientHeight) {
      tree.scrollTop = Math.max(0, top - (tree.clientHeight - el.offsetHeight) / 2);
    }
    const left = depth * TREE_INDENT_PX;
    if (left < tree.scrollLeft || left + 120 > tree.scrollLeft + tree.clientWidth) {
      tree.scrollLeft = Math.max(0, left - 16);
    }
  }, [here, depth]);

  return (
    <li>
      <div
        ref={row}
        className={cx("files-tree-row", here && "is-current")}
        style={{ paddingInlineStart: `${depth * TREE_INDENT_PX}px` }}
      >
        {refused ? (
          <span className="files-tree-toggle" aria-hidden="true" />
        ) : (
          <button
            type="button"
            className="files-tree-toggle"
            aria-expanded={open}
            aria-label={open ? t`Hide the folders in ${name}` : t`Show the folders in ${name}`}
            onClick={() => toggle(path, !open)}
          >
            <Icon name={open ? "chevron-down" : "chevron-right"} size={12} />
          </button>
        )}
        <Link
          to="/servers/$serverId/files"
          params={{ serverId }}
          search={{ path }}
          className="files-tree-link"
          title={path}
          // The router marks the folder's own page; this says where you are while a file is open in it.
          aria-current={here ? "location" : undefined}
          onContextMenu={(event) => {
            if (!onContextMenu) return;
            event.preventDefault();
            event.stopPropagation();
            onContextMenu({ path, name, refused }, { x: event.clientX, y: event.clientY });
          }}
        >
          <Icon name={refused ? "lock" : "folder"} size={14} />
          <span className="files-tree-name" title={name}>
            {name}
          </span>
        </Link>
      </div>
      {open ? <Children path={path} depth={depth + 1} state={state} /> : null}
    </li>
  );
}

/** A folder's own folders, read once it's open. */
function Children({ path, depth, state }: { path: string; depth: number; state: TreeState }) {
  const { t } = useLingui();
  const query = useQuery(
    FileService.method.listFiles,
    path === state.current ? { serverId: state.serverId, path } : { serverId: state.serverId, path, lookup: true },
    {
      retry: state.retry,
      refetchOnMount: false,
      refetchOnWindowFocus: (q) => q.state.status !== "error" && !q.state.data?.nextPage,
    },
  );
  const indent = { paddingInlineStart: `${depth * TREE_INDENT_PX + 22}px` };
  if (query.error) {
    return (
      <p className="files-tree-note" style={indent}>
        {t`Can't read this folder`}
      </p>
    );
  }
  if (!query.data) {
    return (
      <ul aria-busy="true" aria-label={t`Reading this folder`}>
        {[88, 64].map((width) => (
          <li key={width} className="files-tree-note" style={indent} aria-hidden="true">
            <Skeleton text="label" width={width} />
          </li>
        ))}
      </ul>
    );
  }
  const folders = query.data.entries
    .filter((entry) => opensAsFolder(entry) && !entry.unreadableName)
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  if (folders.length === 0) return null;
  const shown = folders.slice(0, TREE_PAGE);
  const onTheWay = folders.slice(TREE_PAGE).find((entry) => leadsTo(join(path, entry.name), state.current));
  if (onTheWay) shown.push(onTheWay);
  const more = folders.length - shown.length;
  return (
    <ul>
      {shown.map((entry) => (
        <Folder
          key={entry.name}
          path={join(path, entry.name)}
          name={entry.name}
          depth={depth}
          refused={entry.refused}
          state={state}
        />
      ))}
      {more > 0 ? (
        <li className="files-tree-note" style={indent}>
          {plural(more, { one: "and # more, in its list", other: "and # more, in its list" })}
        </li>
      ) : null}
    </ul>
  );
}
