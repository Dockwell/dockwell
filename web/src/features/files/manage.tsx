// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type ArchiveJob, type DeletedFile, type FileEntry, FileService, FileType, StepUpAction } from "@dockwell/api";
import { Button, Callout, Icon, Loading, Sheet, SkeletonRows, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { AddPasskeyHere } from "../auth/AddPasskeyHere";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useControlPlaneNow } from "../servers/useFleet";
import { join, nameOf, parentOf } from "./paths";
import { unpackedName } from "./transfer";

/*
 * Making, renaming, moving, compressing, extracting and deleting on a server,
 * and putting back what was deleted (docs/adr/0045-a-file-manager-on-a-server.md).
 * Each is one request through the public API. Where it can let someone back onto
 * the server, the agent asks for a passkey for the paths it resolved.
 */

/** A path and its entry metadata for operations that work on one or many items. */
export interface FileTarget {
  path: string;
  name: string;
  type: FileType;
}

export function targetOf(folder: string, entry: FileEntry): FileTarget {
  return { path: join(folder, entry.name), name: entry.name, type: entry.type };
}

/** The client for files, and what refreshes once something changed. */
export function useFiles(serverId: string) {
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
  const noPasskey = t`That takes a passkey, and there's none for this address yet.`;
  const problem = (err: unknown, _where: string): string => {
    if (reasonOf(err) === "no_passkey_here") return noPasskey;
    if (err instanceof PasskeyError && err.problem === "cancelled") {
      return t`The passkey prompt closed, so nothing changed. Try again when you're ready.`;
    }
    return err instanceof PasskeyError ? explain(err) : sentence(err);
  };
  return { transport, client, changed, problem, noPasskey };
}

/** Why a change didn't happen, offering to add a passkey if none exists for this address. */
function Refused({
  failure,
  noPasskey,
  title,
  onAdded,
}: {
  failure: string;
  noPasskey: string;
  title?: string;
  onAdded: () => void;
}) {
  if (failure === noPasskey) return <AddPasskeyHere title={noPasskey} onAdded={onAdded} />;
  return (
    <Callout tone="critical" title={title}>
      {failure}
    </Callout>
  );
}

/** Why a name can't be one, or nothing. */
export function useNameProblem(): (name: string) => string | undefined {
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
export type Naming = { kind: "folder" } | { kind: "file" } | { kind: "rename"; target: FileTarget };

/** Names a new file or folder in folder, or renames a target (such as from the folder tree). */
export function NameSheet({
  serverId,
  folder,
  naming,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  naming: Naming;
  onClose: () => void;
  onDone: (path: string, kind: FileType) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem, noPasskey } = useFiles(serverId);
  const nameProblem = useNameProblem();
  const field = useRef<HTMLInputElement>(null);
  const was = naming.kind === "rename" ? naming.target.name : "";
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
    const baseDir = naming.kind === "rename" ? parentOf(naming.target.path) : folder;
    const path = join(baseDir, name);
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    try {
      if (naming.kind === "rename") {
        const from = naming.target.path;
        const moved = await withStepUpIfAsked(
          transport,
          StepUpAction.MOVE_WAY_BACK_IN,
          `${serverId}:${from}\n${path}`,
          (proof) => client.moveFile({ serverId, from, to: path, proof }),
        );
        await changed();
        onDone(moved.to, naming.target.type);
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
      subtitle={naming.kind === "rename" ? parentOf(naming.target.path) : folder}
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
          <Refused
            failure={failure}
            noPasskey={noPasskey}
            title={t`Nothing changed`}
            onAdded={() => setFailure(undefined)}
          />
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

/** Moves one or more files or folders to another folder on the same server. */
export function MoveSheet({
  serverId,
  folder,
  targets,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  targets: readonly FileTarget[];
  onClose: () => void;
  onDone: (moved: readonly { from: string; to: string }[]) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem, noPasskey } = useFiles(serverId);
  const field = useRef<HTMLInputElement>(null);
  const [dest, setDest] = useState(folder);
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const single = targets.length === 1 ? targets[0] : undefined;

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || targets.length === 0) return;
    const wanted = dest.trim().replace(/(.)\/+$/, "$1");
    if (!wanted.startsWith("/")) {
      setError(t`Write the whole path, starting with /, such as /var/www.`);
      field.current?.focus();
      return;
    }
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    const results: { from: string; to: string }[] = [];
    try {
      let there: string;
      try {
        there = (await client.listFiles({ serverId, path: wanted })).path;
      } catch (err) {
        setError(reasonOf(err) === "not_found" ? t`There's no folder at ${wanted}.` : sentence(err));
        setBusy(false);
        field.current?.focus();
        return;
      }
      for (const target of targets) {
        const to = join(there, target.name);
        if (to === target.path) {
          if (single) {
            setError(t`It's in that folder already.`);
            setBusy(false);
            return;
          }
          continue;
        }
        try {
          const moved = await withStepUpIfAsked(
            transport,
            StepUpAction.MOVE_WAY_BACK_IN,
            `${serverId}:${target.path}\n${to}`,
            (proof) => client.moveFile({ serverId, from: target.path, to, proof }),
          );
          results.push({ from: target.path, to: moved.to });
        } catch (err) {
          if (results.length > 0) await changed();
          if (reasonOf(err) === "file_exists") {
            setError(t`Something called ${target.name} is in that folder already. Rename one of them first.`);
          } else {
            setFailure(problem(err, target.path));
          }
          setBusy(false);
          return;
        }
      }
      await changed();
      onDone(results);
    } catch (err) {
      setFailure(problem(err, folder));
      setBusy(false);
    }
  };

  const title = single ? t`Move ${single.name}` : plural(targets.length, { one: "Move # item", other: "Move # items" });
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={title}
      subtitle={single ? single.path : folder}
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
          <Refused
            failure={failure}
            noPasskey={noPasskey}
            title={t`Nothing moved`}
            onAdded={() => setFailure(undefined)}
          />
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
          hint={
            single
              ? t`On this server, and on the same disk. It keeps its name, and never replaces what's there.`
              : t`On this server, and on the same disk. Items keep their names, and never replace what's there.`
          }
          onChange={(event) => {
            setDest(event.target.value);
            setError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}

/** Packs one or more selected items into a new .zip archive in folder. */
export function CompressSheet({
  serverId,
  folder,
  targets,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  targets: readonly FileTarget[];
  onClose: () => void;
  onDone: (job: ArchiveJob) => void;
}) {
  const { t } = useLingui();
  const { transport, client, problem, noPasskey } = useFiles(serverId);
  const nameProblem = useNameProblem();
  const field = useRef<HTMLInputElement>(null);
  const single = targets.length === 1 ? targets[0] : undefined;
  const defaultBase = single ? single.name : nameOf(folder) === "/" ? "archive" : nameOf(folder);
  const [name, setName] = useState(`${defaultBase}.zip`);
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || targets.length === 0) return;
    const trimmed = name.trim();
    const withZip = trimmed.toLowerCase().endsWith(".zip") ? trimmed : `${trimmed}.zip`;
    const wrong = nameProblem(withZip);
    if (wrong) {
      setError(wrong);
      field.current?.focus();
      return;
    }
    if (withZip.toLowerCase() === ".zip") {
      setError(t`Give the archive a name before .zip.`);
      field.current?.focus();
      return;
    }
    const archivePath = join(folder, withZip);
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    try {
      const res = await withStepUpIfAsked(
        transport,
        StepUpAction.COMPRESS_WAY_BACK_IN,
        `${serverId}:${archivePath}`,
        (proof) =>
          client.compressFiles({
            serverId,
            paths: targets.map((item) => item.path),
            archivePath,
            proof,
          }),
      );
      if (!res.job) {
        setFailure(t`The server didn't start the archive job. Try again.`);
        setBusy(false);
        return;
      }
      onDone(res.job);
    } catch (err) {
      if (reasonOf(err) === "file_exists") {
        setError(t`Something called ${withZip} is here already. Choose another name.`);
        field.current?.focus();
      } else {
        setFailure(problem(err, archivePath));
      }
      setBusy(false);
    }
  };

  const title = single
    ? t`Compress ${single.name}`
    : plural(targets.length, { one: "Compress # item", other: "Compress # items" });

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
          <Button variant="primary" icon="archive" loading={busy} onClick={() => void submit()}>
            <Trans>Compress</Trans>
          </Button>
        </>
      }
    >
      <form className="files-sheet" onSubmit={(event) => void submit(event)}>
        {failure ? (
          <Refused
            failure={failure}
            noPasskey={noPasskey}
            title={t`Nothing was compressed`}
            onAdded={() => setFailure(undefined)}
          />
        ) : null}
        <TextField
          ref={field}
          label={t`Archive name`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={name}
          error={error}
          hint={t`Creates a new .zip archive in this folder. Never replaces a file that's already there.`}
          onChange={(event) => {
            setName(event.target.value);
            setError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}

/** Extracts an existing .zip, .tar.gz or .tgz archive into a new folder beside it. */
export function useExtract(serverId: string) {
  const { transport, client, problem } = useFiles(serverId);
  const { t } = useLingui();
  const [extracting, setExtracting] = useState(false);
  const extract = async (archivePath: string): Promise<ArchiveJob> => {
    setExtracting(true);
    try {
      const dstPath = join(parentOf(archivePath), unpackedName(nameOf(archivePath)));
      const res = await withStepUpIfAsked(
        transport,
        StepUpAction.EXTRACT_WAY_BACK_IN,
        `${serverId}:${archivePath}\n${dstPath}`,
        (proof) => client.extractArchive({ serverId, path: archivePath, proof }),
      );
      if (!res.job) {
        throw new Error(t`The server didn't start the archive job. Try again.`);
      }
      return res.job;
    } finally {
      setExtracting(false);
    }
  };
  return { extract, extracting, problem };
}

export interface DeletedResult {
  path: string;
  kept?: DeletedFile;
}

/** Deletes one or more items: to Recently deleted, or for good. */
export function DeleteSheet({
  serverId,
  targets,
  onClose,
  onDone,
}: {
  serverId: string;
  targets: readonly FileTarget[];
  onClose: () => void;
  onDone: (results: readonly DeletedResult[]) => void;
}) {
  const { t } = useLingui();
  const { transport, client, changed, problem, noPasskey } = useFiles(serverId);
  const [forGood, setForGood] = useState(false);
  const [onlyForGood, setOnlyForGood] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const unspent = useRef("");
  const single = targets.length === 1 ? targets[0] : undefined;
  const isFolder = single?.type === FileType.DIRECTORY;
  const hasFolders = targets.some((item) => item.type === FileType.DIRECTORY);
  const goesForGood = forGood || onlyForGood !== undefined;

  const remove = async () => {
    if (busy || targets.length === 0) return;
    setBusy(true);
    setFailure(undefined);
    const results: DeletedResult[] = [];
    for (const target of targets) {
      const path = target.path;
      const call = (proof: string) => {
        unspent.current = proof;
        return client.deleteFile({ serverId, path, forGood: goesForGood, proof });
      };
      try {
        let deleted: Awaited<ReturnType<typeof call>> | undefined;
        if (unspent.current && single) {
          try {
            deleted = await call(unspent.current);
          } catch (err) {
            if (reasonOf(err) !== "needs_step_up") throw err;
          }
        }
        deleted ??= await withStepUpIfAsked(transport, StepUpAction.DELETE_WAY_BACK_IN, `${serverId}:${path}`, call);
        unspent.current = "";
        results.push({ path: deleted.path, kept: deleted.deleted });
      } catch (err) {
        if (results.length > 0) await changed();
        if (reasonOf(err) === "delete_for_good") {
          setOnlyForGood(sentence(err));
        } else {
          unspent.current = "";
          setFailure(problem(err, path));
        }
        setBusy(false);
        return;
      }
    }
    await changed();
    onDone(results);
  };

  const title = single
    ? t`Delete ${single.name}`
    : plural(targets.length, { one: "Delete # item", other: "Delete # items" });
  const count = targets.length;

  return (
    <Sheet
      open
      title={title}
      subtitle={single ? single.path : parentOf(targets[0]?.path ?? "/")}
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
          <Refused
            failure={failure}
            noPasskey={noPasskey}
            title={t`Nothing was deleted`}
            onAdded={() => setFailure(undefined)}
          />
        ) : null}
        {onlyForGood !== undefined ? (
          <Callout tone="warning" title={t`This can't be put back`}>
            {onlyForGood}
          </Callout>
        ) : (
          <>
            <p className="files-sheet-line">
              {single ? (
                goesForGood ? (
                  isFolder ? (
                    <Trans>
                      {single.name} and everything in it go at once, and nothing brings them back. Its space is free
                      straight away.
                    </Trans>
                  ) : (
                    <Trans>
                      {single.name} goes at once, and nothing brings it back. Its space is free straight away.
                    </Trans>
                  )
                ) : isFolder ? (
                  <Trans>
                    {single.name} and everything in it go to Recently deleted, where you can put them back for 7 days.
                    They take their space until then.
                  </Trans>
                ) : (
                  <Trans>
                    {single.name} goes to Recently deleted, where you can put it back for 7 days. It takes its space
                    until then.
                  </Trans>
                )
              ) : goesForGood ? (
                hasFolders ? (
                  <Trans>
                    These {count} items and everything in any folders go at once, and nothing brings them back. Their
                    space is free straight away.
                  </Trans>
                ) : (
                  <Trans>
                    These {count} items go at once, and nothing brings them back. Their space is free straight away.
                  </Trans>
                )
              ) : (
                <Trans>
                  These {count} items go to Recently deleted, where you can put them back for 7 days. They take their
                  space until then.
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
        {single?.type === FileType.SYMLINK ? (
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
  const { client, changed, problem, noPasskey } = useFiles(serverId);
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
        {failure ? <Refused failure={failure} noPasskey={noPasskey} onAdded={() => setFailure(undefined)} /> : null}
        {query.error ? <Callout tone="critical">{sentence(query.error)}</Callout> : null}
        {query.isPending ? (
          <Loading label={t`Reading what was deleted`}>
            <SkeletonRows rows={3} lead="icon" end="value" dense />
          </Loading>
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
