// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { FileService, FileType, type WebsiteRuntime } from "@dockwell/api";
import { Callout, SegmentedControl, SelectField, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type Ref, useEffect, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { nameOf, parentOf } from "../files/paths";
import { canWrite } from "../websites/WebsiteFields";
import { toLabel } from "./app";
import type { FolderForm, FormErrors } from "./form";
import { APP_LANGUAGES, languageName, versionsOf } from "./runtime";

/** Who an app from a folder runs as unless it says, or the folder's owner says: the server's first user. */
export const DEFAULT_RUNTIME_USER = "1000:1000";

/** How long typing in the folder settles before the server is asked about it. */
const READ_AFTER_MS = 400;

/** What says a folder holds an app of each language, for its default commands to run. */
const MARKERS: Readonly<Record<string, readonly string[]>> = {
  node: ["package.json"],
  python: ["requirements.txt", "app.py"],
  go: ["go.mod"],
  java: ["mvnw", "app.jar"],
};

/** The name an app from a folder takes unless it's given one: the folder's own, as a label. */
export function nameFromFolder(folder: string): string {
  return toLabel(nameOf(folder.trim().replace(/\/+$/, "")));
}

/** Checks what a person can fix here, before asking the API. */
export function useFolderChecks(): (form: FolderForm) => Record<string, string> {
  const { t } = useLingui();
  return (form) => {
    const out: Record<string, string> = {};
    const folder = form.folder.trim();
    if (folder === "") out["folder"] = t`Give the folder on the server that holds the app, such as /srv/shop.`;
    else if (!folder.startsWith("/")) out["folder"] = t`Use a full path on the server, such as /srv/shop.`;
    else if (folder.replace(/\/+$/, "") === "") out["folder"] = t`Use a folder of its own, not the server's root.`;
    const port = form.port.trim();
    if (port !== "" && !(/^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65_535)) {
      out["port"] = t`Use a port from 1 to 65535.`;
    }
    const user = form.user.trim();
    if (user !== "") {
      const m = /^(\d+):(\d+)$/.exec(user);
      if (!m) out["user"] = t`Write it as uid:gid, such as 1000:1000.`;
      else if (m[1] === "0" || m[2] === "0") {
        out["user"] = t`An app from a folder never runs as root. Use the folder owner's uid:gid, such as 1000:1000.`;
      }
    }
    return out;
  };
}

/**
 * An app from a folder on the server (ADR 0058): the language and its version, the
 * folder, and how it installs, starts and listens, each the language's unless typed,
 * and who it runs as, the folder owner's once the server says.
 */
export function FolderFields({
  form,
  name,
  errors,
  runtimes,
  servers,
  update,
  fieldRef,
}: {
  form: FolderForm;
  /** What was typed as its name. */
  name: string;
  errors: FormErrors;
  runtimes: readonly WebsiteRuntime[] | undefined;
  /** The servers it runs on, to read the folder on each: none while there's no choice yet. */
  servers: readonly { id: string; name: string }[];
  update: (next: Partial<FolderForm> & { name?: string }, clear?: string) => void;
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
}) {
  const { t } = useLingui();
  const versions = versionsOf(form.language, runtimes);
  const chosen = versions.find((v) => v.version === form.version) ?? versions[0];
  const placeholderName = nameFromFolder(form.folder) || "web";
  return (
    <>
      <div className="app-field">
        <span className="dw-field-label">
          <Trans>Language</Trans>
        </span>
        <div>
          <SegmentedControl<string>
            label={t`Language`}
            value={form.language}
            onChange={(language) => update({ language, version: "" })}
            options={APP_LANGUAGES.map((id) => ({ value: id, label: languageName(id, runtimes) }))}
          />
        </div>
      </div>
      {versions.length > 1 ? (
        <SelectField
          label={t`Version`}
          value={chosen?.version ?? ""}
          options={versions.map((v) => ({ value: v.version, label: `${v.version} · ${v.release}` }))}
          hint={t`Changing it later is a plan of its own, which replaces the app's container.`}
          onChange={(event) => update({ version: event.target.value })}
        />
      ) : null}
      <TextField
        ref={fieldRef}
        label={t`Folder`}
        mono
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        placeholder="/srv/shop"
        value={form.folder}
        error={errors["folder"]}
        hint={t`On the server. It's mounted at /app, read-write, and what the app installs goes there.`}
        onChange={(event) => update({ folder: event.target.value }, "folder")}
      />
      <AppFolderNotes
        servers={servers}
        folder={form.folder}
        language={form.language}
        languageWord={chosen?.language ?? form.language}
        start={form.start.trim() || chosen?.start || ""}
        user={form.user.trim() || DEFAULT_RUNTIME_USER}
        onOwner={(owner) => {
          // What's typed stays: the note says when the app can't write there.
          if (form.user.trim() === "") update({ user: owner });
        }}
      />
      <TextField
        label={t`Name`}
        mono
        optional={name === ""}
        placeholder={placeholderName}
        value={name}
        error={errors["name"]}
        hint={t`Other apps on its network reach it by this name.`}
        onChange={(event) => update({ name: event.target.value }, "name")}
      />
      <TextField
        label={t`Installs with`}
        mono
        optional
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        placeholder={chosen?.install ?? ""}
        value={form.install}
        hint={t`Run in /app before each start, so what you change in the folder runs from the next restart. Write - to install nothing.`}
        onChange={(event) => update({ install: event.target.value })}
      />
      <TextField
        label={t`Starts with`}
        mono
        optional
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        placeholder={chosen?.start ?? ""}
        value={form.start}
        onChange={(event) => update({ start: event.target.value })}
      />
      <div className="app-folder-pair">
        <TextField
          label={t`Port`}
          mono
          optional
          inputMode="numeric"
          placeholder={chosen ? String(chosen.port) : ""}
          value={form.port}
          error={errors["port"]}
          hint={t`It's given as PORT, and its check and domain go there.`}
          onChange={(event) => update({ port: event.target.value }, "port")}
        />
        <TextField
          label={t`Runs as`}
          mono
          optional
          placeholder={DEFAULT_RUNTIME_USER}
          value={form.user}
          error={errors["user"]}
          hint={t`uid:gid, the folder owner's, so it can write there. Never root.`}
          onChange={(event) => update({ user: event.target.value }, "user")}
        />
      </div>
    </>
  );
}

/** Hears nothing, for a note whose owner isn't the one the form takes. */
const ignore = () => {};

/**
 * The folder's note for each server it runs on: one note where there's one server,
 * and one under each server's name where there are several, as the folder may be
 * there on one and not another. The first server's owner fills in who it runs as.
 */
function AppFolderNotes({
  servers,
  onOwner,
  ...note
}: Omit<Parameters<typeof AppFolderNote>[0], "server"> & { servers: readonly { id: string; name: string }[] }) {
  if (servers.length <= 1) return <AppFolderNote {...note} server={servers[0]?.id ?? ""} onOwner={onOwner} />;
  return (
    <div className="app-folder-notes">
      {servers.map((s, i) => (
        <div key={s.id} className="app-folder-on">
          <span className="code-sm app-muted">{s.name}</span>
          <AppFolderNote {...note} server={s.id} onOwner={i === 0 ? onOwner : ignore} />
        </div>
      ))}
    </div>
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
 * What an app's folder holds, as the server's file browser reads it, under its field:
 * that it isn't there yet and is made as the plan applies, that its user can't write
 * there, that it has nothing its language's commands would run, or what it holds. It
 * reads the folder above first, so a folder that isn't there yet isn't a refusal on
 * Activity. Reading the disk is a person's, as Files is, so this is too.
 */
function AppFolderNote({
  server,
  folder,
  language,
  languageWord,
  start,
  user,
  onOwner,
}: {
  server: string;
  folder: string;
  language: string;
  languageWord: string;
  start: string;
  user: string;
  /** Hears the owner of a folder that's there, unless that's root, as "uid:gid". */
  onOwner: (owner: string) => void;
}) {
  const { t } = useLingui();
  const path = useSettled(folder.trim().replace(/\/+$/, ""), READ_AFTER_MS);
  const asked = server !== "" && path.startsWith("/") && path.length > 1 && !/\/\.\.?(\/|$)/.test(path);
  const above = useQuery(
    FileService.method.listFiles,
    { serverId: server, path: parentOf(path), lookup: true },
    { enabled: asked, retry: false, refetchOnWindowFocus: false },
  );
  const there = above.data?.entries.find((e) => e.name === nameOf(path));
  const isFolder =
    there !== undefined &&
    (there.type === FileType.DIRECTORY || (there.type === FileType.SYMLINK && there.linkType === FileType.DIRECTORY));
  const listed = useQuery(
    FileService.method.listFiles,
    { serverId: server, path, lookup: true },
    { enabled: asked && isFolder, retry: false, refetchOnWindowFocus: false },
  );
  const aboveMissing = above.error !== null && reasonOf(above.error) === "not_found";
  const dir = listed.data?.directory;
  const owner = dir && dir.uid !== 0 && dir.gid !== 0 ? `${dir.uid}:${dir.gid}` : "";
  // Said once as it's found, so what someone types after stays theirs.
  const told = useRef("");
  useEffect(() => {
    if (!owner || told.current === owner) return;
    told.current = owner;
    onOwner(owner);
  }, [owner, onOwner]);
  if (!asked || above.isPending || (above.error && !aboveMissing)) return null;
  if (aboveMissing || !there) {
    return (
      <Callout tone="info" className="app-folder-note">
        <Trans>
          Nothing is at <code>{path}</code> yet. Dockwell makes it as the plan applies, owned by <code>{user}</code>,
          the user the app runs as. Put the app's files in it with <code>sudo rsync</code>, then{" "}
          <code>
            sudo chown -R {user} {path}
          </code>
          , and restart the app.
        </Trans>
      </Callout>
    );
  }
  if (!isFolder) {
    return (
      <Callout tone="warning" className="app-folder-note">
        <Trans>
          <code>{path}</code> is a file, not a folder. Give the folder that holds the app.
        </Trans>
      </Callout>
    );
  }
  if (listed.isPending) return null;
  if (listed.error) {
    return (
      <Callout tone="warning" className="app-folder-note">
        {sentence(listed.error)}
      </Callout>
    );
  }
  const { entries } = listed.data;
  if (dir && (dir.uid === 0 || dir.gid === 0)) {
    return (
      <Callout tone="warning" className="app-folder-note">
        <Trans>
          <code>{path}</code> is root's, and an app from a folder never runs as root, so it can't install there. Run{" "}
          <code>
            sudo chown -R {user} {path}
          </code>
          , or give the folder to the user it runs as.
        </Trans>
      </Callout>
    );
  }
  if (dir && !canWrite(user, dir.mode, dir.uid, dir.gid)) {
    const theirs = `${dir.uid}:${dir.gid}`;
    // Its own folder, closed to itself: the mode is what to change.
    if (theirs === user) {
      return (
        <Callout tone="warning" className="app-folder-note">
          <Trans>
            <code>{path}</code> is <code>{theirs}</code>'s, whom the app runs as, but <code>{dir.mode}</code>, so it
            can't install there. Run <code>chmod u+w {path}</code>.
          </Trans>
        </Callout>
      );
    }
    return (
      <Callout tone="warning" className="app-folder-note">
        <Trans>
          The app runs as <code>{user}</code>, and <code>{path}</code> is <code>{theirs}</code>'s,{" "}
          <code>{dir.mode}</code>, so it can't install there. Run it as <code>{theirs}</code>, or run{" "}
          <code>
            sudo chown -R {user} {path}
          </code>
          .
        </Trans>
      </Callout>
    );
  }
  const markers = MARKERS[language] ?? [];
  const found = markers.find((m) => entries.some((e) => e.name === m));
  if (!found && markers.length > 0) {
    const first = markers[0];
    return (
      <Callout tone="warning" className="app-folder-note">
        <Trans>
          There's no <code>{first}</code> in <code>{path}</code>, so <code>{start}</code> has nothing of {languageWord}
          's to run. Put the app's files there, or say below how it installs and starts.
        </Trans>
      </Callout>
    );
  }
  const others = entries.length - 1;
  const files = (
    <Link to="/servers/$serverId/files" params={{ serverId: server }} search={{ path }}>
      <Trans>Open in Files</Trans>
    </Link>
  );
  return (
    <p className="app-folder-holds">
      {found && others > 0
        ? plural(others, { one: `Holds ${found} and # more.`, other: `Holds ${found} and # more.` })
        : found
          ? t`Holds ${found}.`
          : null}{" "}
      {files}
    </p>
  );
}
