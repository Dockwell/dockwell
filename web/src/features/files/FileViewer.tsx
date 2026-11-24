// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { downloadFileURL, type FileEntry, FileService, type ReadFileResponse, StepUpAction } from "@dockwell/api";
import {
  Button,
  ButtonLink,
  Callout,
  cx,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  Skeleton,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { lazy, type RefObject, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isRoleRefusal, reasonOf, sentence } from "../../lib/errors";
import { formatBytes } from "../../lib/format";
import { onApple } from "../../lib/platform";
import { PasskeyError } from "../../lib/webauthn";
import { AddPasskeyHere } from "../auth/AddPasskeyHere";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedInUser } from "../auth/useAuthState";
import type { CodeMirrorEditorHandle } from "./CodeMirrorEditor";
import { type Base, type Draft, dropDraft, heldDraft, holdDraft } from "./drafts";
import { changedLines, editable, lineCount, lineEnding, nameOf, withEndings } from "./paths";
import { readWordWrap, writeWordWrap } from "./preferences";

const LazyCodeMirror = lazy(() => import("./CodeMirrorEditor"));

export interface FileWorkspaceProps {
  serverId: string;
  /** Open file paths in tab order. */
  tabs: readonly string[];
  /** Currently visible tab path. */
  activePath: string;
  /** Marked line for the active file, from 1. */
  line?: number | undefined;
  /** Whether the editor fills the browser window. */
  fullWindow: boolean;
  onToggleFullWindow: () => void;
  onSelectTab: (path: string) => void;
  onCloseTab: (path: string) => void;
  /** Optional preview reads already fetched before opening a tab, to avoid duplicate reads. */
  initialReads?: ReadonlyMap<string, ReadFileResponse>;
  /** Called once a tab reads or saves so stale preview caches can be cleared. */
  onConsumedInitialRead?: (path: string) => void;
}

/**
 * Multi-tab file editor workspace powered by lazy-loaded CodeMirror 6.
 * Supports multiple open tabs with unsaved dot indicators, full-window mode,
 * persisted word wrap, find & replace, and all Dockwell safety rails (drafts,
 * passkey step-up, conflict resolution, symlink swap protection, BOM & line endings).
 */
export function FileViewer({
  serverId,
  tabs,
  activePath,
  line,
  fullWindow,
  onToggleFullWindow,
  onSelectTab,
  onCloseTab,
  initialReads,
  onConsumedInitialRead,
}: FileWorkspaceProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const viewerId = useSignedInUser()?.id ?? "";
  const [wordWrap, setWordWrap] = useState<boolean>(() => readWordWrap(viewerId));
  const [dirtyTabs, setDirtyTabs] = useState<ReadonlySet<string>>(() => new Set());
  const [erroredTabs, setErroredTabs] = useState<ReadonlySet<string>>(() => new Set());
  const editorHandles = useRef(new Map<string, CodeMirrorEditorHandle | null>());
  const saveHandlers = useRef(new Map<string, () => void>());

  useEffect(() => {
    setWordWrap(readWordWrap(viewerId));
  }, [viewerId]);

  // Prune dirty/error tracking for tabs that closed.
  useEffect(() => {
    const openSet = new Set(tabs);
    setDirtyTabs((was) => {
      let changed = false;
      const next = new Set<string>();
      for (const p of was) {
        if (openSet.has(p)) next.add(p);
        else changed = true;
      }
      return changed ? next : was;
    });
    setErroredTabs((was) => {
      let changed = false;
      const next = new Set<string>();
      for (const p of was) {
        if (openSet.has(p)) next.add(p);
        else changed = true;
      }
      return changed ? next : was;
    });
  }, [tabs]);

  // Block leaving the Files route while any tab has unsaved changes.
  const anyDirty = dirtyTabs.size > 0;
  useBlocker({
    shouldBlockFn: ({ current, next }) => {
      // Opening or switching tabs stays on the Files route and keeps dirty tabs mounted;
      // only block leaving the server's files route or closing the last dirty tab via URL.
      const stayingOnFiles =
        next.pathname === current.pathname && typeof (next.search as { file?: unknown }).file === "string";
      if (stayingOnFiles) return false;
      const names = [...dirtyTabs].map(nameOf);
      const prompt =
        names.length === 1
          ? t`Leave ${names[0]} without saving? What you typed goes.`
          : t`Leave ${names.length} unsaved files? What you typed goes.`;
      if (!window.confirm(prompt)) return true;
      for (const p of dirtyTabs) dropDraft(serverId, p);
      return false;
    },
    disabled: !anyDirty,
    enableBeforeUnload: anyDirty,
  });

  // Escape exits full-window mode (unless focus is inside the CodeMirror search panel).
  useEffect(() => {
    if (!fullWindow) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      const target = event.target as HTMLElement | null;
      if (target?.closest(".cm-search")) return;
      event.preventDefault();
      onToggleFullWindow();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [fullWindow, onToggleFullWindow]);

  const setTabDirty = useCallback((path: string, dirty: boolean) => {
    setDirtyTabs((was) => {
      if (was.has(path) === dirty) return was;
      const next = new Set(was);
      if (dirty) next.add(path);
      else next.delete(path);
      return next;
    });
  }, []);

  const setTabError = useCallback((path: string, failed: boolean) => {
    setErroredTabs((was) => {
      if (was.has(path) === failed) return was;
      const next = new Set(was);
      if (failed) next.add(path);
      else next.delete(path);
      return next;
    });
  }, []);

  const requestCloseTab = (path: string) => {
    if (dirtyTabs.has(path)) {
      const name = nameOf(path);
      if (!window.confirm(t`Leave ${name} without saving? What you typed goes.`)) return;
      dropDraft(serverId, path);
      setTabDirty(path, false);
    }
    onCloseTab(path);
  };

  const toggleWrap = () => {
    setWordWrap((was) => {
      const next = !was;
      writeWordWrap(viewerId, next);
      return next;
    });
  };

  const activeName = nameOf(activePath);
  const activeDirty = dirtyTabs.has(activePath);
  const activeErrored = erroredTabs.has(activePath);
  const multiTab = tabs.length > 1;
  const showPanelChrome = !phone || fullWindow;

  return (
    <>
      {fullWindow ? <div className="files-fullwindow-scrim" aria-hidden="true" onClick={onToggleFullWindow} /> : null}
      <Panel
        flush
        className={cx("files-viewer", fullWindow && "is-fullwindow")}
        title={
          showPanelChrome ? (
            <span className="files-viewer-title" title={activePath}>
              <span className="files-icon">
                <Icon name="file" size={16} />
              </span>
              <span className="files-viewer-name">{activeName}</span>
              {activeDirty ? (
                <span className="files-edited">
                  <Trans>Edited</Trans>
                </span>
              ) : null}
            </span>
          ) : undefined
        }
        actions={
          <div className="files-editor-tools">
            {!activeErrored ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  icon="search"
                  aria-label={t`Find and replace`}
                  title={t`Find and replace`}
                  onClick={() => editorHandles.current.get(activePath)?.openSearch()}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  icon="wrap-text"
                  aria-label={t`Word wrap`}
                  aria-pressed={wordWrap}
                  title={wordWrap ? t`Disable word wrap` : t`Enable word wrap`}
                  className={wordWrap ? "is-active" : undefined}
                  onClick={toggleWrap}
                />
              </>
            ) : null}
            {showPanelChrome && !activeErrored ? (
              <ButtonLink
                variant="ghost"
                size="sm"
                icon="download"
                href={downloadFileURL({ serverId, path: activePath })}
                download
                aria-label={t`Download ${activeName}`}
                title={t`Download`}
              />
            ) : null}
            {showPanelChrome ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  icon={fullWindow ? "minimize" : "maximize"}
                  aria-label={fullWindow ? t`Exit full window` : t`Full window editor`}
                  title={fullWindow ? t`Exit full window` : t`Full window`}
                  onClick={onToggleFullWindow}
                />
                <Button
                  variant="ghost"
                  size="sm"
                  icon="x"
                  aria-label={t`Close ${activeName}`}
                  onClick={() => requestCloseTab(activePath)}
                />
              </>
            ) : null}
          </div>
        }
      >
        {multiTab ? (
          <div className="files-tabs" role="tablist" aria-label={t`Open files`}>
            {tabs.map((tabPath) => {
              const tabName = nameOf(tabPath);
              const isCurrent = tabPath === activePath;
              const isDirty = dirtyTabs.has(tabPath);
              return (
                <div key={tabPath} className={cx("files-tab", isCurrent && "is-active")}>
                  <button
                    type="button"
                    role="tab"
                    aria-selected={isCurrent}
                    className="files-tab-btn"
                    title={tabPath}
                    onClick={() => onSelectTab(tabPath)}
                  >
                    <Icon name="file" size={13} />
                    <span className="files-tab-name">{tabName}</span>
                    {isDirty ? (
                      <>
                        <span className="files-tab-dot" title={t`Unsaved changes`} aria-hidden="true" />
                        <span className="dw-sr">
                          <Trans>Unsaved changes</Trans>
                        </span>
                      </>
                    ) : null}
                  </button>
                  <button
                    type="button"
                    className="files-tab-close"
                    aria-label={t`Close tab ${tabName}`}
                    onClick={(event) => {
                      event.stopPropagation();
                      requestCloseTab(tabPath);
                    }}
                  >
                    <Icon name="x" size={12} />
                  </button>
                </div>
              );
            })}
          </div>
        ) : null}

        {tabs.map((tabPath) => (
          <FileTabPane
            key={tabPath}
            serverId={serverId}
            path={tabPath}
            active={tabPath === activePath}
            line={tabPath === activePath ? line : undefined}
            wordWrap={wordWrap}
            initialRead={initialReads?.get(tabPath)}
            onConsumedInitialRead={onConsumedInitialRead}
            onDirtyChange={setTabDirty}
            onErrorChange={setTabError}
            editorHandles={editorHandles}
            saveHandlers={saveHandlers}
          />
        ))}
      </Panel>
    </>
  );
}

interface FileTabPaneProps {
  serverId: string;
  path: string;
  active: boolean;
  line: number | undefined;
  wordWrap: boolean;
  initialRead?: ReadFileResponse | undefined;
  onConsumedInitialRead?: ((path: string) => void) | undefined;
  onDirtyChange: (path: string, dirty: boolean) => void;
  onErrorChange: (path: string, failed: boolean) => void;
  editorHandles: RefObject<Map<string, CodeMirrorEditorHandle | null>>;
  saveHandlers: RefObject<Map<string, () => void>>;
}

function FileTabPane({
  serverId,
  path,
  active,
  line,
  wordWrap,
  initialRead,
  onConsumedInitialRead,
  onDirtyChange,
  onErrorChange,
  editorHandles,
  saveHandlers,
}: FileTabPaneProps) {
  const { t, i18n } = useLingui();
  const [seeded] = useState<ReadFileResponse | undefined>(() => initialRead);
  useEffect(() => {
    if (seeded) onConsumedInitialRead?.(path);
  }, [seeded, path, onConsumedInitialRead]);

  const query = useQuery(
    FileService.method.readFile,
    { serverId, path },
    {
      retry: false,
      gcTime: 0,
      staleTime: Number.POSITIVE_INFINITY,
      refetchOnWindowFocus: false,
      initialData: seeded,
    },
  );
  const name = nameOf(path);
  const read = query.data;
  const [saved, setSaved] = useState<FileEntry>();
  const file = saved ?? read?.file;
  const size = file ? formatBytes(file.sizeBytes, i18n.locale) : "";

  useEffect(() => {
    onErrorChange(path, Boolean(query.error));
  }, [path, query.error, onErrorChange]);

  const download = (
    <ButtonLink size="sm" icon="download" href={downloadFileURL({ serverId, path })} download>
      <Trans>Download</Trans>
    </ButtonLink>
  );

  const registerHandle = useCallback(
    (handle: CodeMirrorEditorHandle | null) => {
      editorHandles.current.set(path, handle);
    },
    [editorHandles, path],
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
        <Loading label={t`Reading ${name}`} className="files-reading">
          {["92%", "64%", "78%", "40%", "86%", "56%", "70%", "32%"].map((width) => (
            <Skeleton key={width} text="code" width={width} />
          ))}
        </Loading>
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
          <div className="files-code">
            <Suspense fallback={<EditorLoading name={name} />}>
              <LazyCodeMirror
                ref={registerHandle}
                fileName={name}
                value={read.content}
                markedLine={line}
                wordWrap={wordWrap}
                readOnly
                ariaLabel={t`Contents of ${name}`}
              />
            </Suspense>
          </div>
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
          <div className="files-code">
            <Suspense fallback={<EditorLoading name={name} />}>
              <LazyCodeMirror
                ref={registerHandle}
                fileName={name}
                value={read.content}
                markedLine={line}
                wordWrap={wordWrap}
                readOnly
                ariaLabel={t`Contents of ${name}`}
              />
            </Suspense>
          </div>
        </>
      );
    }
    return (
      <Editor
        serverId={serverId}
        path={path}
        name={name}
        read={read}
        line={line}
        wordWrap={wordWrap}
        onSaved={setSaved}
        onEdited={(dirty) => onDirtyChange(path, dirty)}
        editorRef={registerHandle}
        saveHandlers={saveHandlers}
      />
    );
  };

  return (
    <div className="files-tab-panel" hidden={!active}>
      {file ? (
        <p className="files-facts">
          <code>{file.owner}</code>
          <code>{file.mode}</code>
          <span>{size}</span>
        </p>
      ) : null}
      {body()}
    </div>
  );
}

function EditorLoading({ name }: { name: string }) {
  const { t } = useLingui();
  return (
    <Loading label={t`Loading editor for ${name}`} className="files-reading">
      {["84%", "62%", "74%", "50%", "68%"].map((w) => (
        <Skeleton key={w} text="code" width={w} />
      ))}
    </Loading>
  );
}

interface EditorProps {
  serverId: string;
  path: string;
  name: string;
  read: ReadFileResponse;
  line: number | undefined;
  wordWrap: boolean;
  onSaved: (file: FileEntry | undefined) => void;
  onEdited: (edited: boolean) => void;
  editorRef: (handle: CodeMirrorEditorHandle | null) => void;
  saveHandlers: RefObject<Map<string, () => void>>;
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

function Editor({
  serverId,
  path,
  name,
  read,
  line,
  wordWrap,
  onSaved,
  onEdited,
  editorRef,
  saveHandlers,
}: EditorProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const [base, setBase] = useState(() => baseOf(read));
  const [draft, setDraft] = useState(base.text);
  const ending = useMemo(() => lineEnding(base.original), [base.original]);
  const [saving, setSaving] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [problem, setProblem] = useState<string>();
  const [noPasskey, setNoPasskey] = useState(false);
  const [mayLand, setMayLand] = useState(false);
  const [moved, setMoved] = useState<string>();
  const [justSaved, setJustSaved] = useState(false);
  const [pickEnding, setPickEnding] = useState<string>();
  const who = useSignedInUser()?.id ?? "";
  const [offer, setOffer] = useState<Draft | undefined>(() => heldDraft(serverId, path, who));
  const edited = draft !== base.text;
  const changes = edited ? changedLines(base.text, draft) : 0;

  const client = useMemo(() => createClient(FileService, transport), [transport]);
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
    setNoPasskey(false);
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
      else if (reason === "no_passkey_here") setNoPasskey(true);
      else {
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

  const triggerSave = () => {
    if (edited && !saving && !conflict && !moved) void save(base.version);
  };
  saveHandlers.current.set(path, triggerSave);

  const reread = () => client.readFile({ serverId, path });

  const saveOver = async () => {
    try {
      const now = await reread();
      if (!now.version) {
        setConflict(false);
        setProblem(t`${name} isn't a text file under 1 MB any more, so it can't be saved here.`);
        return;
      }
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
      {noPasskey ? (
        <div className="files-note">
          <AddPasskeyHere
            title={t`Saving this takes a passkey, and there's none for this address yet`}
            onAdded={() => void save(base.version)}
          />
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
      <div className="files-code">
        <Suspense fallback={<EditorLoading name={name} />}>
          <LazyCodeMirror
            ref={editorRef}
            fileName={name}
            value={draft}
            markedLine={line}
            wordWrap={wordWrap}
            readOnly={offer !== undefined}
            ariaLabel={t`Contents of ${name}`}
            onChange={change}
            onSave={triggerSave}
          />
        </Suspense>
      </div>
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
