// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type FileEntry, FileService, type ReadFileResponse } from "@dockwell/api";
import { Button, Callout, CodeField, Icon, Panel, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useBlocker, useNavigate } from "@tanstack/react-router";
import { type KeyboardEvent, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { changedLines, editable, lineCount, lineEnding, nameOf, restoreEndings } from "./paths";

interface FileViewerProps {
  serverId: string;
  path: string;
  /** The folder on show, which closing the file goes back to. */
  folder: string;
}

/**
 * A file, as the server has it. Reading one goes on record, so it reads once each
 * time the file opens: never on its own when the window takes focus, and nothing
 * of it stays in the cache once it closes. What it shows is text in a field,
 * whatever the file's name says: nothing in a file runs here.
 */
export function FileViewer({ serverId, path, folder }: FileViewerProps) {
  const { t, i18n } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const navigate = useNavigate();
  const query = useQuery(
    FileService.method.readFile,
    { serverId, path },
    { retry: false, gcTime: 0, staleTime: Number.POSITIVE_INFINITY, refetchOnWindowFocus: false },
  );
  const name = nameOf(path);
  const read = query.data;
  const [saved, setSaved] = useState<FileEntry>();
  const [edited, setEdited] = useState(false);
  const file = saved ?? read?.file;
  const close = () => {
    void navigate({ to: "/servers/$serverId/files", params: { serverId }, search: { path: folder } });
  };
  const size = file ? formatBytes(file.sizeBytes, i18n.locale) : "";
  const body = () => {
    if (query.error) {
      const reason = reasonOf(query.error);
      const refused = reason === "file_refused";
      const title = refused
        ? t`Dockwell doesn't open this file`
        : reason === "path_not_answering"
          ? t`This path isn't answering`
          : t`Can't read this file`;
      return (
        <div className="files-note">
          <Callout tone={refused ? "warning" : "critical"} title={title}>
            {sentence(query.error)}
          </Callout>
        </div>
      );
    }
    if (!read) {
      return (
        <p className="files-reading" aria-busy="true">
          <Trans>Reading {name}</Trans>
        </p>
      );
    }
    if (read.binary) {
      return (
        <div className="files-note">
          <Callout tone="info" title={t`This isn't a text file`}>
            <Trans>
              It's {size}, and it isn't UTF-8 text, so there's nothing to show here. Connect to the server over SSH to
              work with it.
            </Trans>
          </Callout>
        </div>
      );
    }
    if (read.truncated) {
      return (
        <>
          <div className="files-note">
            <Callout tone="warning" title={t`Showing the first 1 MB`}>
              <Trans>
                This file is {size}, so the rest of it stays on the server, and it can't be edited here. Connect to the
                server over SSH to work with it all.
              </Trans>
            </Callout>
          </div>
          <CodeField className="files-code" aria-label={t`Contents of ${name}`} value={read.content} readOnly />
        </>
      );
    }
    return <Editor serverId={serverId} path={path} name={name} read={read} onSaved={setSaved} onEdited={setEdited} />;
  };
  return (
    <Panel
      flush
      className="files-viewer"
      title={
        phone ? undefined : (
          <span className="files-viewer-title" title={path}>
            <span className="files-icon">
              <Icon name="file" size={16} />
            </span>
            <span className="files-viewer-name">{name}</span>
            {edited ? (
              <span className="files-edited">
                <Trans>Edited</Trans>
              </span>
            ) : null}
          </span>
        )
      }
      actions={
        phone ? undefined : <Button variant="ghost" size="sm" icon="x" aria-label={t`Close ${name}`} onClick={close} />
      }
    >
      {file ? (
        <p className="files-facts">
          <code>{file.owner}</code>
          <code>{file.mode}</code>
          <span>{size}</span>
        </p>
      ) : null}
      {body()}
    </Panel>
  );
}

interface EditorProps {
  serverId: string;
  path: string;
  name: string;
  read: ReadFileResponse;
  onSaved: (file: FileEntry | undefined) => void;
  onEdited: (edited: boolean) => void;
}

/** What the server has, as the field shows it, and what the next save goes over. */
interface Base {
  text: string;
  version: string;
  crlf: boolean;
}

function baseOf(read: ReadFileResponse): Base {
  return { text: editable(read.content), version: read.version, crlf: lineEnding(read.content) === "CRLF" };
}

/**
 * A file's text, to edit and save. A save names the version it was edited from,
 * so a file that changed since isn't written over without you saying so. What you
 * typed stays in the field whatever a save comes to, and leaving with it unsaved
 * asks first.
 */
function Editor({ serverId, path, name, read, onSaved, onEdited }: EditorProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const queryClient = useQueryClient();
  const [base, setBase] = useState(() => baseOf(read));
  const [draft, setDraft] = useState(base.text);
  const [saving, setSaving] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [problem, setProblem] = useState<string>();
  const [justSaved, setJustSaved] = useState(false);
  const edited = draft !== base.text;
  const changes = edited ? changedLines(base.text, draft) : 0;

  useBlocker({
    shouldBlockFn: () => !window.confirm(t`Leave ${name} without saving? What you typed goes.`),
    disabled: !edited,
    enableBeforeUnload: edited,
  });

  const client = createClient(FileService, transport);
  const change = (next: string) => {
    setDraft(next);
    setJustSaved(false);
    onEdited(next !== base.text);
  };

  const discard = () => {
    change(base.text);
    setConflict(false);
    setProblem(undefined);
  };

  const save = async (over: string) => {
    const sent = draft;
    const content = restoreEndings(sent, base.crlf);
    setSaving(true);
    setProblem(undefined);
    setConflict(false);
    setJustSaved(false);
    try {
      const res = await client.saveFile({ serverId, path, content, version: over });
      setBase((was) => ({ ...was, text: sent, version: res.version }));
      setJustSaved(true);
      onSaved(res.file);
      onEdited(false);
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: FileService.method.listFiles, cardinality: "finite" }),
      });
    } catch (err) {
      if (reasonOf(err) === "file_changed") setConflict(true);
      else setProblem(sentence(err));
    } finally {
      setSaving(false);
    }
  };

  /** What's on the server now, for a person who has seen the file changed. */
  const reread = () => client.readFile({ serverId, path });

  const saveOver = async () => {
    try {
      const now = await reread();
      if (!now.version) {
        setConflict(false);
        setProblem(t`${name} isn't a text file under 1 MB any more, so it can't be saved here.`);
        return;
      }
      await save(now.version);
    } catch (err) {
      setProblem(sentence(err));
    }
  };

  const loadServers = async () => {
    try {
      const now = await reread();
      if (!now.version) {
        setConflict(false);
        setProblem(t`${name} isn't a text file under 1 MB any more, so it can't be loaded here.`);
        return;
      }
      const next = baseOf(now);
      setBase(next);
      setDraft(next.text);
      setConflict(false);
      setProblem(undefined);
      onSaved(now.file);
      onEdited(false);
    } catch (err) {
      setProblem(sentence(err));
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
      event.preventDefault();
      if (edited && !saving && !conflict) void save(base.version);
    }
  };

  return (
    <>
      {conflict ? (
        <div className="files-note">
          <Callout tone="warning" title={t`This file changed on the server`}>
            <Trans>
              {name} changed after you opened it, so nothing was saved. What you typed is still here. Save over the
              server's version, or load it in place of yours.
            </Trans>
            <span className="files-note-actions">
              <Button size="sm" disabled={saving} onClick={() => void saveOver()}>
                <Trans>Save over it</Trans>
              </Button>
              <Button size="sm" variant="ghost" disabled={saving} onClick={() => void loadServers()}>
                <Trans>Load the server's version</Trans>
              </Button>
            </span>
          </Callout>
        </div>
      ) : null}
      {problem ? (
        <div className="files-note">
          <Callout tone="critical" title={t`Can't save this file`}>
            {problem} <Trans>What you typed is still here.</Trans>
          </Callout>
        </div>
      ) : null}
      <CodeField
        className="files-code"
        aria-label={t`Contents of ${name}`}
        value={draft}
        onChange={(event) => change(event.target.value)}
        onKeyDown={onKeyDown}
      />
      <div className="files-foot">
        <span>{plural(lineCount(draft), { one: "# line", other: "# lines" })}</span>
        <span>UTF-8</span>
        <span>{base.crlf ? "CRLF" : "LF"}</span>
      </div>
      {edited ? (
        <div className="files-save">
          <div className="files-save-what">
            <p className="dw-text-label files-save-count">
              {plural(changes, { one: "# line changed", other: "# lines changed" })}
            </p>
            <span>
              <Trans>Saving replaces the file at once, and can't be undone.</Trans>
            </span>
          </div>
          <div className="files-save-actions">
            <Button disabled={saving} onClick={discard}>
              <Trans>Discard</Trans>
            </Button>
            <Button
              variant="primary"
              kbd={phone ? undefined : onApple() ? "⌘S" : "Ctrl S"}
              disabled={saving || conflict}
              onClick={() => void save(base.version)}
            >
              {saving ? <Trans>Saving</Trans> : <Trans>Save</Trans>}
            </Button>
          </div>
        </div>
      ) : null}
      <span className="dw-sr" role="status">
        {justSaved ? t`Saved ${name}` : ""}
      </span>
      {justSaved && !edited ? (
        <p className="files-saved">
          <Icon name="check" size={14} />
          <Trans>Saved {name}</Trans>
        </p>
      ) : null}
    </>
  );
}
