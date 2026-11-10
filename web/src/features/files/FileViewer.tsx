// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { downloadFileURL, type FileEntry, FileService, type ReadFileResponse, StepUpAction } from "@dockwell/api";
import { Button, ButtonLink, Callout, CodeField, Icon, Panel, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useBlocker, useNavigate } from "@tanstack/react-router";
import { type KeyboardEvent, useMemo, useState } from "react";

import { isRoleRefusal, reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { PasskeyError } from "../../lib/webauthn";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import { type Base, type Draft, dropDraft, heldDraft, holdDraft } from "./drafts";
import { changedLines, editable, lineCount, lineEnding, nameOf, withEndings } from "./paths";

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
  const download = (
    <ButtonLink size="sm" icon="download" href={downloadFileURL({ serverId, path })} download>
      <Trans>Download</Trans>
    </ButtonLink>
  );
  const body = () => {
    if (query.error) {
      const reason = reasonOf(query.error);
      const refused = reason === "file_refused" || isRoleRefusal(query.error);
      const title = isRoleRefusal(query.error)
        ? t`Your role doesn't cover this`
        : reason === "file_refused"
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
          <Callout tone="info" title={t`This isn't a text file`} actions={download}>
            <Trans>
              It's {size}, and it isn't UTF-8 text, so there's nothing to show here. Download it to open it on your
              computer, or connect to the server over SSH to work with it.
            </Trans>
          </Callout>
        </div>
      );
    }
    if (read.truncated) {
      return (
        <>
          <div className="files-note">
            <Callout tone="warning" title={t`Showing the first 1 MB`} actions={download}>
              <Trans>
                This file is {size}, so the rest of it stays on the server, and it can't be edited here. Download it to
                see it all, or connect to the server over SSH to work with it.
              </Trans>
            </Callout>
          </div>
          <CodeField className="files-code" aria-label={t`Contents of ${name}`} value={read.content} readOnly />
        </>
      );
    }
    if (!read.version) {
      return (
        <>
          <div className="files-note">
            <Callout tone="info" title={t`This server's agent can't save files`}>
              <Trans>Update the agent on this server to edit {name} here. You can still read it.</Trans>
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
        phone ? undefined : (
          <>
            {/* What a read refused, a download does, and a browser shows only "Failed" for it. */}
            {query.error ? null : (
              <ButtonLink
                variant="ghost"
                size="sm"
                icon="download"
                href={downloadFileURL({ serverId, path })}
                download
                aria-label={t`Download ${name}`}
                title={t`Download`}
              />
            )}
            <Button variant="ghost" size="sm" icon="x" aria-label={t`Close ${name}`} onClick={close} />
          </>
        )
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

function baseOf(read: ReadFileResponse): Base {
  return {
    text: editable(read.content),
    version: read.version,
    original: read.content,
    where: read.path,
    wayBackIn: read.wayBackIn,
  };
}

/**
 * A file's text, to edit and save. A save names the version it was edited from,
 * so a file that changed since isn't written over without you saying so. What you
 * typed stays in the field whatever a save comes to, and leaving with it unsaved
 * asks first. A session that ends takes the page with it and asks nobody, so what
 * you typed is held for when you sign in again.
 */
function Editor({ serverId, path, name, read, onSaved, onEdited }: EditorProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const [base, setBase] = useState(() => baseOf(read));
  const [draft, setDraft] = useState(base.text);
  // A save keeps each line's ending, so a file with more than one kind says so and edits as any other.
  const ending = useMemo(() => lineEnding(base.original), [base.original]);
  const [saving, setSaving] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [problem, setProblem] = useState<string>();
  // A save that was writing as the server stopped answering isn't a failure.
  const [mayLand, setMayLand] = useState(false);
  // Where the file leads now, once that isn't where it led when you read it.
  const [moved, setMoved] = useState<string>();
  const [justSaved, setJustSaved] = useState(false);
  // An edit too large to keep each line's ending, on a file with more than one kind: the person picks one,
  // and it's saved over the version this holds, the one the save that asked was going to save over.
  const [pickEnding, setPickEnding] = useState<string>();
  const who = useSignedInUser()?.id ?? "";
  const [offer, setOffer] = useState<Draft | undefined>(() => heldDraft(serverId, path, who));
  const edited = draft !== base.text;
  const changes = edited ? changedLines(base.text, draft) : 0;

  useBlocker({
    shouldBlockFn: () => {
      if (!window.confirm(t`Leave ${name} without saving? What you typed goes.`)) return true;
      dropDraft(serverId, path);
      return false;
    },
    disabled: !edited,
    enableBeforeUnload: edited,
  });

  const client = createClient(FileService, transport);
  const change = (next: string) => {
    setPickEnding(undefined);
    setDraft(next);
    setJustSaved(false);
    onEdited(next !== base.text);
    if (next === base.text) dropDraft(serverId, path);
    else holdDraft(serverId, path, { who, text: next, base });
  };

  const discard = () => {
    change(base.text);
    setConflict(false);
    setMoved(undefined);
    setProblem(undefined);
  };

  /** Back to what was typed before you were signed out, over what it was typed on. */
  const bringBack = (held: Draft) => {
    setBase(held.base);
    setDraft(held.text);
    onEdited(true);
    setOffer(undefined);
  };

  const letGo = () => {
    dropDraft(serverId, path);
    setOffer(undefined);
  };

  /** Saves what's typed over the version given, each line with its own ending, or every line with one. */
  const save = async (over: string, every?: "LF" | "CRLF") => {
    const sent = draft;
    const content =
      every === "CRLF" ? sent.replace(/\n/g, "\r\n") : every === "LF" ? sent : withEndings(sent, base.original);
    if (content === undefined) {
      setPickEnding(over);
      return;
    }
    setPickEnding(undefined);
    setSaving(true);
    setProblem(undefined);
    setMayLand(false);
    setConflict(false);
    setJustSaved(false);
    try {
      const res = await withStepUpIfAsked(
        transport,
        StepUpAction.SAVE_WAY_BACK_IN,
        `${serverId}:${base.where}`,
        (proof) =>
          client.saveFile({
            serverId,
            path,
            content,
            version: over,
            proof,
            confirmedPath: base.wayBackIn || proof ? base.where : "",
          }),
      );
      // What was saved is what the next save keeps each line's ending from.
      setBase((was) => ({ ...was, text: sent, original: content, version: res.version }));
      dropDraft(serverId, path);
      setJustSaved(true);
      onSaved(res.file);
      onEdited(false);
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: FileService.method.listFiles, cardinality: "finite" }),
      });
    } catch (err) {
      const reason = reasonOf(err);
      if (reason === "file_changed") setConflict(true);
      else if (reason === "no_passkey_here") {
        const where = base.where;
        setProblem(
          t`Saving ${where} takes a passkey, and you have none for this address. Connect to the server over SSH to change it.`,
        );
      } else {
        setMayLand(reason === "save_may_land");
        if (err instanceof PasskeyError && err.problem === "cancelled") {
          setProblem(t`The passkey prompt closed, so nothing was saved. Save again when you're ready.`);
        } else {
          setProblem(err instanceof PasskeyError ? explain(err) : sentence(err));
        }
      }
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
      // The change may be that this path leads somewhere else now, so saving would
      // write there. You decide that with the file in front of you.
      if (now.path !== base.where) {
        setConflict(false);
        setMoved(now.path);
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
      dropDraft(serverId, path);
      setConflict(false);
      setMoved(undefined);
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
      if (edited && !saving && !conflict && !moved) void save(base.version);
    }
  };

  return (
    <>
      {base.where !== path ? (
        <p className="files-facts">
          <span>
            <Trans>
              Leads to <code>{base.where}</code>
            </Trans>
          </span>
        </p>
      ) : null}
      {offer ? (
        <div className="files-note">
          <Callout tone="info" title={t`You have unsaved changes to ${name}`}>
            {offer.base.version === base.version ? (
              <Trans>You typed them before your session ended.</Trans>
            ) : (
              <Trans>
                You typed them before your session ended, and the file has changed on the server since, so saving will
                ask what to do.
              </Trans>
            )}
            <span className="files-note-actions">
              <Button size="sm" onClick={() => bringBack(offer)}>
                <Trans>Bring them back</Trans>
              </Button>
              <Button size="sm" variant="ghost" onClick={letGo}>
                <Trans>Discard them</Trans>
              </Button>
            </span>
          </Callout>
        </div>
      ) : null}
      {moved ? (
        <div className="files-note">
          <Callout tone="critical" title={t`This isn't the file you opened`}>
            <Trans>
              {name} now leads to <code>{moved}</code>, so saving would write there. Nothing was saved, and what you
              typed is still here. Load the server's version to see what's there.
            </Trans>
            <span className="files-note-actions">
              <Button size="sm" variant="ghost" disabled={saving} onClick={() => void loadServers()}>
                <Trans>Load the server's version</Trans>
              </Button>
            </span>
          </Callout>
        </div>
      ) : null}
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
          <Callout
            tone={mayLand ? "warning" : "critical"}
            title={mayLand ? t`This save may still land` : t`Can't save this file`}
          >
            {problem} <Trans>What you typed is still here.</Trans>
          </Callout>
        </div>
      ) : null}
      <CodeField
        className="files-code"
        aria-label={t`Contents of ${name}`}
        value={draft}
        readOnly={offer !== undefined}
        onChange={(event) => change(event.target.value)}
        onKeyDown={onKeyDown}
      />
      <div className="files-foot">
        <span>{plural(lineCount(draft), { one: "# line", other: "# lines" })}</span>
        <span>UTF-8</span>
        <span>{ending === "mixed" ? t`Mixed line endings` : ending}</span>
      </div>
      {edited && pickEnding !== undefined ? (
        <div className="files-note">
          <Callout
            tone="warning"
            title={t`This edit moves too much to keep each line's ending`}
            actions={
              <>
                <Button size="sm" disabled={saving} onClick={() => void save(pickEnding, "LF")}>
                  <Trans>Save with LF</Trans>
                </Button>
                <Button size="sm" disabled={saving} onClick={() => void save(pickEnding, "CRLF")}>
                  <Trans>Save with CRLF</Trans>
                </Button>
              </>
            }
          >
            {t`${name} has more than one kind of line ending; pick one for every line.`}
          </Callout>
        </div>
      ) : null}
      {edited ? (
        <div className="files-save">
          <div className="files-save-what">
            <p className="dw-text-label files-save-count">
              {plural(changes, { one: "# line changed", other: "# lines changed" })}
            </p>
            <span>
              <Trans>Saving replaces the file at once, and can't be undone.</Trans>{" "}
              {base.wayBackIn ? <Trans>It asks for your passkey, as this file can let someone back in.</Trans> : null}
            </span>
          </div>
          <div className="files-save-actions">
            <Button disabled={saving} onClick={discard}>
              <Trans>Discard</Trans>
            </Button>
            <Button
              variant="primary"
              kbd={phone ? undefined : onApple() ? "⌘S" : "Ctrl S"}
              disabled={saving || conflict || moved !== undefined}
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
