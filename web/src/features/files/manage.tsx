// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type DeletedFile, type FileEntry, FileService, FileType, StepUpAction } from "@dockwell/api";
import { Button, Callout, Icon, Sheet, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useControlPlaneNow } from "../servers/useFleet";
import { join, nameOf, parentOf } from "./paths";

/*
 * Making, renaming, moving and deleting on a server, and putting back what was deleted
 * (docs/adr/0045-a-file-manager-on-a-server.md). Each is one request through the public
 * API. Where it can let someone back onto the server, the agent asks for a passkey, for
 * the paths it resolved, which are the ones a listing gives: a folder's path has every
 * link in it followed, and its entries' names are what's acted on.
 */

/** The client for files, and what refreshes once something changed. */
function useFiles(serverId: string) {
  const transport = useTransport();
  const client = useMemo(() => createClient(FileService, transport), [transport]);
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const { t } = useLingui();
  const changed = () =>
    Promise.all([
      queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: FileService.method.listFiles, cardinality: "finite" }),
      }),
      queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({
          schema: FileService.method.listDeletedFiles,
          input: { serverId },
          cardinality: "finite",
        }),
      }),
    ]);
  /** What a write that didn't happen says, in words that say what to do. */
  const problem = (err: unknown, where: string): string => {
    if (reasonOf(err) === "no_passkey_here") {
      return t`Changing ${where} takes a passkey, and you have none for this address. Connect to the server over SSH to change it.`;
    }
    if (err instanceof PasskeyError && err.problem === "cancelled") {
      return t`The passkey prompt closed, so nothing changed. Try again when you're ready.`;
    }
    return err instanceof PasskeyError ? explain(err) : sentence(err);
  };
  return { transport, client, changed, problem };
}

/** Why a name can't be one, or nothing. */
function useNameProblem(): (name: string) => string | undefined {
  const { t } = useLingui();
  return (name) => {
    if (name.trim() === "") return t`Give it a name.`;
    if (name.includes("/")) return t`A name can't have a / in it. Make the folder first, then this in it.`;
    if (name === "." || name === "..") return t`That name stands for a folder already. Choose another.`;
    if (new TextEncoder().encode(name).length > 255)
      return t`That name is too long for a server. Keep it under 255 bytes.`;
    return undefined;
  };
}

/** What a sheet that names something makes of it. */
export type Naming = { kind: "folder" } | { kind: "file" } | { kind: "rename"; entry: FileEntry };

/** Names a new file or folder in folder, or a new name for an entry in it. */
export function NameSheet({
  serverId,
  folder,
  naming,
  onClose,
  onDone,
}: {
  serverId: string;
  /** Where it is, with every link followed, as the listing gave it. */
  folder: string;
  naming: Naming;
  onClose: () => void;
  /** What's there now, by its path. */
  onDone: (path: string, kind: FileType) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem } = useFiles(serverId);
  const nameProblem = useNameProblem();
  const field = useRef<HTMLInputElement>(null);
  const was = naming.kind === "rename" ? naming.entry.name : "";
  const [name, setName] = useState(was);
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wrong = nameProblem(name);
    if (wrong) {
      setError(wrong);
      field.current?.focus();
      return;
    }
    if (naming.kind === "rename" && name === was) {
      onClose();
      return;
    }
    const path = join(folder, name);
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    try {
      if (naming.kind === "rename") {
        const from = join(folder, was);
        const moved = await withStepUpIfAsked(
          transport,
          StepUpAction.MOVE_WAY_BACK_IN,
          `${serverId}:${from}\n${path}`,
          (proof) => client.moveFile({ serverId, from, to: path, proof }),
        );
        await changed();
        onDone(moved.to, naming.entry.type);
      } else {
        const type = naming.kind === "folder" ? FileType.DIRECTORY : FileType.FILE;
        const made = await withStepUpIfAsked(transport, StepUpAction.MAKE_WAY_BACK_IN, `${serverId}:${path}`, (proof) =>
          client.makeFile({ serverId, path, type, proof }),
        );
        await changed();
        onDone(made.path, type);
      }
    } catch (err) {
      if (reasonOf(err) === "file_exists") {
        setError(t`Something called ${name} is here already. Choose another name.`);
        field.current?.focus();
      } else {
        setFailure(problem(err, path));
      }
      setBusy(false);
    }
  };

  const title =
    naming.kind === "folder"
      ? t`New folder in ${nameOf(folder)}`
      : naming.kind === "file"
        ? t`New file in ${nameOf(folder)}`
        : t`Rename ${was}`;
  const action =
    naming.kind === "folder" ? (
      <Trans>Make folder</Trans>
    ) : naming.kind === "file" ? (
      <Trans>Make file</Trans>
    ) : (
      <Trans>Rename</Trans>
    );
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={title}
      subtitle={folder}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This goes in the activity log</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={busy} onClick={() => void submit()}>
            {action}
          </Button>
        </>
      }
    >
      <form className="files-sheet" onSubmit={(event) => void submit(event)}>
        {failure ? (
          <Callout tone="critical" title={t`Nothing changed`}>
            {failure}
          </Callout>
        ) : null}
        <TextField
          ref={field}
          label={t`Name`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={name}
          error={error}
          hint={
            naming.kind === "folder"
              ? t`It starts empty. Everyone can open it, and this folder's owner can change it.`
              : naming.kind === "file"
                ? t`It starts empty, and opens to edit. Everyone can read it, and this folder's owner can change it.`
                : undefined
          }
          onChange={(event) => {
            setName(event.target.value);
            setError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}

/** Moves an entry of folder to another folder on the same server. */
export function MoveSheet({
  serverId,
  folder,
  entry,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  entry: FileEntry;
  onClose: () => void;
  onDone: (path: string) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem } = useFiles(serverId);
  const field = useRef<HTMLInputElement>(null);
  const [dest, setDest] = useState(folder);
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const from = join(folder, entry.name);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wanted = dest.trim().replace(/(.)\/+$/, "$1");
    if (!wanted.startsWith("/")) {
      setError(t`Write the whole path, starting with /, such as /var/www.`);
      field.current?.focus();
      return;
    }
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    try {
      // Where it goes, with every link followed, as a passkey confirms what the server resolves.
      let there: string;
      try {
        there = (await client.listFiles({ serverId, path: wanted })).path;
      } catch (err) {
        setError(reasonOf(err) === "not_found" ? t`There's no folder at ${wanted}.` : sentence(err));
        setBusy(false);
        field.current?.focus();
        return;
      }
      const to = join(there, entry.name);
      if (to === from) {
        setError(t`It's in that folder already.`);
        setBusy(false);
        return;
      }
      const moved = await withStepUpIfAsked(
        transport,
        StepUpAction.MOVE_WAY_BACK_IN,
        `${serverId}:${from}\n${to}`,
        (proof) => client.moveFile({ serverId, from, to, proof }),
      );
      await changed();
      onDone(moved.to);
    } catch (err) {
      if (reasonOf(err) === "file_exists") {
        setError(t`Something called ${entry.name} is in that folder already. Rename one of them first.`);
      } else {
        setFailure(problem(err, from));
      }
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      initialFocusRef={field}
      title={t`Move ${entry.name}`}
      subtitle={from}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This goes in the activity log</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="arrow-right" loading={busy} onClick={() => void submit()}>
            <Trans>Move</Trans>
          </Button>
        </>
      }
    >
      <form className="files-sheet" onSubmit={(event) => void submit(event)}>
        {failure ? (
          <Callout tone="critical" title={t`Nothing moved`}>
            {failure}
          </Callout>
        ) : null}
        <TextField
          ref={field}
          label={t`To the folder`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={dest}
          error={error}
          hint={t`On this server, and on the same disk. It keeps its name, and never replaces what's there.`}
          onChange={(event) => {
            setDest(event.target.value);
            setError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}

/** Deletes an entry of folder: to Recently deleted, or for good. */
export function DeleteSheet({
  serverId,
  folder,
  entry,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  entry: FileEntry;
  onClose: () => void;
  /** What's in Recently deleted now, or nothing where it went for good. */
  onDone: (path: string, kept: DeletedFile | undefined) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem } = useFiles(serverId);
  const [forGood, setForGood] = useState(false);
  // The server said it can't be put back, so the only delete there is is for good.
  const [onlyForGood, setOnlyForGood] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  // A proof that came back unspent, as the delete couldn't be put back, which the
  // delete for good that follows uses, so one delete takes one passkey.
  const unspent = useRef("");
  const path = join(folder, entry.name);
  const name = entry.name;
  const isFolder = entry.type === FileType.DIRECTORY;
  const goesForGood = forGood || onlyForGood !== undefined;

  const remove = async () => {
    if (busy) return;
    setBusy(true);
    setFailure(undefined);
    const call = (proof: string) => {
      unspent.current = proof;
      return client.deleteFile({ serverId, path, forGood: goesForGood, proof });
    };
    try {
      let deleted: Awaited<ReturnType<typeof call>> | undefined;
      if (unspent.current) {
        try {
          deleted = await call(unspent.current);
        } catch (err) {
          // Spent or past its minute: ask again, as anything else would.
          if (reasonOf(err) !== "needs_step_up") throw err;
        }
      }
      deleted ??= await withStepUpIfAsked(transport, StepUpAction.DELETE_WAY_BACK_IN, `${serverId}:${path}`, call);
      await changed();
      onDone(deleted.path, deleted.deleted);
    } catch (err) {
      if (reasonOf(err) === "delete_for_good") {
        setOnlyForGood(sentence(err));
      } else {
        unspent.current = "";
        setFailure(problem(err, path));
      }
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      title={t`Delete ${name}`}
      subtitle={path}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This goes in the activity log</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="danger" icon="trash" loading={busy} onClick={() => void remove()}>
            {goesForGood ? <Trans>Delete for good</Trans> : <Trans>Delete</Trans>}
          </Button>
        </>
      }
    >
      <div className="files-sheet">
        {failure ? (
          <Callout tone="critical" title={t`Nothing was deleted`}>
            {failure}
          </Callout>
        ) : null}
        {onlyForGood !== undefined ? (
          <Callout tone="warning" title={t`This can't be put back`}>
            {onlyForGood}
          </Callout>
        ) : (
          <>
            <p className="files-sheet-line">
              {goesForGood ? (
                isFolder ? (
                  <Trans>
                    {name} and everything in it go at once, and nothing brings them back. Its space is free straight
                    away.
                  </Trans>
                ) : (
                  <Trans>{name} goes at once, and nothing brings it back. Its space is free straight away.</Trans>
                )
              ) : isFolder ? (
                <Trans>
                  {name} and everything in it go to Recently deleted, where you can put them back for 7 days. They take
                  their space until then.
                </Trans>
              ) : (
                <Trans>
                  {name} goes to Recently deleted, where you can put it back for 7 days. It takes its space until then.
                </Trans>
              )}
            </p>
            <Switch
              checked={forGood}
              onChange={setForGood}
              label={t`Delete for good`}
              description={t`Free its space now. Nothing brings it back.`}
            />
          </>
        )}
        {entry.type === FileType.SYMLINK ? (
          <p className="files-sheet-line">
            <Trans>This deletes the link. What it leads to stays where it is.</Trans>
          </p>
        ) : null}
      </div>
    </Sheet>
  );
}

/** Puts back what was deleted, with a passkey where it's a way back in. */
export function useRestore(serverId: string): (d: Pick<DeletedFile, "id" | "path">) => Promise<string> {
  const { transport, client, changed } = useFiles(serverId);
  return async (d) => {
    const back = await withStepUpIfAsked(
      transport,
      StepUpAction.RESTORE_WAY_BACK_IN,
      `${serverId}:${d.path}`,
      (proof) => client.restoreFile({ serverId, id: d.id, proof }),
    );
    await changed();
    return back.path;
  };
}

/** What a put back that didn't happen says. */
export function useRestoreProblem(serverId: string): (err: unknown, path: string) => string {
  const { t } = useLingui();
  const { problem } = useFiles(serverId);
  return (err, path) => {
    const folder = parentOf(path);
    const name = nameOf(path);
    switch (reasonOf(err)) {
      case "file_exists":
        return t`Something is at ${path} now, so it wasn't put back. Move or rename that first.`;
      case "folder_gone":
        return t`The folder ${folder} is gone, so ${name} wasn't put back, and it's still in Recently deleted. Make the folder again first, then put it back.`;
    }
    return problem(err, path);
  };
}

/** What's in a server's Recently deleted, to put back or delete for good. */
export function DeletedSheet({
  serverId,
  onClose,
  onRestored,
}: {
  serverId: string;
  onClose: () => void;
  onRestored: (path: string) => void;
}) {
  const { t, i18n } = useLingui();
  const { client, changed, problem } = useFiles(serverId);
  const putBack = useRestore(serverId);
  const restoreProblem = useRestoreProblem(serverId);
  const words = useTimeWords();
  const now = useControlPlaneNow();
  const query = useQuery(FileService.method.listDeletedFiles, { serverId });
  const [busy, setBusy] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [purging, setPurging] = useState<string>();
  const deleted = query.data?.deleted ?? [];

  const restore = async (d: DeletedFile) => {
    setBusy(d.id);
    setFailure(undefined);
    try {
      onRestored(await putBack(d));
    } catch (err) {
      setFailure(restoreProblem(err, d.path));
    } finally {
      setBusy(undefined);
    }
  };

  const purge = async (d: DeletedFile) => {
    setBusy(d.id);
    setFailure(undefined);
    try {
      await client.purgeDeletedFile({ serverId, id: d.id });
      setPurging(undefined);
      await changed();
    } catch (err) {
      setFailure(problem(err, d.path));
    } finally {
      setBusy(undefined);
    }
  };

  return (
    <Sheet
      open
      title={t`Recently deleted`}
      subtitle={t`Kept for 7 days, then deleted for good`}
      onClose={onClose}
      footer={
        <Button variant="ghost" onClick={onClose}>
          <Trans>Close</Trans>
        </Button>
      }
    >
      <div className="files-sheet">
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {query.error ? <Callout tone="critical">{sentence(query.error)}</Callout> : null}
        {query.isPending ? (
          <p className="files-sheet-line">
            <Trans>Reading what was deleted</Trans>
          </p>
        ) : deleted.length === 0 && !query.error ? (
          <p className="files-sheet-line">
            <Trans>Nothing is in Recently deleted.</Trans>
          </p>
        ) : null}
        {deleted.length > 0 ? (
          <ul className="files-deleted" aria-label={t`Recently deleted`}>
            {deleted.map((d) => {
              const name = nameOf(d.path);
              const when = d.deleteTime ? words.ago(timestampDate(d.deleteTime), now) : "";
              const left = d.purgeTime ? words.span(Math.max(0, timestampDate(d.purgeTime).getTime() - now)) : "";
              return (
                <li key={d.id} className="files-deleted-row">
                  <span className="files-icon">
                    <Icon
                      name={d.type === FileType.DIRECTORY ? "folder" : d.type === FileType.SYMLINK ? "link" : "file"}
                      size={15}
                    />
                  </span>
                  <span className="files-deleted-what">
                    <span className="files-entry-name" title={d.path}>
                      {name}
                    </span>
                    <span className="files-deleted-meta">
                      <Trans>
                        Deleted {when}, goes in {left}
                      </Trans>
                      {d.type === FileType.FILE ? ` · ${formatBytes(d.sizeBytes, i18n.locale)}` : ""}
                    </span>
                    <span className="files-deleted-meta files-deleted-from" title={parentOf(d.path)}>
                      {parentOf(d.path)}
                    </span>
                  </span>
                  {purging === d.id ? (
                    <span className="files-deleted-actions">
                      <Button size="sm" variant="ghost" onClick={() => setPurging(undefined)}>
                        <Trans>Keep</Trans>
                      </Button>
                      <Button size="sm" variant="danger" loading={busy === d.id} onClick={() => void purge(d)}>
                        <Trans>Delete for good</Trans>
                      </Button>
                    </span>
                  ) : (
                    <span className="files-deleted-actions">
                      <Button
                        size="sm"
                        variant="ghost"
                        icon="trash"
                        aria-label={t`Delete ${name} for good`}
                        disabled={busy !== undefined}
                        onClick={() => setPurging(d.id)}
                      />
                      <Button
                        size="sm"
                        icon="undo"
                        loading={busy === d.id}
                        disabled={busy !== undefined && busy !== d.id}
                        onClick={() => void restore(d)}
                      >
                        <Trans context="what was deleted">Put back</Trans>
                      </Button>
                    </span>
                  )}
                </li>
              );
            })}
          </ul>
        ) : null}
      </div>
    </Sheet>
  );
}

/** What can be done to an entry from its row. */
export function RowActions({
  entry,
  onRename,
  onMove,
  onDelete,
}: {
  entry: FileEntry;
  onRename: () => void;
  onMove: () => void;
  onDelete: () => void;
}) {
  const { t } = useLingui();
  const name = entry.name;
  // The table leaves a click on these to them, rather than opening the row.
  return (
    <span className="files-row-actions">
      <Button
        size="sm"
        variant="ghost"
        icon="edit"
        aria-label={t`Rename ${name}`}
        title={t`Rename`}
        onClick={onRename}
      />
      <Button
        size="sm"
        variant="ghost"
        icon="arrow-right"
        aria-label={t`Move ${name}`}
        title={t`Move`}
        onClick={onMove}
      />
      <Button
        size="sm"
        variant="ghost"
        icon="trash"
        aria-label={t`Delete ${name}`}
        title={t`Delete`}
        onClick={onDelete}
      />
    </span>
  );
}

/** On a phone, a row's actions in a sheet of their own, as three icons a row is too many there. */
export function ActionsSheet({
  folder,
  entry,
  onClose,
  onRename,
  onMove,
  onDelete,
}: {
  folder: string;
  entry: FileEntry;
  onClose: () => void;
  onRename: () => void;
  onMove: () => void;
  onDelete: () => void;
}) {
  return (
    <Sheet open title={entry.name} subtitle={join(folder, entry.name)} onClose={onClose}>
      <div className="files-actions">
        <Button icon="edit" onClick={onRename}>
          <Trans>Rename</Trans>
        </Button>
        <Button icon="arrow-right" onClick={onMove}>
          <Trans>Move</Trans>
        </Button>
        <Button icon="trash" variant="danger" onClick={onDelete}>
          <Trans>Delete</Trans>
        </Button>
      </div>
    </Sheet>
  );
}
