// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { FileService, FileType } from "@dockwell/api";
import { Callout, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { nameOf, parentOf } from "../files/paths";
import { indexOf, type Missing, type Serves, type Settings } from "./settings";

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
    }
  };
}

/** What each kind of website is, in the words its forms choose it by. */
export function useServesWords(): Record<Serves, string> {
  const { t } = useLingui();
  return { folder: t`Files in a folder`, url: t`A URL`, port: t`A port on this server` };
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

/** Whether everyone may read an entry, as ls's mode says: a folder needs x too, to be walked. */
export function openToAll(mode: string, folder: boolean): boolean {
  const others = mode.slice(-3);
  return others[0] === "r" && (!folder || others[2] === "x" || others[2] === "t");
}

/**
 * The fields of what a website serves, for the kind its settings say: a folder, a URL
 * or a port on the server. New website and a website's configuration both have them,
 * and was, where there is one, says what each field had.
 */
export function WebsiteFields({
  settings: s,
  onChange,
  was,
  errors,
  server,
}: {
  settings: Settings;
  onChange: (next: Settings) => void;
  was?: Settings;
  errors: { url?: string | undefined; port?: string | undefined; root?: string | undefined };
  /** The server it runs on, whose files a folder's note reads. */
  server: string;
}) {
  const { t } = useLingui();
  const same = was?.serves === s.serves;
  if (s.serves === "folder") {
    return (
      <>
        <TextField
          label={t`Folder`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder="/var/www/blog"
          value={s.root}
          error={errors.root}
          hint={
            same && was && s.root.trim() !== was.root ? (
              <WasHint>{t`Was ${was.root}`}</WasHint>
            ) : (
              t`On the server. Its web server reads it as nobody's user, so what's in it must be readable by everyone.`
            )
          }
          onChange={(event) => onChange({ ...s, root: event.target.value })}
        />
        <FolderNote
          server={server}
          root={s.root}
          index={indexOf(s.index)}
          onOwner={(owner) => {
            if (owner !== s.owner) onChange({ ...s, owner });
          }}
        />
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
              t`The first of these in a folder answers for it.`
            )
          }
          onChange={(event) => onChange({ ...s, index: event.target.value })}
        />
        <Switch
          label={t`Single-page app`}
          description={t`Answers a path with no file of its own with the index, as an app that routes in the browser needs. Off, it gets a 404.`}
          checked={s.singlePageApp}
          onChange={(on) => onChange({ ...s, singlePageApp: on })}
        />
      </>
    );
  }
  if (s.serves === "url") {
    return (
      <>
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
          hint={
            same && was && s.url.trim() !== was.url ? (
              <WasHint>{t`Was ${was.url}`}</WasHint>
            ) : (
              t`With a path, such as /app, each request goes under it.`
            )
          }
          onChange={(event) => onChange({ ...s, url: event.target.value })}
        />
        <Switch
          label={t`Send the URL's own name as Host`}
          description={t`For a site that serves several names, such as another site over HTTPS. Off, it gets this website's domain.`}
          checked={s.targetHost}
          onChange={(on) => onChange({ ...s, targetHost: on })}
        />
      </>
    );
  }
  return (
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
          t`What listens on the server itself, on 127.0.0.1 or every address. It sees every visitor as local, so you approve it in the plan.`
        )
      }
      onChange={(event) => onChange({ ...s, port: event.target.value })}
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
  index,
  onOwner,
}: {
  server: string;
  root: string;
  index: readonly string[];
  /** Hears who the folder is made for when it isn't there: the folder above's owner, or no one. */
  onOwner?: (owner: string) => void;
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
  useEffect(() => {
    if (missing) onOwner?.(owner);
  }, [missing, owner, onOwner]);
  if (!asked || above.isPending || (above.error && !aboveMissing)) return null;
  const files = (
    <Link to="/servers/$serverId/files" params={{ serverId: server }} search={{ path }}>
      <Trans>Open in Files</Trans>
    </Link>
  );
  if (aboveMissing || !there) {
    const up = parentOf(path);
    return (
      <Callout tone="info" className="website-folder-note">
        {owner ? (
          <Trans>
            Nothing is at <code>{path}</code> yet. Dockwell makes it as the plan applies, owned by{" "}
            <code>{parent?.owner || owner}</code> as <code>{up}</code> is, so put the site's files in it as that user,
            with scp or rsync.
          </Trans>
        ) : (
          <Trans>
            Nothing is at <code>{path}</code> or <code>{up}</code> yet, so the folder would be made root's, and no one
            else could put the site's files in it. Make it first, as yours, with{" "}
            <code>sudo install -d -o $(id -u) -g $(id -g) {path}</code>.
          </Trans>
        )}
      </Callout>
    );
  }
  if (!folder) {
    return (
      <Callout tone="warning" className="website-folder-note">
        <Trans>
          <code>{path}</code> is a file, not a folder. Give the folder that holds the site's files.
        </Trans>
      </Callout>
    );
  }
  if (listed.isPending) return null;
  if (listed.error) {
    return (
      <Callout tone="warning" className="website-folder-note">
        {sentence(listed.error)}
      </Callout>
    );
  }
  const { entries, directory } = listed.data;
  // Files in a folder run no script, and hide them, so a PHP site's folder answers 404 for them.
  if (entries.some((e) => e.name === "index.php" && e.type === FileType.FILE)) {
    return (
      <Callout tone="warning" className="website-folder-note">
        <Trans>
          <code>{path}</code> holds <code>index.php</code>. Files in a folder run no script, and hide every one, as its
          source can hold passwords, so those addresses answer 404.
        </Trans>
      </Callout>
    );
  }
  const names = index.length > 0 ? index : ["index.html", "index.htm"];
  const found = names.find((name) => entries.some((e) => e.name === name && e.type === FileType.FILE));
  const mode = directory?.mode ?? "";
  if (mode !== "" && !openToAll(mode, true)) {
    return (
      <Callout tone="warning" className="website-folder-note">
        <Trans>
          The web server reads the folder as nobody's user, and <code>{path}</code> is <code>{mode}</code>, closed to
          everyone else, so it answers 403. Run <code>chmod o+rx {path}</code>, and <code>o+r</code> on its files.
        </Trans>
      </Callout>
    );
  }
  const shut = found ? entries.find((e) => e.name === found && !openToAll(e.mode, false)) : undefined;
  if (shut) {
    return (
      <Callout tone="warning" className="website-folder-note">
        <Trans>
          <code>{shut.name}</code> is <code>{shut.mode}</code>, closed to everyone but its owner, so the web server
          can't read it. Run <code>chmod o+r</code> on it, and on the folder's other files.
        </Trans>
      </Callout>
    );
  }
  if (!found) {
    const first = names[0];
    return (
      <Callout tone="warning" className="website-folder-note">
        <Trans>
          There's no <code>{first}</code> in <code>{path}</code>, so its address answers 404 until there is. Add one, or
          name the index the site has.
        </Trans>
      </Callout>
    );
  }
  const others = entries.length - 1;
  return (
    <p className="website-folder-holds">
      {others > 0
        ? plural(others, { one: `Holds ${found} and # more.`, other: `Holds ${found} and # more.` })
        : t`Holds ${found}.`}{" "}
      {files}
    </p>
  );
}
