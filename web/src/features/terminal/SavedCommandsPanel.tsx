// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type SavedCommand, TerminalService } from "@dockwell/api";
import {
  Button,
  Callout,
  CodeField,
  Loading,
  SegmentedControl,
  Sheet,
  Skeleton,
  Tag,
  TextField,
  Toast,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { type ChangeEvent, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { ActionMenu } from "../shell/ActionMenu";
import {
  commandLineCount,
  type ExportedCommand,
  exportCommandsJson,
  hasDisallowedControlChars,
  looksLikeSecret,
  normalizeCommandBody,
  parseCommandsFile,
} from "./secrets";

export interface SavedCommandsPanelProps {
  /** Whether a sheet overlay is used instead of the desktop side panel. */
  asSheet: boolean;
  open: boolean;
  onClose: () => void;
  /** True when at least one target terminal can accept typed input. */
  canType: boolean;
  /** True when broadcast mode is active (used for empty-target guidance). */
  broadcasting: boolean;
  /** Human label describing where commands will go (active tab or ticked broadcast tabs). */
  targetLabel: string | undefined;
  /** Inserts the command text into the target terminal(s) without pressing Enter; returns an error string if refused. */
  onInsert: (command: string) => string | undefined;
  /** Runs the command text into the target terminal(s) followed by Enter; returns an error string if refused. */
  onRun: (command: string) => string | undefined;
}

type ScopeFilter = "all" | "personal" | "shared";
type ImportScope = "personal" | "file" | "shared";

export function SavedCommandsPanel({
  asSheet,
  open,
  onClose,
  canType,
  broadcasting,
  targetLabel,
  onInsert,
  onRun,
}: SavedCommandsPanelProps) {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(TerminalService, transport), [transport]);

  const query = useQuery(TerminalService.method.listSavedCommands, {}, { enabled: open });
  const commands = query.data?.commands ?? [];

  const [filter, setFilter] = useState("");
  const [scope, setScope] = useState<ScopeFilter>("all");
  const [editing, setEditing] = useState<{ mode: "add" } | { mode: "edit"; command: SavedCommand } | undefined>();
  const [deleting, setDeleting] = useState<string>();
  const [confirmDelete, setConfirmDelete] = useState<SavedCommand>();
  const [undoDeleted, setUndoDeleted] = useState<SavedCommand>();
  const [confirmRun, setConfirmRun] = useState<{ command: SavedCommand; lines: number }>();
  const [importDraft, setImportDraft] = useState<{ filename: string; items: ExportedCommand[] } | undefined>();
  const [panelError, setPanelError] = useState<string>();
  const fileInput = useRef<HTMLInputElement>(null);

  const hasPersonal = commands.some((c) => !c.shared);
  const hasShared = commands.some((c) => c.shared);

  const existingGroups = useMemo(() => {
    const set = new Set<string>();
    for (const c of commands) {
      if (c.group.trim()) set.add(c.group.trim());
    }
    return [...set].sort((a, b) => a.localeCompare(b));
  }, [commands]);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return commands.filter((c) => {
      if (scope === "personal" && c.shared) return false;
      if (scope === "shared" && !c.shared) return false;
      if (!q) return true;
      return (
        c.name.toLowerCase().includes(q) || c.command.toLowerCase().includes(q) || c.group.toLowerCase().includes(q)
      );
    });
  }, [commands, filter, scope]);

  const grouped = useMemo(() => {
    const map = new Map<string, SavedCommand[]>();
    for (const cmd of filtered) {
      const key = cmd.group.trim();
      const bucket = map.get(key) ?? [];
      bucket.push(cmd);
      map.set(key, bucket);
    }
    return [...map.entries()].sort(([a], [b]) => {
      if (a === "" && b !== "") return 1;
      if (b === "" && a !== "") return -1;
      return a.localeCompare(b);
    });
  }, [filtered]);

  const invalidate = () =>
    queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: TerminalService.method.listSavedCommands, cardinality: "finite" }),
    });

  const performDelete = async (cmd: SavedCommand) => {
    setDeleting(cmd.id);
    setPanelError(undefined);
    try {
      await client.deleteSavedCommand({ id: cmd.id, shared: cmd.shared });
      setConfirmDelete(undefined);
      await invalidate();
      if (!cmd.shared) {
        setUndoDeleted(cmd);
      }
    } catch (err) {
      setPanelError(sentence(err));
    } finally {
      setDeleting(undefined);
    }
  };

  const handleUndoDelete = async (cmd: SavedCommand) => {
    setUndoDeleted(undefined);
    setPanelError(undefined);
    try {
      await client.saveCommand({
        id: "",
        name: cmd.name,
        command: cmd.command,
        group: cmd.group,
        shared: cmd.shared,
        allowPlainTextSecret: looksLikeSecret(cmd.command),
      });
      await invalidate();
    } catch (err) {
      setPanelError(sentence(err));
    }
  };

  const handleDeleteClick = (cmd: SavedCommand) => {
    if (cmd.shared) {
      setConfirmDelete(cmd);
      return;
    }
    void performDelete(cmd);
  };

  const handleInsertClick = (cmd: SavedCommand) => {
    setPanelError(undefined);
    const err = onInsert(cmd.command);
    if (err) {
      setPanelError(err);
      return;
    }
    if (asSheet) onClose();
  };

  const handleRunClick = (cmd: SavedCommand) => {
    setPanelError(undefined);
    const lines = commandLineCount(cmd.command);
    if (lines > 1) {
      setConfirmRun({ command: cmd, lines });
      return;
    }
    const err = onRun(cmd.command);
    if (err) {
      setPanelError(err);
      return;
    }
    if (asSheet) onClose();
  };

  const handleConfirmRun = () => {
    if (!confirmRun) return;
    const cmd = confirmRun.command;
    setConfirmRun(undefined);
    setPanelError(undefined);
    const err = onRun(cmd.command);
    if (err) {
      setPanelError(err);
      return;
    }
    if (asSheet) onClose();
  };

  const handleExport = () => {
    const json = exportCommandsJson(filtered.length > 0 ? filtered : commands);
    const blob = new Blob([json], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "dockwell-terminal-commands.json";
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  const handlePickFile = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    setPanelError(undefined);
    try {
      const text = await file.text();
      const parsed = parseCommandsFile(text);
      if (parsed.error) {
        switch (parsed.error) {
          case "invalid_json":
            setPanelError(t`That file isn't valid JSON. Choose a JSON file exported from Dockwell.`);
            break;
          case "empty":
            setPanelError(t`That file has no commands to import.`);
            break;
          case "too_many":
            setPanelError(t`Import at most 200 commands per file.`);
            break;
          case "control_chars":
            setPanelError(t`That file contains disallowed control characters. Remove them and try importing again.`);
            break;
          default:
            setPanelError(t`That file doesn't match the saved commands format. Each item needs a name and a command.`);
        }
        return;
      }
      setImportDraft({ filename: file.name, items: parsed.commands });
    } catch {
      setPanelError(t`Couldn't read that file.`);
    }
  };

  if (!open) {
    return undoDeleted ? (
      <div className="dw-toaster">
        <Toast
          tone="info"
          title={t`Deleted "${undoDeleted.name}"`}
          duration={8000}
          action={{ label: t`Undo`, onClick: () => void handleUndoDelete(undoDeleted) }}
          onClose={() => setUndoDeleted(undefined)}
        />
      </div>
    ) : null;
  }

  const headerActions = (
    <div className="terminal-commands-actions">
      <Button
        variant="ghost"
        size="sm"
        icon="plus"
        onClick={() => {
          setPanelError(undefined);
          setEditing({ mode: "add" });
        }}
      >
        <Trans>Add</Trans>
      </Button>
      <ActionMenu
        label={t`Import or export commands`}
        actions={[
          {
            id: "import",
            label: t`Import from file`,
            icon: "upload",
            onSelect: () => fileInput.current?.click(),
          },
          {
            id: "export",
            label: t`Export to file`,
            icon: "download",
            disabled: commands.length === 0,
            onSelect: handleExport,
          },
        ]}
      />
      {!asSheet ? (
        <Button variant="ghost" size="sm" icon="x" aria-label={t`Close saved commands`} onClick={onClose} />
      ) : null}
    </div>
  );

  const body = (
    <div className="terminal-commands-inner">
      <input
        ref={fileInput}
        type="file"
        accept=".json,application/json"
        className="terminal-sr-file"
        aria-label={t`Import saved commands file`}
        onChange={(e) => void handlePickFile(e)}
      />
      {commands.length > 0 ? (
        <div className="terminal-commands-controls">
          {commands.length > 3 ? (
            <TextField
              size="sm"
              value={filter}
              aria-label={t`Filter saved commands`}
              placeholder={t`Filter commands`}
              onChange={(e) => setFilter(e.target.value)}
            />
          ) : null}
          {hasPersonal && hasShared ? (
            <SegmentedControl<ScopeFilter>
              label={t`Command scope`}
              size="sm"
              value={scope}
              options={[
                { value: "all", label: t`All` },
                { value: "personal", label: t`Personal` },
                { value: "shared", label: t`Shared` },
              ]}
              onChange={setScope}
            />
          ) : null}
        </div>
      ) : null}

      {panelError ? (
        <div className="terminal-commands-callout">
          <Callout tone="critical" title={<Trans>Saved commands</Trans>}>
            {panelError}
          </Callout>
        </div>
      ) : null}

      <div className="terminal-commands-list">
        {query.isLoading ? (
          <Loading label={t`Loading saved commands…`} className="terminal-commands-empty">
            <Skeleton width="55%" />
            <Skeleton width="85%" text="code-sm" />
          </Loading>
        ) : query.isError ? (
          <p className="terminal-commands-empty">{sentence(query.error)}</p>
        ) : commands.length === 0 ? (
          <div className="terminal-commands-empty">
            <p>
              <Trans>No saved commands yet. Save commands you run often to insert or run them in one click.</Trans>
            </p>
            <div className="terminal-commands-empty-actions">
              <Button size="sm" variant="primary" icon="plus" onClick={() => setEditing({ mode: "add" })}>
                <Trans>Add command</Trans>
              </Button>
              <Button size="sm" variant="secondary" icon="upload" onClick={() => fileInput.current?.click()}>
                <Trans>Import</Trans>
              </Button>
            </div>
          </div>
        ) : grouped.length === 0 ? (
          <p className="terminal-commands-empty">
            <Trans>No saved command matches "{filter.trim()}".</Trans>
          </p>
        ) : (
          grouped.map(([groupName, items]) => (
            <div key={groupName || "__ungrouped"} className="terminal-commands-group">
              <div className="terminal-commands-group-head">{groupName || t`General`}</div>
              <ul className="terminal-commands-ul">
                {items.map((cmd) => {
                  const author = cmd.shared ? cmd.updatedBy || cmd.createdBy : "";
                  const authorHint =
                    cmd.shared && cmd.createdBy && cmd.updatedBy && cmd.createdBy !== cmd.updatedBy
                      ? t`Created by ${cmd.createdBy}, updated by ${cmd.updatedBy}`
                      : undefined;
                  return (
                    <li key={cmd.id} className="terminal-command-row">
                      <div className="terminal-command-info">
                        <div className="terminal-command-title-line">
                          <span className="terminal-command-name">{cmd.name}</span>
                          {cmd.shared ? <Tag>{t`Shared`}</Tag> : null}
                          {author ? (
                            <span className="terminal-command-by" title={authorHint}>
                              <Trans>by {author}</Trans>
                            </span>
                          ) : null}
                        </div>
                        <code className="terminal-command-code" title={cmd.command}>
                          {cmd.command}
                        </code>
                      </div>
                      <div className="terminal-command-btns">
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={!canType}
                          title={canType ? t`Type into terminal without pressing Enter` : t`Open a terminal first`}
                          aria-label={t`Insert ${cmd.name}`}
                          onClick={() => handleInsertClick(cmd)}
                        >
                          <Trans>Insert</Trans>
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          icon="play"
                          disabled={!canType}
                          title={canType ? t`Type into terminal and press Enter` : t`Open a terminal first`}
                          aria-label={t`Run ${cmd.name}`}
                          onClick={() => handleRunClick(cmd)}
                        >
                          <Trans>Run</Trans>
                        </Button>
                        <ActionMenu
                          label={t`Options for ${cmd.name}`}
                          actions={[
                            {
                              id: "edit",
                              label: t`Edit`,
                              icon: "edit",
                              onSelect: () => {
                                setPanelError(undefined);
                                setEditing({ mode: "edit", command: cmd });
                              },
                            },
                            {
                              id: "delete",
                              label: t`Delete`,
                              icon: "trash",
                              danger: true,
                              disabled: deleting === cmd.id,
                              onSelect: () => handleDeleteClick(cmd),
                            },
                          ]}
                        />
                      </div>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))
        )}
      </div>

      <p className="terminal-commands-foot">
        {canType && targetLabel ? (
          <Trans>
            Target: <code>{targetLabel}</code>. Insert types without Enter; Run executes immediately.
          </Trans>
        ) : broadcasting ? (
          <Trans>Select at least one tab to insert or run saved commands.</Trans>
        ) : (
          <Trans>Open a terminal session to insert or run saved commands.</Trans>
        )}
      </p>
    </div>
  );

  return (
    <>
      {asSheet ? (
        <Sheet open={open} onClose={onClose} title={t`Saved commands`} footer={headerActions}>
          {body}
        </Sheet>
      ) : (
        <aside className="dw-panel terminal-commands-aside" aria-label={t`Saved commands`}>
          <div className="dw-panel-head terminal-commands-head">
            <h2 className="dw-panel-title">
              <Trans>Saved commands</Trans>
            </h2>
            {headerActions}
          </div>
          {body}
        </aside>
      )}

      {editing ? (
        <CommandEditorSheet
          mode={editing.mode}
          initial={editing.mode === "edit" ? editing.command : undefined}
          existingGroups={existingGroups}
          onClose={() => setEditing(undefined)}
          onSaved={async () => {
            setEditing(undefined);
            await invalidate();
          }}
        />
      ) : null}

      {importDraft ? (
        <ImportCommandsSheet
          filename={importDraft.filename}
          items={importDraft.items}
          onClose={() => setImportDraft(undefined)}
          onImported={async () => {
            setImportDraft(undefined);
            await invalidate();
          }}
        />
      ) : null}

      {confirmRun ? (
        <Sheet
          open
          onClose={() => setConfirmRun(undefined)}
          title={t`Run ${confirmRun.lines} lines?`}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmRun(undefined)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button variant="primary" icon="play" onClick={handleConfirmRun}>
                <Trans>Run {confirmRun.lines} lines</Trans>
              </Button>
            </>
          }
        >
          <div className="terminal-cmd-form">
            <p className="terminal-confirm-copy">
              {targetLabel ? (
                <Trans>
                  This runs all {confirmRun.lines} lines of <strong>{confirmRun.command.name}</strong> in{" "}
                  <code>{targetLabel}</code> right away.
                </Trans>
              ) : (
                <Trans>
                  This runs all {confirmRun.lines} lines of <strong>{confirmRun.command.name}</strong> right away.
                </Trans>
              )}
            </p>
            <pre className="terminal-confirm-code">{confirmRun.command.command}</pre>
          </div>
        </Sheet>
      ) : null}

      {confirmDelete ? (
        <Sheet
          open
          onClose={() => setConfirmDelete(undefined)}
          title={t`Delete shared command?`}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmDelete(undefined)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button
                variant="danger"
                loading={deleting === confirmDelete.id}
                onClick={() => void performDelete(confirmDelete)}
              >
                <Trans>Delete command</Trans>
              </Button>
            </>
          }
        >
          <div className="terminal-cmd-form">
            <p className="terminal-confirm-copy">
              <Trans>
                Deleting <strong>{confirmDelete.name}</strong> removes it for every admin on this install and is
                recorded on Activity.
              </Trans>
            </p>
            <pre className="terminal-confirm-code">{confirmDelete.command}</pre>
          </div>
        </Sheet>
      ) : null}

      {undoDeleted ? (
        <div className="dw-toaster">
          <Toast
            tone="info"
            title={t`Deleted "${undoDeleted.name}"`}
            duration={8000}
            action={{ label: t`Undo`, onClick: () => void handleUndoDelete(undoDeleted) }}
            onClose={() => setUndoDeleted(undefined)}
          />
        </div>
      ) : null}
    </>
  );
}

interface CommandEditorSheetProps {
  mode: "add" | "edit";
  initial: SavedCommand | undefined;
  existingGroups: readonly string[];
  onClose: () => void;
  onSaved: () => Promise<void>;
}

function CommandEditorSheet({ mode, initial, existingGroups, onClose, onSaved }: CommandEditorSheetProps) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(TerminalService, transport), [transport]);

  const [name, setName] = useState(initial?.name ?? "");
  const [group, setGroup] = useState(initial?.group ?? "");
  const [command, setCommand] = useState(initial?.command ?? "");
  const [scope, setScope] = useState<"personal" | "shared">(initial?.shared ? "shared" : "personal");
  const [ackSecret, setAckSecret] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();

  const normalizedCommand = useMemo(() => normalizeCommandBody(command), [command]);
  const hasControlChars = useMemo(
    () =>
      hasDisallowedControlChars(name.trim(), false) ||
      hasDisallowedControlChars(group.trim(), false) ||
      hasDisallowedControlChars(normalizedCommand, true),
    [name, group, normalizedCommand],
  );
  const hasSecret = useMemo(() => looksLikeSecret(normalizedCommand), [normalizedCommand]);
  const canSubmit =
    name.trim().length > 0 &&
    normalizedCommand.trim().length > 0 &&
    !hasControlChars &&
    (!hasSecret || ackSecret) &&
    !saving;

  const submit = async () => {
    if (!canSubmit) return;
    setSaving(true);
    setError(undefined);
    try {
      await client.saveCommand({
        id: initial?.id ?? "",
        name: name.trim(),
        command: normalizedCommand,
        group: group.trim(),
        shared: scope === "shared",
        allowPlainTextSecret: hasSecret && ackSecret,
      });
      await onSaved();
    } catch (err) {
      setError(sentence(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={mode === "edit" ? t`Edit saved command` : t`Add saved command`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={saving} disabled={!canSubmit} onClick={() => void submit()}>
            {mode === "edit" ? <Trans>Save changes</Trans> : <Trans>Save command</Trans>}
          </Button>
        </>
      }
    >
      <div className="terminal-cmd-form">
        <TextField
          label={t`Name`}
          placeholder={t`Tail API logs`}
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <TextField
          label={t`Group`}
          hint={t`Optional. Commands with the same group appear together.`}
          placeholder={t`Diagnostics`}
          list="terminal-command-groups"
          value={group}
          onChange={(e) => setGroup(e.target.value)}
        />
        {existingGroups.length > 0 ? (
          <datalist id="terminal-command-groups">
            {existingGroups.map((g) => (
              <option key={g} value={g} />
            ))}
          </datalist>
        ) : null}
        <CodeField
          label={t`Command`}
          hint={t`Insert types this text without Enter; Run types it and presses Enter.`}
          placeholder="docker logs -f --tail 200 api"
          lines={4}
          value={command}
          onChange={(e) => {
            setCommand(e.target.value);
            if (!looksLikeSecret(e.target.value)) setAckSecret(false);
          }}
        />
        {hasControlChars ? (
          <Callout tone="critical" title={<Trans>Control characters aren't allowed</Trans>}>
            <Trans>Remove control characters (only printable text, newlines and tabs are allowed).</Trans>
          </Callout>
        ) : null}
        <div className="terminal-settings-group">
          <span className="terminal-settings-label">
            <Trans>Who can use it</Trans>
          </span>
          <SegmentedControl<"personal" | "shared">
            label={t`Who can use it`}
            value={scope}
            options={[
              { value: "personal", label: t`Personal (only you)` },
              { value: "shared", label: t`Shared (all admins)` },
            ]}
            onChange={setScope}
          />
        </div>
        {hasSecret ? (
          <Callout tone="warning" title={<Trans>This looks like a secret</Trans>}>
            <div className="terminal-secret-warn">
              <p>
                <Trans>Kept in plain text in Dockwell's database.</Trans>
              </p>
              <label className="terminal-checkbox">
                <input type="checkbox" checked={ackSecret} onChange={(e) => setAckSecret(e.target.checked)} />
                <span>
                  <Trans>Store this command in plain text anyway</Trans>
                </span>
              </label>
            </div>
          </Callout>
        ) : null}
        {error ? (
          <Callout tone="critical" title={<Trans>Couldn't save command</Trans>}>
            {error}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}

interface ImportCommandsSheetProps {
  filename: string;
  items: readonly ExportedCommand[];
  onClose: () => void;
  onImported: () => Promise<void>;
}

function ImportCommandsSheet({ filename, items, onClose, onImported }: ImportCommandsSheetProps) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(TerminalService, transport), [transport]);

  const [importScope, setImportScope] = useState<ImportScope>("personal");
  const [ackShared, setAckShared] = useState(false);
  const [ackSecret, setAckSecret] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();

  const secretItems = useMemo(() => items.filter((item) => looksLikeSecret(item.command)), [items]);
  const keyedItems = useMemo(() => {
    const counts = new Map<string, number>();
    return items.map((item) => {
      const base = `${item.shared ? "s" : "p"}:${item.group}:${item.name}:${item.command}`;
      const n = (counts.get(base) ?? 0) + 1;
      counts.set(base, n);
      return { key: `${base}#${n}`, item };
    });
  }, [items]);
  const hasSecret = secretItems.length > 0;
  const willShareCount = useMemo(
    () => items.filter((item) => (importScope === "file" ? item.shared : importScope === "shared")).length,
    [items, importScope],
  );
  const canSubmit = items.length > 0 && (!hasSecret || ackSecret) && (willShareCount === 0 || ackShared) && !saving;

  const submit = async () => {
    if (!canSubmit) return;
    setSaving(true);
    setError(undefined);
    try {
      await client.importSavedCommands({
        commands: items.map((item) => ({
          name: item.name,
          command: item.command,
          group: item.group,
          shared: importScope === "file" ? item.shared : importScope === "shared",
        })),
        allowPlainTextSecret: hasSecret && ackSecret,
      });
      await onImported();
    } catch (err) {
      setError(sentence(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Import saved commands`}
      subtitle={t`${items.length} commands from ${filename}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={saving} disabled={!canSubmit} onClick={() => void submit()}>
            <Trans>Import {items.length} commands</Trans>
          </Button>
        </>
      }
    >
      <div className="terminal-cmd-form">
        <div className="terminal-settings-group">
          <span className="terminal-settings-label">
            <Trans>Import into</Trans>
          </span>
          <SegmentedControl<ImportScope>
            label={t`Import into`}
            value={importScope}
            options={[
              { value: "personal", label: t`All personal` },
              { value: "file", label: t`Keep file scope` },
              { value: "shared", label: t`All shared` },
            ]}
            onChange={(next) => {
              setImportScope(next);
              setAckShared(false);
            }}
          />
        </div>

        {willShareCount > 0 ? (
          <Callout tone="warning" title={<Trans>Shared commands apply to every admin</Trans>}>
            <div className="terminal-secret-warn">
              <p>
                {plural(willShareCount, {
                  one: "# command will be added to the shared list and recorded on Activity.",
                  other: "# commands will be added to the shared list and recorded on Activity.",
                })}
              </p>
              <label className="terminal-checkbox">
                <input type="checkbox" checked={ackShared} onChange={(e) => setAckShared(e.target.checked)} />
                <span>
                  {plural(willShareCount, {
                    one: "Share # command with all admins on this install",
                    other: "Share # commands with all admins on this install",
                  })}
                </span>
              </label>
            </div>
          </Callout>
        ) : null}

        {hasSecret ? (
          <Callout tone="warning" title={<Trans>Some imported commands look like secrets</Trans>}>
            <div className="terminal-secret-warn">
              <p>
                <Trans>Kept in plain text in Dockwell's database: {secretItems.map((i) => i.name).join(", ")}.</Trans>
              </p>
              <label className="terminal-checkbox">
                <input type="checkbox" checked={ackSecret} onChange={(e) => setAckSecret(e.target.checked)} />
                <span>
                  <Trans>Import these commands in plain text anyway</Trans>
                </span>
              </label>
            </div>
          </Callout>
        ) : null}

        <ul className="terminal-import-preview">
          {keyedItems.map(({ key, item }) => {
            const landsShared = importScope === "file" ? item.shared : importScope === "shared";
            return (
              <li key={key} className="terminal-import-item">
                <div className="terminal-command-title-line">
                  <span className="terminal-command-name">
                    {item.name}
                    {item.group ? <span className="terminal-import-group"> · {item.group}</span> : null}
                  </span>
                  {landsShared ? <Tag>{t`Shared`}</Tag> : item.shared ? <Tag>{t`Shared in file`}</Tag> : null}
                </div>
                <code className="terminal-command-code">{item.command}</code>
              </li>
            );
          })}
        </ul>

        {error ? (
          <Callout tone="critical" title={<Trans>Couldn't import commands</Trans>}>
            {error}
          </Callout>
        ) : null}
      </div>
    </Sheet>
  );
}
