// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import { Icon } from "../Icon/Icon";
import "./FolderPicker.css";

/** What a folder holds, as far as choosing one needs: its own folders, by name. */
export interface FolderListing {
  /** The folder's path as the server resolved it, with links followed. */
  path: string;
  folders: readonly { name: string; locked?: boolean }[];
  /**
   * Set where the folder holds more than a listing carries: how many entries the listing
   * shows, and how many there are, 0 where the server can't say. The picker says so,
   * and finds a folder by name instead.
   */
  cut?: { shown: number; total: number };
}

/** How long typing rests before a name is looked for on the server. */
const FIND_AFTER_MS = 300;

export interface FolderPickerProps {
  /** Sits above, in sentence case. */
  label: ReactNode;
  /** The folder chosen, as an absolute path. */
  value: string;
  onChange: (path: string) => void;
  /**
   * Reads a folder's own folders: all of them, or, given a name to find, those whose
   * names hold it, searched on the server. It rejects when the folder isn't there or
   * can't be read, and the picker then looks in the folder above.
   */
  list: (path: string, find?: string) => Promise<FolderListing>;
  /**
   * What's known of the chosen folder, as one short line under it, such as "New, made
   * with the site" or "12 files".
   */
  note?: ReactNode;
  /** Replaces the note. Say what is wrong and how to fix it. */
  error?: ReactNode;
  /** Starts open, as when a form sends someone back to it. */
  defaultOpen?: boolean;
  disabled?: boolean;
  /**
   * Why Change is off for this reader, as a role refuses it. It stays in place and in
   * the tab order, as the Button's own off does.
   */
  off?: string | undefined;
  className?: string;
}

/** The folder a path sits in: "/" for what's at the top. */
function parentOf(path: string): string {
  const cut = path.replace(/\/+$/, "").lastIndexOf("/");
  return cut <= 0 ? "/" : path.slice(0, cut);
}

const join = (dir: string, name: string) => (dir === "/" ? `/${name}` : `${dir}/${name}`);

/** Each folder on the way to a path, from the top. */
function crumbs(path: string): { name: string; path: string }[] {
  const out = [{ name: "/", path: "/" }];
  for (const name of path.split("/").filter(Boolean)) out.push({ name, path: join(out.at(-1)?.path ?? "/", name) });
  return out;
}

/**
 * Chooses a folder on a server. Closed, it is the folder and Change, with what's known
 * of the folder on the line under. Open, a person browses the server's folders and picks one, names a new
 * one inside the folder they're looking at, or types a path, as someone who knows it
 * would. It makes nothing itself: a new folder is whatever uses the path's to make.
 */
export function FolderPicker({
  label,
  value,
  onChange,
  list,
  note,
  error,
  defaultOpen = false,
  disabled = false,
  off,
  className,
}: FolderPickerProps) {
  const strings = useUIStrings().folderPicker;
  const id = useId();
  const [open, setOpen] = useState(defaultOpen);
  // The folder being looked in, and what it holds once read.
  const [at, setAt] = useState("/");
  const [listing, setListing] = useState<FolderListing>();
  const [failed, setFailed] = useState(false);
  const [naming, setNaming] = useState("");
  const asked = useRef(0);
  // A name to find in the folder being looked in, for one that holds more than a listing
  // carries, and the folders whose names hold it.
  const [finding, setFinding] = useState("");
  const [found, setFound] = useState<FolderListing>();
  const [findFailed, setFindFailed] = useState(false);
  const invalid = hasContent(error);
  const noted = !invalid && hasContent(note);

  // Something refused opens it, with the path to change, and its going away as it's
  // fixed never closes it under the cursor: only Done does.
  useEffect(() => {
    if (invalid) setOpen(true);
  }, [invalid]);

  /** Looks in a folder, and in the one above when it isn't there, up to the top. */
  const look = (path: string) => {
    const mine = ++asked.current;
    setListing(undefined);
    setFailed(false);
    setFinding("");
    const tryAt = (where: string) => {
      list(where).then(
        (found) => {
          if (asked.current !== mine) return;
          setAt(where);
          setListing(found);
        },
        () => {
          if (asked.current !== mine) return;
          if (where === "/") {
            setAt("/");
            setFailed(true);
          } else tryAt(parentOf(where));
        },
      );
    };
    tryAt(path);
  };

  // A name is looked for on the server a moment after the last key, in the folder being
  // looked in, so a folder of any size finds one; what an earlier name found never lands
  // over a later one's.
  useEffect(() => {
    const name = finding.trim();
    setFound(undefined);
    setFindFailed(false);
    if (name === "") return;
    let current = true;
    const timer = setTimeout(() => {
      list(at, name).then(
        (r) => {
          if (current) setFound(r);
        },
        () => {
          if (current) setFindFailed(true);
        },
      );
    }, FIND_AFTER_MS);
    return () => {
      current = false;
      clearTimeout(timer);
    };
  }, [finding, at, list]);

  // Opening looks where the chosen folder would be: in it when it's there, above it otherwise.
  // biome-ignore lint/correctness/useExhaustiveDependencies: only as it opens, not as the value follows what's picked.
  useEffect(() => {
    if (open) look(value.startsWith("/") ? value : "/");
  }, [open]);

  // What the list shows: the folders whose names hold what's typed, or all the folder's.
  const searching = finding.trim() !== "";
  const shown = searching ? found : listing;
  const cantRead = searching ? findFailed : failed;

  const choose = (path: string) => {
    onChange(path);
    look(path);
  };
  const add = () => {
    const name = naming.trim().replace(/^\/+|\/+$/g, "");
    if (name === "" || name.includes("/")) return;
    onChange(join(at, name));
    setNaming("");
  };

  return (
    <div className={cx("dw-field", "dw-folder", invalid && "is-invalid", className)}>
      <span className="dw-field-label" id={`${id}-label`}>
        {label}
      </span>
      <div className="dw-folder-summary">
        <span className="dw-folder-icon" aria-hidden="true">
          <Icon name="folder" size={16} />
        </span>
        <code className="dw-folder-path" title={value}>
          {value || strings.none}
        </code>
        {/* Said as it changes, and what Change is described by, as a reader there needs it too. */}
        <span id={`${id}-note`} className="dw-folder-note" aria-live="polite">
          {noted ? note : null}
        </span>
        <Button
          size="sm"
          variant="ghost"
          disabled={disabled}
          off={off}
          aria-expanded={open}
          aria-describedby={noted ? `${id}-note` : undefined}
          aria-controls={`${id}-panel`}
          aria-label={open ? strings.done : strings.changeNamed(value)}
          onClick={() => setOpen(!open)}
        >
          {open ? strings.done : strings.change}
        </Button>
      </div>
      {invalid ? <div className="dw-field-error">{error}</div> : null}
      {open ? (
        // biome-ignore lint/a11y/useSemanticElements: a fieldset would nest a second legend under the field's label
        <div className="dw-folder-panel" id={`${id}-panel`} role="group" aria-labelledby={`${id}-label`}>
          <label className="dw-folder-typed">
            <span className="dw-folder-sub">{strings.path}</span>
            <input
              className="dw-folder-input"
              value={value}
              spellCheck={false}
              autoCapitalize="none"
              autoCorrect="off"
              aria-invalid={invalid || undefined}
              onChange={(event) => onChange(event.target.value)}
              // A path that's typed is looked in once it's left, so the list follows it.
              onBlur={() => {
                if (value.startsWith("/")) look(value);
              }}
            />
          </label>
          <nav className="dw-folder-crumbs" aria-label={strings.lookingIn}>
            {crumbs(at).map((c, i, all) => (
              <span key={c.path}>
                {i > 1 ? <span aria-hidden="true">/</span> : null}
                <button
                  type="button"
                  className="dw-folder-crumb"
                  aria-current={i === all.length - 1 ? "location" : undefined}
                  onClick={() => choose(c.path)}
                >
                  {c.name}
                </button>
              </span>
            ))}
          </nav>
          {listing?.cut || searching ? (
            <div className="dw-folder-find">
              {listing?.cut ? (
                <p className="dw-folder-cut" id={`${id}-cut`}>
                  {strings.cut(listing.cut.shown, listing.cut.total)}
                </p>
              ) : null}
              <label className="dw-folder-typed">
                <span className="dw-folder-sub">{strings.findIn(at)}</span>
                <input
                  className="dw-folder-input"
                  type="search"
                  value={finding}
                  placeholder={strings.findName}
                  spellCheck={false}
                  autoCapitalize="none"
                  autoCorrect="off"
                  aria-describedby={listing?.cut ? `${id}-cut` : undefined}
                  onChange={(event) => setFinding(event.target.value)}
                  onKeyDown={(event) => {
                    // The form around it isn't sent by finding a folder.
                    if (event.key === "Enter") event.preventDefault();
                  }}
                />
              </label>
            </div>
          ) : null}
          <ul
            className="dw-folder-list"
            aria-label={strings.foldersIn(at)}
            aria-busy={shown === undefined && !cantRead}
          >
            {cantRead ? (
              <li className="dw-folder-quiet">{strings.cantRead}</li>
            ) : shown === undefined ? (
              <li className="dw-folder-quiet">{strings.reading}</li>
            ) : shown.folders.length === 0 ? (
              <li className="dw-folder-quiet">
                {searching ? strings.noMatch(finding.trim()) : shown.cut ? strings.noneShown : strings.empty}
              </li>
            ) : (
              shown.folders.map((folder) => {
                const path = join(at, folder.name);
                return (
                  <li key={folder.name}>
                    <button
                      type="button"
                      className={cx("dw-folder-row", path === value && "is-chosen")}
                      disabled={folder.locked}
                      aria-pressed={path === value}
                      onClick={() => choose(path)}
                    >
                      <Icon name={folder.locked ? "lock" : "folder"} size={14} />
                      <span>{folder.name}</span>
                    </button>
                  </li>
                );
              })
            )}
          </ul>
          {searching && found?.cut ? <p className="dw-folder-cut">{strings.moreMatch}</p> : null}
          <div className="dw-folder-new">
            <label className="dw-folder-typed">
              <span className="dw-folder-sub">{strings.newIn(at)}</span>
              <input
                className="dw-folder-input"
                value={naming}
                placeholder={strings.newName}
                spellCheck={false}
                autoCapitalize="none"
                autoCorrect="off"
                onChange={(event) => setNaming(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key !== "Enter") return;
                  // The form around it isn't sent by naming a folder.
                  event.preventDefault();
                  add();
                }}
              />
            </label>
            <Button size="sm" icon="plus" disabled={naming.trim() === "" || naming.includes("/")} onClick={add}>
              {strings.use}
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
