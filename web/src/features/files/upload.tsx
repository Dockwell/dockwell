// SPDX-License-Identifier: AGPL-3.0-only

import { StepUpAction, StreamError, Unpack, uploadFilePart } from "@dockwell/api";
import { Button, Icon, Sheet, Switch } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type DragEvent, useEffect, useRef, useState } from "react";

import { isTransient } from "../../app/api";
import { reasonOf, sentence } from "../../lib/errors";
import { formatBytes, formatBytesOf, formatPercent } from "../../lib/format";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useFiles } from "./manage";
import { join, nameOf } from "./paths";
import { archiveKind, MAX_UPLOAD_BYTES, PART_BYTES, unpackedName } from "./transfer";

/*
 * Uploading files into a folder (docs/adr/0045-a-file-manager-on-a-server.md). Each
 * file goes on its own, a part at a time, and a part that doesn't arrive is sent
 * again from where the server says it got, so a dropped connection costs a part,
 * not the file. What's there already is never replaced, and where a file can let
 * someone back onto the server, the agent asks for a passkey first.
 */

/** How many times a part is tried again before the file waits for you. */
const RETRIES = 5;

/** Where a file to upload is. */
type Stage =
  | "ready"
  | "waiting"
  | "sending"
  | "finishing"
  | "done"
  | "failed"
  | "stopped"
  // Neither is sent: one is too large, and the other a folder.
  | "tooLarge"
  | "folder";

interface Item {
  key: number;
  file: File;
  stage: Stage;
  /** How much of it the server has, as far as the panel knows. */
  sent: number;
  /** The server was busy with other uploads and downloads, so it waits for one to end. */
  busy?: boolean;
  /** The upload on the server, once it began, to go on with. */
  uploadId?: string;
  problem?: string;
  /** Where the archive's contents went, and how many entries. */
  unpackedTo?: string;
  unpackedEntries?: number;
  /** Why an archive that's in place wasn't unpacked. */
  unpackProblem?: string;
}

/** What was dropped or chosen: files, and the names of folders, which an upload doesn't take. */
export interface Chosen {
  files: File[];
  folders: string[];
}

/** What a drop holds, with folders told apart from files. */
export function chosenFrom(data: DataTransfer): Chosen {
  const files: File[] = [];
  const folders: string[] = [];
  for (const item of Array.from(data.items)) {
    if (item.kind !== "file") continue;
    const entry = item.webkitGetAsEntry?.();
    const file = item.getAsFile();
    if (entry?.isDirectory) folders.push(entry.name);
    else if (file) files.push(file);
  }
  return { files, folders };
}

/** Whether a drag carries files, to show where they'd go. */
export function carriesFiles(event: DragEvent): boolean {
  return Array.from(event.dataTransfer.types).includes("Files");
}

/** Whether the next try may get further: the connection, or a server on the way. */
function transient(err: StreamError): boolean {
  return err.status === 0 || err.status >= 500;
}

/** Waits ms, or until signal says stop. */
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(new DOMException("The upload stopped", "AbortError"));
      },
      { once: true },
    );
  });
}

let nextKey = 0;

function itemsOf(chosen: Chosen): Item[] {
  return [
    ...chosen.files.map(
      (file): Item => ({
        key: nextKey++,
        file,
        stage: file.size > MAX_UPLOAD_BYTES ? "tooLarge" : "ready",
        sent: 0,
      }),
    ),
    ...chosen.folders.map((name): Item => ({ key: nextKey++, file: new File([], name), stage: "folder", sent: 0 })),
  ];
}

/** Whether an item is one the Upload button sends. */
function toSend(item: Item): boolean {
  return item.stage === "ready" || item.stage === "failed" || item.stage === "stopped";
}

/** Uploads files into folder, each with its progress, and each on record. */
export function UploadSheet({
  serverId,
  folder,
  chosen,
  onClose,
}: {
  serverId: string;
  /** Where they go, with every link followed, as the listing gave it. */
  folder: string;
  /** What was dropped onto the listing, if anything. */
  chosen?: Chosen;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const { transport, client, changed, problem } = useFiles(serverId);
  const picker = useRef<HTMLInputElement>(null);
  const items = useRef<Item[]>(chosen ? itemsOf(chosen) : []);
  const [shown, setShown] = useState<Item[]>(items.current);
  const [unpack, setUnpack] = useState(false);
  const [running, setRunning] = useState(false);
  const [over, setOver] = useState(false);
  const stopper = useRef<AbortController>(undefined);

  /**
   * Stops an upload on the server and removes what it had, as nothing here will
   * finish it now. A write the stop cut short may still be ending there, so that's
   * waited for. What can't be stopped goes on its own after a day.
   */
  const letGo = (uploadId: string) => {
    const attempt = async (tries: number): Promise<void> => {
      try {
        await client.cancelUpload({ serverId, uploadId });
      } catch (err) {
        if (reasonOf(err) !== "upload_offset" || tries >= 3) return;
        await new Promise((resolve) => setTimeout(resolve, 1000));
        await attempt(tries + 1);
      }
    };
    void attempt(0);
  };
  const unfinished = (item: Item) => (item.uploadId && item.stage !== "done" ? item.uploadId : undefined);
  // Leaving stops what's under way, and lets go of what reached the server.
  const leaving = useRef<() => void>(undefined);
  leaving.current = () => {
    stopper.current?.abort();
    for (const item of items.current) {
      const id = unfinished(item);
      if (id) letGo(id);
    }
  };
  useEffect(() => () => leaving.current?.(), []);

  const patch = (key: number, change: Partial<Item>) => {
    items.current = items.current.map((item) => (item.key === key ? { ...item, ...change } : item));
    setShown(items.current);
  };
  const add = (more: Chosen) => {
    items.current = [...items.current, ...itemsOf(more)];
    setShown(items.current);
  };
  const remove = (key: number) => {
    const left = items.current.find((item) => item.key === key);
    const id = left && unfinished(left);
    if (id) letGo(id);
    items.current = items.current.filter((item) => item.key !== key);
    setShown(items.current);
  };

  /** What a file that didn't upload says, in words that say what to do. */
  const failure = (err: unknown, item: Item, path: string): string => {
    const name = item.file.name;
    if (err instanceof StreamError && err.reason !== "upload_gone") {
      if (err.status === 0) {
        return t`The connection dropped before ${name} was all sent. What arrived is kept for a day: upload it again to go on from there.`;
      }
      return (
        err.message ||
        t`The control plane answered ${err.status}, so ${name} stopped. Upload it again to go on from where it got.`
      );
    }
    switch (reasonOf(err)) {
      case "file_exists":
        return t`Something called ${name} is in this folder already, so nothing was uploaded. Rename one of them first.`;
      case "too_large":
      case "upload_gone":
        return sentence(err);
    }
    return problem(err, path);
  };

  /** Where the server says an upload got, or nothing when it can't say now. */
  const whereItGot = async (uploadId: string, signal: AbortSignal): Promise<number | undefined> => {
    try {
      return Number((await client.beginUpload({ serverId, uploadId }, { signal })).upload?.receivedBytes ?? 0);
    } catch (err) {
      if (signal.aborted || !isTransient(err)) throw err;
      return undefined;
    }
  };

  const send = async (item: Item, signal: AbortSignal) => {
    const path = join(folder, item.file.name);
    const size = item.file.size;
    const kind = archiveKind(item.file.name);
    patch(item.key, { stage: "sending", problem: undefined });
    try {
      let id = item.uploadId;
      let offset = 0;
      if (id) {
        try {
          offset = (await whereItGot(id, signal)) ?? 0;
        } catch (err) {
          // Gone, as a day passed: it starts again.
          if (reasonOf(err) !== "upload_gone") throw err;
          id = undefined;
        }
      }
      if (!id) {
        const as = unpack && kind ? (kind === "zip" ? Unpack.ZIP : Unpack.TAR_GZ) : Unpack.UNSPECIFIED;
        const began = await withStepUpIfAsked(
          transport,
          StepUpAction.MAKE_WAY_BACK_IN,
          `${serverId}:${path}`,
          (proof) => client.beginUpload({ serverId, path, sizeBytes: BigInt(size), unpack: as, proof }, { signal }),
        );
        id = began.upload?.uploadId ?? "";
        offset = Number(began.upload?.receivedBytes ?? 0);
        patch(item.key, { uploadId: id });
      }
      patch(item.key, { sent: offset });
      let tries = 0;
      while (offset < size) {
        const from = offset;
        const end = Math.min(size, from + PART_BYTES);
        try {
          await uploadFilePart({
            serverId,
            uploadId: id,
            offset: from,
            part: item.file.slice(from, end),
            onSent: (bytes) => patch(item.key, { sent: from + bytes, busy: false }),
            signal,
          });
          offset = end;
          tries = 0;
        } catch (err) {
          // A part that didn't arrive, or arrived in part, goes again from where the
          // server got. Anything else is the file's to say.
          // One the server had no room for waits, and says so.
          const dropped = err instanceof StreamError && transient(err);
          const elsewhere = err instanceof StreamError && err.reason === "upload_offset";
          const busy = err instanceof StreamError && err.reason === "transfers_busy";
          if (signal.aborted || !(dropped || elsewhere || busy) || ++tries > RETRIES) throw err;
          if (busy) patch(item.key, { busy: true });
          if (dropped || busy) await pause(Math.min(16_000, 1000 * 2 ** (tries - 1)), signal);
          const got = await whereItGot(id, signal);
          if (got === undefined) continue;
          // Sent from where it got, and refused all the same: that's for the file to say.
          if (elsewhere && got === from) throw err;
          offset = got;
        }
        patch(item.key, { sent: offset });
      }
      patch(item.key, { stage: "finishing", busy: false });
      try {
        const done = await client.finishUpload({ serverId, uploadId: id });
        patch(item.key, {
          stage: "done",
          unpackedTo: done.unpackedTo || undefined,
          unpackedEntries: Number(done.unpackedEntries),
        });
      } catch (err) {
        // It's in place, and not unpacked: both are worth saying.
        if (reasonOf(err) !== "unpack_refused") throw err;
        patch(item.key, { stage: "done", unpackProblem: sentence(err) });
      }
    } catch (err) {
      if (signal.aborted) {
        patch(item.key, { stage: "stopped", busy: false });
        return;
      }
      patch(item.key, { stage: "failed", busy: false, problem: failure(err, item, path) });
    }
  };

  const start = async (only?: number) => {
    if (running) return;
    const stop = new AbortController();
    stopper.current = stop;
    setRunning(true);
    const queue = items.current.filter((item) => (only === undefined ? toSend(item) : item.key === only));
    for (const item of queue) patch(item.key, { stage: "waiting" });
    for (const item of queue) {
      if (stop.signal.aborted) {
        patch(item.key, { stage: item.uploadId ? "stopped" : "ready" });
        continue;
      }
      // What the item is now, with the upload it began, if it did.
      await send(items.current.find((i) => i.key === item.key) ?? item, stop.signal);
      await changed();
    }
    setRunning(false);
  };

  const pick = (list: FileList | null) => {
    if (list?.length) add({ files: Array.from(list), folders: [] });
    if (picker.current) picker.current.value = "";
  };
  const drop = (event: DragEvent) => {
    event.preventDefault();
    setOver(false);
    if (!running) add(chosenFrom(event.dataTransfer));
  };

  const pending = shown.filter(toSend);
  const archives = shown.some((item) => item.stage === "ready" && archiveKind(item.file.name));
  const finished = shown.length > 0 && pending.length === 0 && !running;
  const limit = formatBytes(MAX_UPLOAD_BYTES, i18n.locale);
  return (
    <Sheet
      open
      title={t`Upload to ${nameOf(folder)}`}
      subtitle={folder}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This goes in the activity log</Trans>
          </span>
          {running ? (
            <Button variant="ghost" icon="stop" onClick={() => stopper.current?.abort()}>
              <Trans>Stop</Trans>
            </Button>
          ) : finished ? (
            <Button variant="primary" onClick={onClose}>
              <Trans>Done</Trans>
            </Button>
          ) : (
            <>
              <Button variant="ghost" onClick={onClose}>
                <Trans>Cancel</Trans>
              </Button>
              <Button variant="primary" icon="upload" disabled={pending.length === 0} onClick={() => void start()}>
                {plural(pending.length, { one: "Upload # file", other: "Upload # files" })}
              </Button>
            </>
          )}
        </>
      }
    >
      <div className="files-sheet">
        {running ? null : (
          // biome-ignore lint/a11y/noStaticElementInteractions: a drop target, with its button for the keyboard
          <div
            className="files-drop"
            data-over={over || undefined}
            onDragOver={(event) => {
              if (!carriesFiles(event)) return;
              event.preventDefault();
              setOver(true);
            }}
            onDragLeave={() => setOver(false)}
            onDrop={drop}
          >
            <Icon name="upload" size={20} />
            <p className="files-sheet-line">
              <Trans>Drop files here, or choose them.</Trans>
            </p>
            <Button size="sm" onClick={() => picker.current?.click()}>
              <Trans>Choose files</Trans>
            </Button>
            <input
              ref={picker}
              type="file"
              multiple
              hidden
              aria-label={t`Choose files to upload`}
              onChange={(event) => pick(event.target.files)}
            />
            <p className="files-drop-note">
              <Trans>
                Up to {limit} a file. A file goes in under its own name, and never replaces one that's there.
              </Trans>
            </p>
          </div>
        )}
        {shown.length > 0 ? (
          <ul className="files-uploads" aria-label={t`Files to upload`}>
            {shown.map((item) => (
              <UploadRow
                key={item.key}
                item={item}
                folder={folder}
                running={running}
                unpack={unpack}
                onRemove={() => remove(item.key)}
                onRetry={() => void start(item.key)}
              />
            ))}
          </ul>
        ) : null}
        {archives ? (
          <Switch
            checked={unpack}
            onChange={setUnpack}
            disabled={running}
            label={t`Unpack archives`}
            description={t`Each .zip, .tar.gz or .tgz also unpacks into a new folder beside it, named after it. Links and anything that could let someone onto the server stop it.`}
          />
        ) : null}
      </div>
    </Sheet>
  );
}

/** One file of an upload: its name, how far it got, and what it came to. */
function UploadRow({
  item,
  folder,
  running,
  unpack,
  onRemove,
  onRetry,
}: {
  item: Item;
  folder: string;
  running: boolean;
  /** Whether archives unpack, as the switch says. */
  unpack: boolean;
  onRemove: () => void;
  onRetry: () => void;
}) {
  const { t, i18n } = useLingui();
  const name = item.file.name;
  const size = item.file.size;
  const [part, whole] = formatBytesOf(item.sent, size, i18n.locale);
  const percent = size === 0 ? 100 : Math.floor((item.sent / size) * 100);
  const limit = formatBytes(MAX_UPLOAD_BYTES, i18n.locale);
  const status = (() => {
    switch (item.stage) {
      case "ready":
        return unpack && archiveKind(name)
          ? t`${formatBytes(size, i18n.locale)} · unpacks into ${unpackedName(name)}`
          : formatBytes(size, i18n.locale);
      case "waiting":
        return t`Waiting · ${formatBytes(size, i18n.locale)}`;
      case "sending":
        if (item.busy) return t`Waiting for the server, which is busy with other uploads and downloads`;
        return t`${formatPercent(percent, i18n.locale)} · ${part} of ${whole}`;
      case "finishing":
        return t`Putting it in place`;
      case "done":
        if (item.unpackProblem) return t`Uploaded, and not unpacked`;
        if (item.unpackedTo) {
          const into = nameOf(item.unpackedTo);
          const entries = item.unpackedEntries ?? 0;
          return plural(entries, {
            one: `Uploaded, and unpacked # item into ${into}`,
            other: `Uploaded, and unpacked # items into ${into}`,
          });
        }
        return t`Uploaded · ${formatBytes(size, i18n.locale)}`;
      case "stopped":
        return t`Stopped at ${part} of ${whole}`;
      case "failed":
        return item.sent > 0 ? t`Stopped at ${part} of ${whole}` : t`Not uploaded`;
      case "tooLarge":
        return t`${formatBytes(size, i18n.locale)}, more than the ${limit} an upload can be. Copy it over SSH.`;
      case "folder":
        return t`A folder doesn't upload as it is. Make a .zip of it, then upload that with Unpack archives on.`;
    }
  })();
  const icon = item.stage === "done" ? "check" : item.stage === "folder" ? "folder" : "file";
  return (
    <li className="files-upload-row" data-stage={item.stage}>
      <span className="files-icon">
        <Icon name={icon} size={15} />
      </span>
      <span className="files-deleted-what">
        <span className="files-entry-name" title={join(folder, name)}>
          {name}
        </span>
        <span className="files-deleted-meta">{status}</span>
        {item.stage === "sending" || item.stage === "finishing" || item.stage === "stopped" ? (
          <progress className="files-progress" max={size || 1} value={item.sent} aria-label={t`Sent of ${name}`} />
        ) : null}
        {item.problem ? <span className="files-upload-problem">{item.problem}</span> : null}
        {item.unpackProblem ? <span className="files-upload-problem">{item.unpackProblem}</span> : null}
      </span>
      <span className="files-deleted-actions">
        {!running && (item.stage === "failed" || item.stage === "stopped") ? (
          <Button size="sm" icon="refresh" onClick={onRetry}>
            {item.sent > 0 ? <Trans>Go on</Trans> : <Trans>Try again</Trans>}
          </Button>
        ) : null}
        {!running && item.stage !== "done" ? (
          <Button size="sm" variant="ghost" icon="x" aria-label={t`Leave out ${name}`} onClick={onRemove} />
        ) : null}
      </span>
    </li>
  );
}
