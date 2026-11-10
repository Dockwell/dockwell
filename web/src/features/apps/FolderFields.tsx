// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { FileService, FileType, type WebsiteRuntime } from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  FolderPicker,
  PortField,
  SegmentedControl,
  SelectField,
  TextField,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, type Ref, useCallback, useEffect, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { nameOf, parentOf } from "../files/paths";
import { canWrite } from "../websites/WebsiteFields";
import { toLabel } from "./app";
import { useServerFolders } from "./folders";
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

/** The file that names the version an app wants, for each language, where its folder has one. */
const VERSION_FILES: Readonly<Record<string, readonly string[]>> = {
  node: [".nvmrc", ".node-version"],
  python: [".python-version"],
  go: ["go.mod"],
  java: [".java-version"],
};

/** Where an app's folder goes unless it's typed: /srv/<name>, which Dockwell makes. */
export function defaultFolder(name: string): string {
  const label = toLabel(name.trim());
  return label ? `/srv/${label}` : "";
}

/** The version a version file asks for, as Dockwell names its versions: "24" of v24.3.0, "3.13" of 3.13.1. */
export function versionIn(language: string, file: string, content: string): string {
  const text = content.trim();
  switch (language) {
    case "node":
    case "java":
      return /^v?(\d+)/.exec(text)?.[1] ?? "";
    case "python":
      return /^(\d+\.\d+)/.exec(text)?.[1] ?? "";
    case "go":
      return file === "go.mod" ? (/^go\s+(\d+\.\d+)/m.exec(text)?.[1] ?? "") : "";
    default:
      return "";
  }
}

/** The file that says the port an app listens on, where it sets one of its own, for each language. */
const PORT_FILES: Readonly<Record<string, string>> = { node: "package.json", python: "app.py" };

/**
 * The port an app's own files say it listens on, whatever PORT says: a start script's
 * --port, -p or PORT=, or app.py's port=. Zero where they say none.
 */
export function portIn(language: string, content: string): number {
  let text = content;
  if (language === "node") {
    try {
      text = String((JSON.parse(content) as { scripts?: { start?: unknown } }).scripts?.start ?? "");
    } catch {
      return 0;
    }
  }
  const found =
    language === "node"
      ? /(?:--port[ =]|-p\s+|\bPORT=)(\d{2,5})\b/.exec(text)
      : language === "python"
        ? /\bport\s*=\s*(\d{2,5})\b/.exec(text)
        : null;
  const port = Number(found?.[1] ?? 0);
  return port >= 1 && port <= 65_535 ? port : 0;
}

/** What a folder's files say it runs on: the language, the version where a file names one, and the files that said so. */
interface Detected {
  language: string;
  version: string;
  from: readonly string[];
  /** The port its files say it listens on, or 0. */
  port: number;
}

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
    if (folder === "") out["folder"] = t`Name the app, or give its folder on the server, such as /srv/shop.`;
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
        out["user"] = t`An app never runs as root. Use the folder owner's uid:gid, such as 1000:1000.`;
      }
    }
    return out;
  };
}

/**
 * An app from a folder on the server (ADR 0058). It asks for a name, and decides the
 * rest: the folder follows the name until it's typed, and is checked on the server as
 * it's typed; the language and version are what the folder's files say; and how it
 * installs, starts, listens and who it runs as are the language's and the folder
 * owner's, under Advanced.
 */
export function FolderFields({
  form,
  name,
  errors,
  runtimes,
  servers,
  asked = false,
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
  /** True when the form opened on a language and version someone chose, which a folder's files don't change. */
  asked?: boolean;
  update: (next: Partial<FolderForm> & { name?: string }, clear?: string) => void;
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
}) {
  const { t } = useLingui();
  const versions = versionsOf(form.language, runtimes);
  const chosen = versions.find((v) => v.version === form.version) ?? versions[0];
  const placeholderName = nameFromFolder(form.folder) || "shop";
  // The folder follows the name until someone types one.
  const [folderTyped, setFolderTyped] = useState(false);
  // The language and version are the folder's until someone picks them.
  const [picked, setPicked] = useState(asked);
  const [changing, setChanging] = useState(false);
  const [detected, setDetected] = useState<Detected>();
  const [advanced, setAdvanced] = useState(false);
  // The port is what the folder's files say until someone types one.
  const [portTyped, setPortTyped] = useState(false);
  // A single server's folders to pick from, where its reader may list them.
  const folders = useServerFolders(servers.length === 1 ? servers[0]?.id : undefined);
  const onDetected = useCallback(
    (found: Detected | undefined) => {
      setDetected(found);
      if (!found || picked) return;
      const offered = versionsOf(found.language, runtimes);
      const version = offered.some((v) => v.version === found.version) ? found.version : "";
      update({ language: found.language, version, ...(found.port && !portTyped ? { port: String(found.port) } : {}) });
    },
    [picked, portTyped, runtimes, update],
  );
  const user = form.user.trim() || DEFAULT_RUNTIME_USER;
  const start = form.start.trim() || chosen?.start || "";
  const port = form.port.trim() || (chosen ? String(chosen.port) : "");
  const follows = detected !== undefined && !picked && !changing && detected.language === form.language;
  const words = chosen ? `${chosen.language} ${chosen.version}` : languageName(form.language, runtimes);
  const from = detected?.from.join(", ") ?? "";
  const pickers = (
    <>
      <div className="app-field">
        <span className="dw-field-label">
          <Trans>Language</Trans>
        </span>
        <div>
          <SegmentedControl<string>
            label={t`Language`}
            value={form.language}
            onChange={(language) => {
              setPicked(true);
              update({ language, version: "" });
            }}
            options={APP_LANGUAGES.map((id) => ({ value: id, label: languageName(id, runtimes) }))}
          />
        </div>
      </div>
      {versions.length > 1 ? (
        <SelectField
          label={t`Version`}
          value={chosen?.version ?? ""}
          options={versions.map((v) => ({ value: v.version, label: `${v.version} · ${v.release}` }))}
          onChange={(event) => {
            setPicked(true);
            update({ version: event.target.value });
          }}
        />
      ) : null}
    </>
  );
  return (
    <>
      <TextField
        ref={fieldRef}
        label={t`Name`}
        mono
        autoCapitalize="none"
        autoCorrect="off"
        spellCheck={false}
        placeholder={placeholderName}
        value={name}
        error={errors["name"]}
        onChange={(event) => {
          const typed = event.target.value;
          update({ name: typed, ...(folderTyped ? {} : { folder: defaultFolder(typed) }) }, "name");
        }}
      />
      <AppFolderNotes
        servers={servers}
        folder={form.folder}
        language={form.language}
        languageWord={chosen?.language ?? form.language}
        user={user}
        onOwner={(owner) => {
          // What's typed stays: the note says when the app can't write there.
          if (form.user.trim() === "") update({ user: owner });
        }}
        onRunAs={(owner) => update({ user: owner }, "user")}
        onDetected={onDetected}
      >
        {(note) =>
          folders ? (
            <FolderPicker
              label={t`Folder`}
              value={form.folder}
              list={folders}
              note={note}
              error={errors["folder"]}
              onChange={(path) => {
                setFolderTyped(path !== "");
                update({ folder: path }, "folder");
              }}
            />
          ) : (
            <TextField
              label={t`Folder`}
              mono
              autoCapitalize="none"
              autoCorrect="off"
              spellCheck={false}
              placeholder="/srv/shop"
              value={form.folder}
              error={errors["folder"]}
              hint={note}
              onChange={(event) => {
                setFolderTyped(event.target.value !== "");
                update({ folder: event.target.value }, "folder");
              }}
            />
          )
        }
      </AppFolderNotes>
      {follows ? (
        <div className="app-field">
          <span className="dw-field-label">
            <Trans>Runtime</Trans>
          </span>
          <div className="app-folder-runtime">
            <span>
              <strong>{words}</strong>
              <span className="app-muted">
                {" · "}
                <Trans>from {from}</Trans>
              </span>
            </span>
            <Button size="sm" variant="ghost" onClick={() => setChanging(true)}>
              <Trans>Change</Trans>
            </Button>
          </div>
        </div>
      ) : (
        pickers
      )}
      <Advanced
        open={advanced || errors["port"] !== undefined || errors["user"] !== undefined}
        onOpenChange={setAdvanced}
        summary={
          <>
            <code>{start}</code>
            {" · "}
            <Trans>port {port}</Trans>
            {" · "}
            <Trans>as {user}</Trans>
          </>
        }
      >
        <TextField
          label={t`Installs with`}
          mono
          optional
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder={chosen?.install ?? ""}
          value={form.install}
          hint={t`Runs before each start. Write - to install nothing.`}
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
          <PortField
            label={t`Port`}
            optional
            placeholder={chosen ? String(chosen.port) : ""}
            value={form.port}
            error={errors["port"]}
            hint={
              detected?.port && String(detected.port) === form.port.trim()
                ? t`From ${PORT_FILES[detected.language] ?? ""}. The app gets it as PORT.`
                : t`The app gets it as PORT.`
            }
            onChange={(value) => {
              setPortTyped(value !== "");
              update({ port: value }, "port");
            }}
          />
          <TextField
            label={t`Runs as`}
            mono
            optional
            placeholder={DEFAULT_RUNTIME_USER}
            value={form.user}
            error={errors["user"]}
            hint={t`uid:gid of the folder's owner.`}
            onChange={(event) => update({ user: event.target.value }, "user")}
          />
        </div>
      </Advanced>
    </>
  );
}

/** Hears nothing, for a note whose owner isn't the one the form takes. */
const ignore = () => {};

/**
 * The folder's field with what each server it runs on says of it. With one server,
 * that is the field's own line, and anything that stops the app there under it. With
 * several, the field stands alone and each server's line is under its name, as the
 * folder may be there on one and not another. The first server's owner fills in who
 * it runs as, and its files what it runs on.
 */
function AppFolderNotes({
  servers,
  onOwner,
  onDetected,
  children,
  ...note
}: Omit<Parameters<typeof useFolderState>[0], "server"> & {
  servers: readonly { id: string; name: string }[];
  children: (note: ReactNode) => ReactNode;
}) {
  const one = servers.length <= 1;
  const first = useFolderState({ ...note, server: one ? (servers[0]?.id ?? "") : "", onOwner, onDetected });
  if (one) {
    return (
      <>
        {children(first.note)}
        {first.problem}
      </>
    );
  }
  return (
    <>
      {children(undefined)}
      <div className="app-folder-notes">
        {servers.map((s, i) => (
          <div key={s.id} className="app-folder-on">
            <span className="code-sm app-muted">{s.name}</span>
            <ServerFolderNote
              {...note}
              server={s.id}
              onOwner={i === 0 ? onOwner : ignore}
              onDetected={i === 0 ? onDetected : ignore}
            />
          </div>
        ))}
      </div>
    </>
  );
}

/** What one of several servers says of the folder: its line, or what stops the app there. */
function ServerFolderNote(props: Parameters<typeof useFolderState>[0]) {
  const { note, problem } = useFolderState(props);
  return problem ?? (note ? <p className="app-folder-holds">{note}</p> : null);
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
 * What the server says of an app's folder: a short line for its field (new, and
 * Dockwell makes it; or what it holds), or what stops the app there, with what puts
 * it right. It reads the folder above first, so a folder that isn't there yet isn't a
 * refusal on Activity, and says what the folder's files run on (onDetected).
 * Reading the disk is a person's, as Files is, so this is too.
 */
function useFolderState({
  server,
  folder,
  language,
  languageWord,
  user,
  onOwner,
  onRunAs,
  onDetected,
}: {
  server: string;
  folder: string;
  language: string;
  languageWord: string;
  user: string;
  /** Hears the owner of a folder that's there, unless that's root, as "uid:gid". */
  onOwner: (owner: string) => void;
  /** Runs the app as the folder's owner, when someone asks. */
  onRunAs: (owner: string) => void;
  /** Hears what the folder's files run on, or undefined once they say nothing. */
  onDetected: (found: Detected | undefined) => void;
}): { note?: ReactNode; problem?: ReactNode } {
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
  // What its files run on: the first language with a file of its own there, and the
  // version a version file names.
  const names = listed.data?.entries.map((e) => e.name);
  const itsLanguage = names
    ? APP_LANGUAGES.find((id) => (MARKERS[id] ?? []).some((m) => names.includes(m)))
    : undefined;
  const itsMarker = itsLanguage ? (MARKERS[itsLanguage] ?? []).find((m) => names?.includes(m)) : undefined;
  const versionFile = itsLanguage ? (VERSION_FILES[itsLanguage] ?? []).find((f) => names?.includes(f)) : undefined;
  const read = useQuery(
    FileService.method.readFile,
    { serverId: server, path: `${path}/${versionFile ?? ""}` },
    { enabled: asked && versionFile !== undefined, retry: false, refetchOnWindowFocus: false },
  );
  const itsVersion =
    itsLanguage && versionFile && read.data ? versionIn(itsLanguage, versionFile, read.data.content) : "";
  const portFile = itsLanguage ? PORT_FILES[itsLanguage] : undefined;
  const hasPortFile = portFile !== undefined && names?.includes(portFile) === true;
  const readPort = useQuery(
    FileService.method.readFile,
    { serverId: server, path: `${path}/${portFile ?? ""}` },
    { enabled: asked && hasPortFile, retry: false, refetchOnWindowFocus: false },
  );
  const itsPort = itsLanguage && readPort.data ? portIn(itsLanguage, readPort.data.content) : 0;
  const waiting = (versionFile !== undefined && read.isPending) || (hasPortFile && readPort.isPending);
  const said = useRef("");
  useEffect(() => {
    if (waiting) return;
    const key = itsLanguage ? `${path}|${itsLanguage}|${itsVersion}|${itsPort}` : "";
    if (said.current === key) return;
    said.current = key;
    onDetected(
      itsLanguage && itsMarker
        ? {
            language: itsLanguage,
            version: itsVersion,
            from: itsVersion && versionFile && versionFile !== itsMarker ? [itsMarker, versionFile] : [itsMarker],
            port: itsPort,
          }
        : undefined,
    );
  }, [waiting, path, itsLanguage, itsMarker, itsVersion, itsPort, versionFile, onDetected]);
  if (!asked || above.isPending || (above.error && !aboveMissing)) return {};
  if (aboveMissing || !there) {
    return {
      note: (
        <>
          <span className="app-folder-state">{t({ message: "New", context: "a folder that isn't there yet" })}</span>{" "}
          <Trans>
            Dockwell makes it, owned by <code>{user}</code>
          </Trans>
        </>
      ),
    };
  }
  if (!isFolder) {
    return {
      problem: (
        <Callout tone="warning" className="app-folder-note" title={t`${path} is a file`}>
          <Trans>Choose the folder that holds the app.</Trans>
        </Callout>
      ),
    };
  }
  if (listed.isPending) return {};
  if (listed.error) {
    return {
      problem: (
        <Callout tone="warning" className="app-folder-note">
          {sentence(listed.error)}
        </Callout>
      ),
    };
  }
  const { entries } = listed.data;
  if (dir && (dir.uid === 0 || dir.gid === 0)) {
    return {
      problem: (
        <Callout tone="warning" className="app-folder-note" title={t`${path} is root's, and an app never runs as root`}>
          <Trans>
            Run{" "}
            <code>
              sudo chown -R {user} {path}
            </code>{" "}
            on the server.
          </Trans>
        </Callout>
      ),
    };
  }
  if (dir && !canWrite(user, dir.mode, dir.uid, dir.gid)) {
    const theirs = `${dir.uid}:${dir.gid}`;
    // Its own folder, closed to itself: the mode is what to change.
    if (theirs === user) {
      return {
        problem: (
          <Callout tone="warning" className="app-folder-note" title={t`The app can't write to ${path}`}>
            <Trans>
              Its mode is <code>{dir.mode}</code>. Run <code>chmod u+w {path}</code> on the server.
            </Trans>
          </Callout>
        ),
      };
    }
    return {
      problem: (
        <Callout
          tone="warning"
          className="app-folder-note"
          title={t`The app can't write to ${path}`}
          actions={
            <Button size="sm" onClick={() => onRunAs(theirs)}>
              <Trans>Run as {theirs}</Trans>
            </Button>
          }
        >
          <Trans>
            It's <code>{theirs}</code>'s, and the app runs as <code>{user}</code>.
          </Trans>
        </Callout>
      ),
    };
  }
  const markers = MARKERS[language] ?? [];
  const found = markers.find((m) => entries.some((e) => e.name === m));
  const files = (
    <Link to="/servers/$serverId/files" params={{ serverId: server }} search={{ path }}>
      <Trans>Open in Files</Trans>
    </Link>
  );
  if (!found && markers.length > 0) {
    const first = markers[0];
    return {
      problem: (
        <Callout tone="warning" className="app-folder-note" title={t`No ${languageWord} app in ${path} yet`}>
          <Trans>
            It has no <code>{first}</code>. Put the app's files there.
          </Trans>{" "}
          {files}
        </Callout>
      ),
    };
  }
  const others = entries.length - 1;
  return {
    note: (
      <>
        {found && others > 0
          ? plural(others, { one: `Holds ${found} and # more`, other: `Holds ${found} and # more` })
          : found
            ? t`Holds ${found}`
            : null}
        {" · "}
        {files}
      </>
    ),
  };
}
