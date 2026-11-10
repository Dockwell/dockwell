// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType } from "@dockwell/api";
import { cx, Icon } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import { join } from "./paths";

/** How many folders one folder shows in the tree before it says how many more the list has. */
export const TREE_PAGE = 60;

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
}

/**
 * The server's folders as a tree, as Server-Files has it under the places: the way to
 * the folder you're in stands open, and any folder opens in place. It lists a folder
 * only once it's open. The folder you're in is the page's own listing. The others are
 * lookups, as a form's are: the tree opened them, not a person, so one the server
 * refuses, such as Docker's data above a volume, goes on no Activity.
 */
export function FolderTree({
  serverId,
  current,
  retry,
}: {
  serverId: string;
  current: string;
  retry: TreeState["retry"];
}) {
  const [opened, setOpened] = useState<ReadonlySet<string>>(new Set());
  const [closed, setClosed] = useState<ReadonlySet<string>>(new Set());
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
  const state: TreeState = { serverId, current, opened, closed, toggle, retry };
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
  const { serverId, current, opened, closed, toggle } = state;
  const open = !refused && (opened.has(path) || (leadsTo(path, current) && !closed.has(path)));
  const here = path === current;
  const row = useRef<HTMLDivElement>(null);
  // The folder you're in is brought into the tree's own view, without moving the page under you.
  useEffect(() => {
    const el = row.current;
    const tree = el?.closest<HTMLElement>(".files-tree");
    if (!here || !el || !tree) return;
    const top = el.getBoundingClientRect().top - tree.getBoundingClientRect().top + tree.scrollTop;
    if (top < tree.scrollTop || top + el.offsetHeight > tree.scrollTop + tree.clientHeight) {
      tree.scrollTop = Math.max(0, top - (tree.clientHeight - el.offsetHeight) / 2);
    }
  }, [here]);
  return (
    <li>
      <div
        ref={row}
        className={cx("files-tree-row", here && "is-current")}
        style={{ paddingInlineStart: `calc(${depth} * var(--space-3))` }}
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
        >
          <Icon name={refused ? "lock" : "folder"} size={14} />
          <span className="files-tree-name">{name}</span>
        </Link>
      </div>
      {open ? <Children path={path} depth={depth + 1} state={state} /> : null}
    </li>
  );
}

/** A folder's own folders, read once it's open. */
function Children({ path, depth, state }: { path: string; depth: number; state: TreeState }) {
  const { t } = useLingui();
  // For the folder the page shows, the page's own query, so the two share one answer.
  // What failed isn't asked again each time the window takes focus, nor a folder larger than a page, which
  // its agent reads every name of to answer.
  const query = useQuery(
    FileService.method.listFiles,
    path === state.current ? { serverId: state.serverId, path } : { serverId: state.serverId, path, lookup: true },
    // The page's listing is as new as the tree needs, so a folder opening here doesn't ask for it again.
    {
      retry: state.retry,
      refetchOnMount: false,
      refetchOnWindowFocus: (q) => q.state.status !== "error" && !q.state.data?.nextPage,
    },
  );
  const indent = { paddingInlineStart: `calc(${depth} * var(--space-3) + 22px)` };
  if (query.error) {
    return (
      <p className="files-tree-note" style={indent}>
        {t`Can't read this folder`}
      </p>
    );
  }
  if (!query.data) return null;
  const folders = query.data.entries
    .filter((entry) => opensAsFolder(entry) && !entry.unreadableName)
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  if (folders.length === 0) return null;
  const shown = folders.slice(0, TREE_PAGE);
  // The way to where you are is always there, however far down its folder's list it comes.
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
