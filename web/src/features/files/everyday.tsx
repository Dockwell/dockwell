// SPDX-License-Identifier: AGPL-3.0-only

import { ConnectError, type Transport } from "@connectrpc/connect";
import { useQuery } from "@connectrpc/connect-query";
import {
  type ArchiveJob,
  type ChangeFileAccessResponse,
  type FileEntry,
  FileService,
  FileType,
  type SearchFilesResponse,
  StepUpNeededSchema,
} from "@dockwell/api";
import { Button, Callout, SelectField, Sheet, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { stepUp } from "../auth/stepUp";
import { useFiles } from "./manage";
import { nameOf } from "./paths";

export async function confirmed<T>(
  transport: Transport,
  call: (proof: string, target: string) => Promise<T>,
): Promise<T> {
  try {
    return await call("", "");
  } catch (error) {
    if (reasonOf(error) !== "needs_step_up") throw error;
    const needed = ConnectError.from(error).findDetails(StepUpNeededSchema)[0];
    if (!needed) throw error;
    return call(await stepUp(transport, needed.action, needed.target), needed.target);
  }
}

type AccessTarget = { path: string; entry: FileEntry };
export type AccessResult = { result: ChangeFileAccessResponse; recursive: boolean };

function permissionBits(mode: string): number {
  let bits = 0;
  for (const [i, char] of [...mode.slice(1, 10)].entries()) {
    if ("rwxst".includes(char)) bits |= 1 << (8 - i);
  }
  if (/[sS]/.test(mode.charAt(3))) bits |= 0o4000;
  if (/[sS]/.test(mode.charAt(6))) bits |= 0o2000;
  if (/[tT]/.test(mode.charAt(9))) bits |= 0o1000;
  return bits;
}

export function PermissionsSheet({
  serverId,
  targets,
  onClose,
  onApplied,
}: {
  serverId: string;
  targets: readonly AccessTarget[];
  onClose: () => void;
  onApplied: (results: readonly AccessResult[], partial: boolean) => void;
}) {
  const { t } = useLingui();
  const { client, transport, problem, changed } = useFiles(serverId);
  const accounts = useQuery(FileService.method.listFileAccounts, { serverId }, { retry: false });
  const first = targets[0];
  const single = targets.length === 1 ? first : undefined;
  const start = first ? permissionBits(first.entry.mode) : 0;
  const uniform = targets.every(({ entry }) => permissionBits(entry.mode) === start);
  const [mode, setMode] = useState(uniform ? start : 0);
  const [setModeOnSave, setSetModeOnSave] = useState(false);
  const [uid, setUID] = useState("");
  const [gid, setGID] = useState("");
  const [recursive, setRecursive] = useState(false);
  const [fileMode, setFileMode] = useState(0o644);
  const [keepFileExecute, setKeepFileExecute] = useState(true);
  const [numericUID, setNumericUID] = useState("");
  const [numericGID, setNumericGID] = useState("");
  const [results, setResults] = useState<readonly AccessResult[]>([]);
  const [acknowledge, setAcknowledge] = useState(false);
  const [working, setWorking] = useState(false);
  const [failures, setFailures] = useState<readonly { path: string; message: string }[]>([]);
  const [applied, setApplied] = useState<ReadonlySet<string>>(() => new Set());
  const setID = setModeOnSave && ((mode | (recursive ? fileMode : 0)) & 0o6000) !== 0;
  const userID = uid === "numeric" ? numericUID : uid;
  const groupID = gid === "numeric" ? numericGID : gid;
  const validID = (id: string) => /^\d+$/.test(id) && Number(id) < 0xffffffff;
  const validAccounts = (uid === "" || validID(userID)) && (gid === "" || validID(groupID));
  const editable = validAccounts && (setModeOnSave || uid !== "" || gid !== "");
  const title = t`Permissions and owner`;
  const permissions = [t`Read`, t`Write`, t`Run`];
  const roles = [t`Owner`, t`Group`, t`Others`];
  const octal = mode.toString(8).padStart(4, "0");

  function toggleRecursion(include: boolean) {
    setRecursive(include);
    if (include && !setModeOnSave) {
      setMode(0o755);
      setFileMode(0o644);
    }
  }

  function modeGrid(value: number, toggle: (bit: number) => void, label?: string) {
    return (
      <table className="files-permissions" aria-label={label}>
        {label ? (
          <caption>
            {label} · <code>{value.toString(8).padStart(4, "0")}</code>
          </caption>
        ) : null}
        <thead>
          <tr>
            <td />
            {permissions.map((word) => (
              <th key={word} scope="col">
                {word}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {roles.map((role, row) => (
            <tr key={role}>
              <th scope="row">{role}</th>
              {permissions.map((word, column) => {
                const bit = 1 << (8 - row * 3 - column);
                return (
                  <td key={word}>
                    <input
                      type="checkbox"
                      aria-label={t`${role}: ${word}`}
                      checked={(value & bit) !== 0}
                      onChange={() => {
                        toggle(value ^ bit);
                        setSetModeOnSave(true);
                      }}
                    />
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    );
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!editable || working || (setID && !acknowledge)) return;
    setWorking(true);
    setFailures([]);
    const failed: { path: string; message: string }[] = [];
    const completed = new Set(applied);
    const nextResults = [...results];
    for (const { path, entry } of targets) {
      if (completed.has(path)) continue;
      const isTree = recursive && entry.type === FileType.DIRECTORY;
      const request = {
        ...(setModeOnSave ? { mode: recursive && !isTree ? fileMode : mode } : {}),
        ...(setModeOnSave && isTree ? { fileMode, keepFileExecute } : {}),
        ...(userID ? { uid: Number(userID) } : {}),
        ...(groupID ? { gid: Number(groupID) } : {}),
        recursive: isTree,
        allowSetId: setID && acknowledge,
      };
      try {
        const result = await confirmed(transport, (proof, confirmationTarget) =>
          client.changeFileAccess({ serverId, path, proof, confirmationTarget, change: request }),
        );
        completed.add(path);
        if (result.changed > 0n) nextResults.push({ result, recursive: isTree });
        if (result.stopped || result.skipped > 0n) {
          failed.push({
            path,
            message:
              result.stopped || plural(Number(result.skipped), { one: "# entry skipped", other: "# entries skipped" }),
          });
        }
      } catch (error) {
        failed.push({ path, message: problem(error, path) });
      }
    }
    setWorking(false);
    setApplied(completed);
    void changed();
    setResults(nextResults);
    setFailures(failed);
    if (!failed.length || nextResults.length) onApplied(nextResults, failed.length > 0);
  }

  const keepOwner = single ? t`Keep ${single.entry.owner}` : t`Keep each owner`;
  const keepGroup = single ? t`Keep ${single.entry.group}` : t`Keep each group`;
  const accountOptions = (items: readonly { name: string; id: number }[] | undefined, keep: string) => [
    { value: "", label: keep },
    ...[...new Map(items?.map((item) => [item.id, item])).values()].map(({ name, id }) => ({
      value: String(id),
      label: `${name} (${id})`,
    })),
    { value: "numeric", label: t`Numeric ID` },
  ];

  return (
    <Sheet
      open
      title={title}
      onClose={working ? undefined : onClose}
      footer={
        <>
          <Button variant="ghost" disabled={working} onClick={onClose}>
            <Trans>Close</Trans>
          </Button>
          <Button
            type="submit"
            form="files-access-form"
            variant="primary"
            loading={working}
            disabled={!editable || (setID && !acknowledge) || applied.size >= targets.length}
          >
            <Trans>Apply changes</Trans>
          </Button>
        </>
      }
    >
      <form id="files-access-form" className="files-sheet" onSubmit={(event) => void submit(event)}>
        <fieldset className="files-access-fields" disabled={working || applied.size > 0}>
          <legend className="dw-sr">{title}</legend>
          <div className="files-access-heading">
            <span>
              {single ? <code>{single.entry.name}</code> : plural(targets.length, { one: "# item", other: "# items" })}
            </span>
            {!recursive ? (
              <output aria-label={t`Octal mode`}>
                <code>{!uniform && !setModeOnSave ? t`Mixed` : octal}</code>
              </output>
            ) : null}
          </div>
          {targets.some(({ entry }) => entry.type === FileType.DIRECTORY) ? (
            <label className="files-access-check">
              <input type="checkbox" checked={recursive} onChange={(event) => toggleRecursion(event.target.checked)} />
              <Trans>And everything inside</Trans>
            </label>
          ) : null}
          {recursive ? (
            <>
              <SelectField
                label={t`Folder and file presets`}
                value={
                  setModeOnSave &&
                  [
                    [0o755, 0o644],
                    [0o750, 0o640],
                    [0o700, 0o600],
                  ].some(([folder, file]) => mode === folder && fileMode === file && keepFileExecute)
                    ? String(mode)
                    : ""
                }
                options={[
                  { value: "", label: setModeOnSave ? t`Custom permissions` : t`Choose a preset` },
                  { value: String(0o755), label: "755 / 644" },
                  { value: String(0o750), label: "750 / 640" },
                  { value: String(0o700), label: "700 / 600" },
                ]}
                onChange={(event) => {
                  if (!event.target.value) return;
                  const folder = Number(event.target.value);
                  setMode(folder);
                  setFileMode(folder & ~0o111);
                  setKeepFileExecute(true);
                  setSetModeOnSave(true);
                }}
              />
              {modeGrid(mode, setMode, t`Folders`)}
              {modeGrid(fileMode, setFileMode, t`Files`)}
              <label className="files-access-check">
                <input
                  type="checkbox"
                  checked={keepFileExecute}
                  onChange={(event) => {
                    setKeepFileExecute(event.target.checked);
                    setSetModeOnSave(true);
                  }}
                />
                <Trans>Keep existing run permissions on files</Trans>
              </label>
            </>
          ) : (
            modeGrid(mode, setMode)
          )}
          {!single ? (
            <label className="files-access-check">
              <input
                type="checkbox"
                checked={setModeOnSave}
                onChange={(event) => setSetModeOnSave(event.target.checked)}
              />
              <Trans>Apply these permissions to every selected item</Trans>
            </label>
          ) : null}
          <div className="files-ownership">
            <SelectField
              label={t`Owner`}
              options={accountOptions(accounts.data?.users, keepOwner)}
              value={uid}
              onChange={(event) => setUID(event.target.value)}
            />
            <SelectField
              label={t`Group`}
              options={accountOptions(accounts.data?.groups, keepGroup)}
              value={gid}
              onChange={(event) => setGID(event.target.value)}
            />
          </div>
          {uid === "numeric" || gid === "numeric" ? (
            <div className="files-ownership">
              {uid === "numeric" ? (
                <TextField
                  label={t`User ID`}
                  type="text"
                  inputMode="numeric"
                  pattern="[0-9]+"
                  required
                  value={numericUID}
                  onChange={(event) => setNumericUID(event.target.value)}
                />
              ) : null}
              {gid === "numeric" ? (
                <TextField
                  label={t`Group ID`}
                  type="text"
                  inputMode="numeric"
                  pattern="[0-9]+"
                  required
                  value={numericGID}
                  onChange={(event) => setNumericGID(event.target.value)}
                />
              ) : null}
            </div>
          ) : null}
          {accounts.error ? (
            <Callout
              tone="warning"
              title={t`Couldn't load users and groups`}
              actions={
                <Button size="sm" variant="secondary" onClick={() => void accounts.refetch()}>
                  <Trans>Retry</Trans>
                </Button>
              }
            >
              {sentence(accounts.error)}
            </Callout>
          ) : null}
          <details className="files-special-mode">
            <summary>
              <Trans>Special permissions</Trans>
            </summary>
            {[
              [0o4000, t`Set-user-ID`],
              [0o2000, t`Set-group-ID`],
              [0o1000, t`Sticky bit`],
            ].map(([bit, label]) => (
              <label className="files-access-check" key={bit}>
                <input
                  type="checkbox"
                  checked={(mode & Number(bit)) !== 0}
                  onChange={() => {
                    setMode(mode ^ Number(bit));
                    setSetModeOnSave(true);
                  }}
                />
                {label}
              </label>
            ))}
          </details>
          {setID ? (
            <Callout tone="warning" title={t`Set-ID changes how programs run`}>
              <label className="files-access-check">
                <input
                  type="checkbox"
                  checked={acknowledge}
                  onChange={(event) => setAcknowledge(event.target.checked)}
                />
                <Trans>Allow programs to run with the file owner's or group's privileges</Trans>
              </label>
            </Callout>
          ) : null}
        </fieldset>
        {failures.length > 0 ? (
          <Callout tone="warning" title={t`Some changes stopped`}>
            <ul className="files-result-list">
              {failures.map(({ path, message }) => (
                <li key={path}>
                  <code>{path}</code> · {message}
                </li>
              ))}
            </ul>
          </Callout>
        ) : null}
      </form>
    </Sheet>
  );
}

function proposedName(raw: string): string {
  try {
    const url = new URL(raw);
    const name = decodeURIComponent(url.pathname.split("/").at(-1) ?? "");
    return /^[^/\\\p{Cc}\p{Cf}]+$/u.test(name) && name !== "." && name !== ".." ? name : "";
  } catch {
    return "";
  }
}

export function DownloadURLSheet({
  serverId,
  folder,
  onClose,
  onDone,
}: {
  serverId: string;
  folder: string;
  onClose: () => void;
  onDone: (job: ArchiveJob) => void;
}) {
  const { t } = useLingui();
  const { client, transport, problem } = useFiles(serverId);
  const [url, setURL] = useState("");
  const [givenName, setName] = useState<string>();
  const [working, setWorking] = useState(false);
  const [error, setError] = useState<string>();
  const name = givenName ?? proposedName(url.trim());

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (working || !url.trim() || !name) return;
    setWorking(true);
    setError(undefined);
    try {
      const result = await confirmed(transport, (proof, confirmationTarget) =>
        client.downloadFileFromURL({ serverId, folder, url: url.trim(), name, proof, confirmationTarget }),
      );
      if (result.job) onDone(result.job);
      else setError(t`The server didn't start the download. Try again.`);
    } catch (error) {
      setError(problem(error, folder));
    } finally {
      setWorking(false);
    }
  }

  return (
    <Sheet
      open
      title={t`Download from a URL`}
      onClose={working ? undefined : onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={working}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            type="submit"
            form="files-url-form"
            variant="primary"
            icon="download"
            loading={working}
            disabled={!url.trim() || !name}
          >
            <Trans>Start download</Trans>
          </Button>
        </>
      }
    >
      <form id="files-url-form" className="files-sheet" onSubmit={(event) => void submit(event)}>
        <code className="files-download-folder">{folder}</code>
        <TextField
          type="url"
          label={t`Download URL`}
          value={url}
          onChange={(event) => setURL(event.target.value)}
          placeholder="https://"
          required
          disabled={working}
        />
        <TextField
          label={t`File name`}
          value={name}
          onChange={(event) => setName(event.target.value)}
          hint={t`Up to 1 GB per download`}
          required
          disabled={working}
        />
        {error ? (
          <Callout tone="critical" title={t`Couldn't start the download`}>
            {error}
          </Callout>
        ) : null}
      </form>
    </Sheet>
  );
}

export function SearchFoldersSheet({
  serverId,
  folder,
  initialQuery,
  onClose,
  onOpen,
}: {
  serverId: string;
  folder: string;
  initialQuery: string;
  onClose: () => void;
  onOpen: (path: string, isFolder: boolean, line?: number) => void;
}) {
  const { t } = useLingui();
  const { client } = useFiles(serverId);
  const [query, setQuery] = useState(initialQuery);
  const [text, setText] = useState(false);
  const [working, setWorking] = useState(false);
  const [result, setResult] = useState<SearchFilesResponse>();
  const [error, setError] = useState<string>();
  const request = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => request.current?.abort(), []);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setWorking(true);
    setResult(undefined);
    setError(undefined);
    try {
      const found = await client.searchFiles(
        { serverId, path: folder, query: query.trim(), text },
        { signal: controller.signal },
      );
      if (!controller.signal.aborted) setResult(found);
    } catch (error) {
      if (!controller.signal.aborted) setError(sentence(error));
    } finally {
      if (request.current === controller && !controller.signal.aborted) setWorking(false);
    }
  }

  return (
    <Sheet open title={t`Search below this folder`} onClose={onClose}>
      <form className="files-sheet" onSubmit={(event) => void submit(event)}>
        <div className="files-search-fields">
          <TextField
            size="sm"
            label={t`Search term`}
            icon="search"
            value={query}
            maxLength={256}
            onChange={(event) => setQuery(event.target.value)}
          />
          <Button type="submit" variant="primary" size="sm" loading={working} disabled={!query.trim()}>
            <Trans>Search</Trans>
          </Button>
        </div>
        <label className="files-access-check">
          <input type="checkbox" checked={text} onChange={(event) => setText(event.target.checked)} />
          <Trans>Search inside text files too</Trans>
        </label>
        {error ? (
          <Callout tone="critical" title={t`Couldn't search this folder`}>
            {error}
          </Callout>
        ) : null}
        {result ? (
          <div aria-live="polite" className="files-search-results">
            {result.partial ? (
              <Callout tone="warning" title={t`Search stopped at its scan limit`}>
                <Trans>Choose a smaller folder or narrow your search.</Trans>
              </Callout>
            ) : null}
            {result.matches.length ? (
              <ul className="files-result-list">
                {result.matches.map((match) => (
                  <li key={match.path}>
                    <Button
                      variant="ghost"
                      icon={match.type === FileType.DIRECTORY ? "folder" : "file"}
                      onClick={() => onOpen(match.path, match.type === FileType.DIRECTORY, match.line || undefined)}
                    >
                      <span className="files-search-match">
                        <code>{nameOf(match.path)}</code>
                        <span>{match.line ? t`${match.path} · line ${match.line}` : match.path}</span>
                      </span>
                    </Button>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="files-sheet-line">
                <Trans>No matches found</Trans>
              </p>
            )}
          </div>
        ) : null}
      </form>
    </Sheet>
  );
}
