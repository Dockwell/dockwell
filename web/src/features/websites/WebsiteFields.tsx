// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type FileEntry, FileService, FileType } from "@dockwell/api";
import { type FolderListing, FolderPicker, SelectField, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { nameOf, parentOf } from "../files/paths";
import { indexOf, type Missing, type Serves, type Settings } from "./settings";
import { useRuntimes } from "./website";

/** What a form says of a field it can refuse by itself. */
export function useMissingWords(): (m: Missing) => string {
  const { t } = useLingui();
  return (m) => {
    switch (m.why) {
      case "port":
        return t`Use a port from 1 to 65535.`;
      case "url":
        return t`Give the URL to send to, such as https://backend.example.com.`;
      case "url-scheme":
        return t`Start it with http:// or https://, such as https://backend.example.com.`;
      case "root":
        return t`Give the folder that holds its files, such as /var/www/blog.`;
      case "root-absolute":
        return t`Give the folder from the top, starting with /, such as /var/www/blog.`;
      case "user":
        return t`Give the user by number, such as 1000.`;
      case "upload":
        return t`Give a number of MiB, such as 64.`;
    }
  };
}

/** What each kind of website is, in the words its forms choose it by. */
export function useServesWords(): Record<Serves, string> {
  const { t } = useLingui();
  return { folder: t`Files in a folder`, php: t`PHP`, url: t`A URL`, port: t`A port on this server` };
}

/** What a field's hint says once it differs from what's stored: what it was, as a plan marks a change. */
export function WasHint({ children }: { children: ReactNode }) {
  return (
    <span className="website-was">
      <span className="dw-plan-sym is-change" aria-hidden="true">
        ~
      </span>
      <span>{children}</span>
    </span>
  );
}

/** How long typing settles before a folder is read. */
const READ_AFTER_MS = 400;

/**
 * Whether everyone may read an entry, as ls's mode says: a file needs r, and a folder
 * x, to be walked. A folder's r lists it, which no website does.
 */
export function openToAll(mode: string, folder: boolean): boolean {
  const others = mode.slice(-3);
  return folder ? others[2] === "x" || others[2] === "t" : others[0] === "r";
}

/** Whether a user, as "uid:gid", may write in a folder that ls's mode and its owner's numbers describe. */
export function canWrite(user: string, mode: string, uid: number, gid: number): boolean {
  const [u = "", g = ""] = user.split(":");
  if (Number(u) === uid) return mode[2] === "w";
  if (Number(g) === gid) return mode[5] === "w";
  return mode[8] === "w";
}

/** Who PHP runs as unless a website says: www-data, by number. */
export const DEFAULT_PHP_USER = "82:82";

/** Who PHP runs as, as the server keeps it: "uid:gid", a number alone for both. */
export function phpUser(typed: string): string {
  const u = typed.trim();
  if (u === "") return DEFAULT_PHP_USER;
  return u.includes(":") ? u : `${u}:${u}`;
}

/** Which of a kind's fields a form shows: what everyone sets, what has a good default, or both. */
export type FieldsPart = "main" | "advanced" | "all";

/** Whether a kind of website has anything under Advanced. */
export function hasAdvanced(serves: Serves): boolean {
  return serves !== "port";
}

/**
 * The fields of what a website serves, for the kind its settings say: a folder, a URL
 * or a port on the server. New website shows what everyone sets, and keeps the rest,
 * which all start at a good default, under Advanced; a website's configuration shows
 * both, and was, where there is one, says what each field had.
 */
export function WebsiteFields({
  settings: s,
  onChange,
  was,
  errors,
  server,
  part = "all",
}: {
  settings: Settings;
  onChange: (next: Settings) => void;
  was?: Settings;
  errors: Partial<Record<Missing["field"], string | undefined>>;
  /** The server it runs on, whose folders the folder field reads. */
  server: string;
  part?: FieldsPart;
}) {
  const { t } = useLingui();
  const same = was?.serves === s.serves;
  const runtimes = useRuntimes();
  const main = part !== "advanced";
  const advanced = part !== "main";
  const folder = (
    <FolderField settings={s} was={same ? was : undefined} error={errors.root} server={server} onChange={onChange} />
  );
  if (s.serves === "php") {
    const newest = runtimes.data?.[0]?.version ?? "";
    const version = s.version || newest;
    const versions = runtimes.data ?? (version ? [{ version, release: "" }] : []);
    const wasVersion = was?.version || newest;
    return (
      <>
        {main ? (
          <>
            <SelectField
              label={t`PHP version`}
              value={version}
              options={versions.map((r) => ({
                value: r.version,
                label: r.release ? t`PHP ${r.version} · ${r.release}` : t`PHP ${r.version}`,
              }))}
              hint={
                same && was && version !== wasVersion ? (
                  <WasHint>{t`Was PHP ${wasVersion} · only PHP restarts, with no downtime`}</WasHint>
                ) : undefined
              }
              onChange={(event) => onChange({ ...s, version: event.target.value })}
            />
            {folder}
          </>
        ) : null}
        {advanced ? (
          <>
            <TextField
              label={t`Index files`}
              mono
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="index.php index.html"
              value={s.index}
              hint={
                same && was && indexOf(s.index).join(" ") !== indexOf(was.index).join(" ") ? (
                  <WasHint>{was.index ? t`Was ${was.index}` : t`Was index.php index.html`}</WasHint>
                ) : (
                  t`The first one found answers for a folder.`
                )
              }
              onChange={(event) => onChange({ ...s, index: event.target.value })}
            />
            <SelectField
              label={t`What isn't a file`}
              value={s.filesOnly ? "404" : "front"}
              options={[
                { value: "front", label: t`Goes to index.php, as WordPress and Laravel want` },
                { value: "404", label: t`Gets a 404` },
              ]}
              hint={
                same && was && s.filesOnly !== was.filesOnly ? (
                  <WasHint>{was.filesOnly ? t`Was a 404` : t`Was index.php`}</WasHint>
                ) : undefined
              }
              onChange={(event) => onChange({ ...s, filesOnly: event.target.value === "404" })}
            />
            <TextField
              label={t`Upload limit`}
              inputMode="numeric"
              placeholder="64"
              suffix="MiB"
              value={s.uploadMB}
              error={errors.uploadMB}
              hint={
                same && was && (Number(s.uploadMB) || 64) !== (Number(was.uploadMB) || 64) ? (
                  <WasHint>{t`Was ${Number(was.uploadMB) || 64} MiB`}</WasHint>
                ) : undefined
              }
              onChange={(event) => onChange({ ...s, uploadMB: event.target.value })}
            />
            <TextField
              label={t`Runs as`}
              mono
              placeholder={DEFAULT_PHP_USER}
              value={s.user}
              error={errors.user}
              hint={
                same && was && s.user.trim() !== was.user ? (
                  <WasHint>{was.user ? t`Was ${was.user}` : t`Was 82:82, www-data`}</WasHint>
                ) : (
                  t`A user and group by number, as uid:gid. Empty runs it as www-data.`
                )
              }
              onChange={(event) => onChange({ ...s, user: event.target.value })}
            />
          </>
        ) : null}
      </>
    );
  }
  if (s.serves === "folder") {
    return (
      <>
        {main ? folder : null}
        {advanced ? (
          <>
            <TextField
              label={t`Index files`}
              mono
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="index.html index.htm"
              value={s.index}
              hint={
                same && was && indexOf(s.index).join(" ") !== indexOf(was.index).join(" ") ? (
                  <WasHint>{was.index ? t`Was ${was.index}` : t`Was index.html index.htm`}</WasHint>
                ) : (
                  t`The first one found answers for a folder.`
                )
              }
              onChange={(event) => onChange({ ...s, index: event.target.value })}
            />
            <Switch
              label={t`Single-page app`}
              description={t`A path with no file gets the index, for an app that routes in the browser.`}
              checked={s.singlePageApp}
              onChange={(on) => onChange({ ...s, singlePageApp: on })}
            />
          </>
        ) : null}
      </>
    );
  }
  if (s.serves === "url") {
    return (
      <>
        {main ? (
          <TextField
            label={t`URL`}
            mono
            inputMode="url"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            placeholder="https://backend.example.com"
            value={s.url}
            error={errors.url}
            hint={same && was && s.url.trim() !== was.url ? <WasHint>{t`Was ${was.url}`}</WasHint> : undefined}
            onChange={(event) => onChange({ ...s, url: event.target.value })}
          />
        ) : null}
        {advanced ? (
          <Switch
            label={t`Send the URL's own name as Host`}
            description={t`For a backend that serves several names. Off, it gets this website's domain.`}
            checked={s.targetHost}
            onChange={(on) => onChange({ ...s, targetHost: on })}
          />
        ) : null}
      </>
    );
  }
  return main ? (
    <TextField
      label={t`Port on the server`}
      mono
      inputMode="numeric"
      placeholder="3000"
      value={s.port}
      error={errors.port}
      hint={
        same && was && s.port !== was.port ? (
          <WasHint>{t`Was ${was.port} · whatever listens there sees every visitor as local`}</WasHint>
        ) : (
          t`It sees every visitor as local, so the plan asks you to approve it.`
        )
      }
      onChange={(event) => onChange({ ...s, port: event.target.value })}
    />
  ) : null;
}

/**
 * A website's folder: the one chosen, on one line with what the server says of it, and
 * a picker behind Change that browses the server's folders, names a new one, or takes a
 * typed path. A folder that isn't there is made with the site.
 */
function FolderField({
  settings: s,
  was,
  error,
  server,
  onChange,
}: {
  settings: Settings;
  was: Settings | undefined;
  error: string | undefined;
  server: string;
  onChange: (next: Settings) => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(FileService, transport), [transport]);
  const index = indexOf(s.index);
  // Lookups, as the folder note's are: the form reads them, not a person opening a folder, so
  // one the server refuses goes on no Activity.
  const list = useCallback(
    async (path: string): Promise<FolderListing> => {
      const res = await client.listFiles({ serverId: server, path, lookup: true });
      return {
        path: res.path,
        folders: res.entries
          .filter(
            (e) =>
              !e.unreadableName &&
              (e.type === FileType.DIRECTORY || (e.type === FileType.SYMLINK && e.linkType === FileType.DIRECTORY)),
          )
          .map((e) => ({ name: e.name, locked: e.refused }))
          .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0)),
      };
    },
    [client, server],
  );
  return (
    <FolderPicker
      label={t`Folder`}
      value={s.root}
      error={error}
      // Sent back to it with something wrong, it's open, with the path to change.
      defaultOpen={error !== undefined}
      key={error === undefined ? "closed" : "refused"}
      list={list}
      note={
        was && s.root.trim() !== was.root ? (
          <WasHint>{t`Was ${was.root}`}</WasHint>
        ) : (
          <FolderNote
            server={server}
            root={s.root}
            serves={s.serves}
            index={index.length > 0 ? index : s.serves === "php" ? ["index.php", "index.html"] : []}
            onOwner={
              s.serves === "folder"
                ? (owner) => {
                    if (owner !== s.owner) onChange({ ...s, owner });
                  }
                : undefined
            }
            user={s.serves === "php" ? phpUser(s.user) : undefined}
            onFolderOwner={
              s.serves === "php"
                ? (owner) => {
                    // What's typed stays: the note says when PHP can't write there.
                    if (s.user.trim() === "") onChange({ ...s, user: owner });
                  }
                : undefined
            }
          />
        )
      }
      onChange={(root) => onChange({ ...s, root })}
    />
  );
}

/** A value once typing has settled. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

/**
 * What a website's folder holds, as the server's file browser reads it, said under its
 * field: that it isn't there yet, that it has no index, or that the web server's user
 * can't read it, each with what to do, or what it holds. It reads the folder above
 * first, to see whether the folder is there, as a listing that comes to nothing goes on
 * Activity, and a folder that isn't there yet is no one's business but the form's.
 * Reading the disk is a person's, as Files is, so this is too.
 */
export function FolderNote({
  server,
  root,
  serves = "folder",
  index,
  onOwner,
  user,
  onFolderOwner,
}: {
  server: string;
  root: string;
  /** What the website does with the folder: serves its files, or runs its PHP too. */
  serves?: Serves;
  index: readonly string[];
  /** Hears who the folder is made for when it isn't there: the folder above's owner, or no one. */
  onOwner?: (owner: string) => void;
  /** Who PHP runs as, as "uid:gid", for a PHP website, which writes in its folder. */
  user?: string;
  /** Hears the owner of a folder that's there, for a PHP website to run as, as "uid:gid". */
  onFolderOwner?: (owner: string) => void;
}) {
  const { t } = useLingui();
  const path = useSettled(root.trim().replace(/\/+$/, ""), READ_AFTER_MS);
  const asked = server !== "" && path.startsWith("/") && path.length > 1 && !/\/\.\.?(\/|$)/.test(path);
  const above = useQuery(
    FileService.method.listFiles,
    { serverId: server, path: parentOf(path), lookup: true },
    { enabled: asked, retry: false, refetchOnWindowFocus: false },
  );
  const there = above.data?.entries.find((e) => e.name === nameOf(path));
  const folder =
    there !== undefined &&
    (there.type === FileType.DIRECTORY || (there.type === FileType.SYMLINK && there.linkType === FileType.DIRECTORY));
  const listed = useQuery(
    FileService.method.listFiles,
    { serverId: server, path, lookup: true },
    { enabled: asked && folder, retry: false, refetchOnWindowFocus: false },
  );
  // The folder above not being there either means the same, as Docker makes both. What
  // else can't be read of it says nothing of this one, so this says nothing.
  const aboveMissing = above.error !== null && reasonOf(above.error) === "not_found";
  // A folder that isn't there is made as the one above it is owned, so whoever puts
  // files there now can put the site's there too.
  const parent = above.data?.directory;
  const owner = asked && !there && parent ? `${parent.uid}:${parent.gid}` : "";
  // Only while it isn't there: a folder that's there keeps its owner, whatever's stored.
  const missing = asked && !above.isPending && (aboveMissing || (above.data !== undefined && !there));
  // Each is said once as it's found, so what someone types after stays theirs.
  const toldOwner = useRef<string>(undefined);
  useEffect(() => {
    if (!missing || toldOwner.current === owner) return;
    toldOwner.current = owner;
    onOwner?.(owner);
  }, [missing, owner, onOwner]);
  // A folder that's there, for PHP to run as its owner, unless that's root.
  const listedDir = listed.data?.directory;
  const folderOwner =
    listedDir && listedDir.uid !== 0 && listedDir.gid !== 0 ? `${listedDir.uid}:${listedDir.gid}` : "";
  const toldFolderOwner = useRef("");
  useEffect(() => {
    if (!folderOwner || toldFolderOwner.current === folderOwner) return;
    toldFolderOwner.current = folderOwner;
    onFolderOwner?.(folderOwner);
  }, [folderOwner, onFolderOwner]);
  if (!asked || above.isPending || (above.error && !aboveMissing)) return null;
  // One line: what's there as a state, and nothing a person has to go and run.
  const say = (tone: "new" | "ok" | "warning", words: ReactNode) => (
    <span className={`website-folder-state is-${tone}`}>{words}</span>
  );
  if (aboveMissing || !there) {
    if (user) return say("new", t`New. Made with the site, for PHP to write in.`);
    // Made as the folder above is owned, so whoever puts files there can put the site's there too.
    const by = parent && parent.uid !== 0 ? parent.owner || `${parent.uid}:${parent.gid}` : "";
    return say("new", by ? t`New. Made with the site, owned by ${by}.` : t`New. Made with the site.`);
  }
  if (!folder) return say("warning", t`That's a file, not a folder.`);
  if (listed.isPending) return null;
  if (listed.error) return say("warning", sentence(listed.error));
  const { entries, directory } = listed.data;
  // Files in a folder run no script, and hide them, so a PHP site's folder answers 404 for them.
  if (serves === "folder" && entries.some((e) => e.name === "index.php" && e.type === FileType.FILE)) {
    return say("warning", t`Holds index.php, which only a PHP site runs.`);
  }
  // PHP writes in its folder, as the user it runs as, which root's never is.
  if (user && directory) {
    if (directory.uid === 0 || directory.gid === 0) {
      return say("warning", t`Root owns it, so PHP can't save uploads there.`);
    }
    if (!canWrite(user, directory.mode, directory.uid, directory.gid)) {
      const theirs = `${directory.uid}:${directory.gid}`;
      return say("warning", t`PHP runs as ${user} and can't write there. Its owner is ${theirs}.`);
    }
  }
  const names = index.length > 0 ? index : ["index.html", "index.htm"];
  // A symlink to a file counts, as the web server follows it.
  const isFile = (e: FileEntry) =>
    e.type === FileType.FILE || (e.type === FileType.SYMLINK && e.linkType === FileType.FILE);
  const found = names.find((name) => entries.some((e) => e.name === name && isFile(e)));
  const mode = directory?.mode ?? "";
  if (mode !== "" && !openToAll(mode, true)) {
    return say("warning", t`Closed to the web server (${mode}), so it would answer 403.`);
  }
  // PHP reads its scripts as its own user, so only what the web server sends must be open.
  const shut =
    found && !found.endsWith(".php")
      ? entries.find((e) => e.name === found && e.type === FileType.FILE && !openToAll(e.mode, false))
      : undefined;
  if (shut) {
    const { name, mode: fileMode } = shut;
    return say("warning", t`The web server can't read ${name} (${fileMode}).`);
  }
  if (!found) {
    const first = names[0];
    return say("warning", t`No ${first} here yet, so it answers 404 until there is.`);
  }
  const others = entries.length - 1;
  return say(
    "ok",
    others > 0
      ? plural(others, { one: `Holds ${found} and # more`, other: `Holds ${found} and # more` })
      : t`Holds ${found}`,
  );
}
