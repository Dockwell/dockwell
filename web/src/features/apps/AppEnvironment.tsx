// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { type Change, ChangeState, type EnvironmentVariable } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Switch,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { onApple } from "../../lib/platform";
import { useTimeWords } from "../../lib/time";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useControlPlaneNow } from "../servers/useFleet";
import { RevealsProvider, useReveals } from "../variables/useReveal";
import { SecretValue, useAllVariables } from "../variables/VariablesPage";
import { useApp } from "./AppContext";
import { environmentOf, isMoving, nameOf } from "./app";
import { alsoRestarts, type EnvEdit, type EnvRow, envCounts, envEdits, envRows } from "./envEdit";
import { parseEnv, validEnvName } from "./form";

/**
 * What one app runs with, to change: the variables and secrets of its
 * environment it uses, by the names it sees them under, and what its own spec
 * sets. A new value goes into its environment, as a variable or a secret, and
 * the app uses it from there; one there already can be used as it is. Nothing
 * changes until a plan is reviewed and applied: each edit waits in the list,
 * and Review plan plans them together. No response carries a secret's value, or
 * one of the app's own, so those are hidden, and an edit keeps what it doesn't
 * change without reading it back.
 */
export function AppEnvironment() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const sheet = useChangeSheet();
  const name = nameOf(service);
  const environment = environmentOf(service);
  const store = useAllVariables(environment);
  // A secret it uses reveals here as on Variables, with a passkey, for a minute.
  const reveals = useReveals();
  const [pending, setPending] = useState<ReadonlyMap<string, EnvEdit>>(new Map());
  const [changing, setChanging] = useState<string>();
  const [importing, setImporting] = useState(false);
  const rows = useMemo(() => envRows(service, pending, store.data?.variables), [service, pending, store.data]);
  const moving = isMoving(state);
  const readable = service.spec !== undefined;

  const put = useCallback((edit: EnvEdit) => {
    setPending((was) => new Map(was).set(edit.name, edit));
  }, []);
  const revert = useCallback(
    (key: string) =>
      setPending((was) => {
        const next = new Map(was);
        next.delete(key);
        return next;
      }),
    [],
  );

  const review = useCallback(() => {
    if (pending.size === 0 || moving) return;
    const only = pending.size === 1 ? [...pending.values()][0] : undefined;
    const key = only?.name ?? "";
    const summary =
      only?.op === "add" || only?.op === "use"
        ? t`Add ${key} to ${name}`
        : only?.op === "change"
          ? t`Change ${key} on ${name}`
          : only?.op === "remove"
            ? t`Remove ${key} from ${name}`
            : t`Change the environment of ${name}`;
    sheet.open({
      kind: "propose",
      summary,
      edits: envEdits(service, pending),
      onClose: (change: Change | undefined) => {
        // Applied, or applying: what was pending is the app's now, or on its way.
        if (change && (change.state === ChangeState.APPLIED || change.state === ChangeState.APPLYING)) {
          setPending(new Map());
        }
      },
      fix: (reason) =>
        reason === "plan_stale"
          ? t`${name} changed after you started. Close this and review your edits again: they apply to what it is now.`
          : undefined,
    });
  }, [pending, moving, service, sheet, name, t]);

  // ⌘↵ or Ctrl ↵ reviews what's pending, from anywhere on the page.
  useEffect(() => {
    if (pending.size === 0) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Enter" && (onApple() ? event.metaKey : event.ctrlKey)) {
        event.preventDefault();
        review();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [pending.size, review]);

  const counts = useCountsWords(rows);
  const columns = useColumns(
    phone,
    (row) => setChanging(row.name),
    (row) => put({ op: "remove", name: row.name }),
    revert,
    store.isPending,
  );

  if (!readable) {
    return (
      <Callout tone="warning" title={t`${name}'s spec can't be read`}>
        {service.specUnreadable}
      </Callout>
    );
  }

  return (
    <RevealsProvider value={reveals}>
      <div className="app-environment">
        <div className="app-env-head">
          <span className="app-env-counts">{counts}</span>
          <span className="app-env-spacer" />
          <Button variant="ghost" icon="upload" onClick={() => setImporting(true)}>
            <Trans>Import .env</Trans>
          </Button>
        </div>

        <AddForm
          key={changing ?? "add"}
          name={name}
          rows={rows}
          store={store.data?.variables}
          changing={rows.find((row) => row.name === changing)}
          onAdd={put}
          onDone={() => setChanging(undefined)}
        />

        <Panel flush>
          <Table
            columns={columns}
            rows={rows.map((row) => ({ ...row, id: row.name }))}
            rowClassName={(row) => (row.pending?.op === "remove" ? "app-env-removed" : undefined)}
            density="compact"
            caption={t`Environment variables of ${name}`}
            empty={
              <p className="app-env-empty">
                <Trans>
                  This app runs with only what its image sets. Add a variable above, and it gets it once you apply the
                  plan.
                </Trans>
              </p>
            }
          />
        </Panel>

        <p className="app-note">
          <Trans>
            A variable or secret lives in its environment, and goes only to the servers that run an app using it. Change
            one here or in{" "}
            <Link to="/variables" search={{ environment }}>
              Variables and secrets
            </Link>
            , and its plan restarts each app that uses it. What's set on this app alone is never shown again, as it may
            hold secrets.
          </Trans>
        </p>

        {pending.size > 0 ? (
          <PendingBar
            name={name}
            pending={pending}
            others={alsoRestarts(service, rows)}
            root={service.hostAccess.some((a) => a.sameAsRoot)}
            moving={moving}
            phone={phone}
            onDiscard={() => setPending(new Map())}
            onReview={review}
          />
        ) : null}

        {importing ? (
          <ImportSheet
            rows={rows}
            onClose={() => setImporting(false)}
            onImport={(edits) => {
              setPending((was) => {
                const next = new Map(was);
                for (const edit of edits) next.set(edit.name, edit);
                return next;
              });
              setImporting(false);
            }}
          />
        ) : null}
      </div>
    </RevealsProvider>
  );
}

/** "3 variables · 1 secret · 2 set on this app", for what it has now. */
function useCountsWords(rows: readonly EnvRow[]): string {
  const { t } = useLingui();
  let variables = 0;
  let secrets = 0;
  let own = 0;
  for (const row of rows) {
    if (row.now?.kind === "variable") variables++;
    else if (row.now?.kind === "secret") secrets++;
    else if (row.now?.kind === "own") own++;
  }
  const parts = [
    plural(variables, { one: "# variable", other: "# variables" }),
    plural(secrets, { one: "# secret", other: "# secrets" }),
  ];
  if (own > 0) parts.push(t`${own} set on this app`);
  return parts.join(" · ");
}

const MARK: Record<EnvEdit["op"], string> = { add: "+", use: "+", change: "~", remove: "−" };

function useColumns(
  phone: boolean,
  onChange: (row: EnvRow) => void,
  onRemove: (row: EnvRow) => void,
  onRevert: (name: string) => void,
  loading: boolean,
): TableColumn<EnvRow & { id: string }>[] {
  const { t } = useLingui();
  const key = (row: EnvRow) => {
    const op = row.pending?.op;
    const said =
      op === "add" || op === "use"
        ? t`added, not applied`
        : op === "change"
          ? t`changed, not applied`
          : op === "remove"
            ? t`removed, not applied`
            : "";
    return (
      <span className="app-env-key">
        <span aria-hidden="true" className={cx("app-env-mark", op && `is-${op}`)}>
          {op ? MARK[op] : "·"}
        </span>
        <code className="app-env-name">{row.name}</code>
        {said ? <span className="dw-sr">, {said}</span> : null}
      </span>
    );
  };
  const actions = (row: EnvRow) => {
    const name = row.name;
    return row.pending ? (
      <Button size="sm" variant="ghost" icon="undo" aria-label={t`Revert ${name}`} onClick={() => onRevert(row.name)} />
    ) : (
      <span className="app-env-actions">
        <Button size="sm" variant="ghost" icon="edit" aria-label={t`Change ${name}`} onClick={() => onChange(row)} />
        <Button size="sm" variant="ghost" icon="trash" aria-label={t`Remove ${name}`} onClick={() => onRemove(row)} />
      </span>
    );
  };
  if (phone) {
    return [
      {
        key: "who",
        header: t`Key`,
        render: (row) => (
          <>
            {key(row)}
            <span className="app-env-phone-value">
              <Value row={row} loading={loading} />
            </span>
            <span className="app-env-source">
              <Source row={row} />
            </span>
          </>
        ),
      },
      { key: "more", header: <span className="dw-sr">{t`Actions`}</span>, width: 72, align: "right", render: actions },
    ];
  }
  return [
    { key: "name", header: t`Key`, width: "28%", render: key },
    { key: "value", header: t`Value`, render: (row) => <Value row={row} loading={loading} /> },
    { key: "source", header: t`Source`, width: 200, muted: true, render: (row) => <Source row={row} /> },
    { key: "updated", header: t`Updated`, width: 160, render: (row) => <Updated row={row} /> },
    { key: "more", header: <span className="dw-sr">{t`Actions`}</span>, width: 80, align: "right", render: actions },
  ];
}

/** Dots for a value no one reads here. */
function Masked({ label }: { label?: ReactNode }) {
  const { t } = useLingui();
  return (
    <span className="app-env-masked-row">
      <span className="app-env-masked" role="img" aria-label={t`Hidden`}>
        ••••••••••••
      </span>
      {label ? <span className="app-muted">{label}</span> : null}
    </span>
  );
}

function Value({ row, loading }: { row: EnvRow; loading: boolean }) {
  const { t } = useLingui();
  const edit = row.pending;
  const kind = edit?.op === "add" || edit?.op === "use" ? (edit.secret ? "secret" : "variable") : row.now?.kind;
  if (edit?.op === "remove") return <span className="app-muted">{t`Removed`}</span>;
  if (edit?.op === "add") {
    return kind === "secret" ? <Masked label={t`New secret`} /> : <code className="app-env-to">{edit.value}</code>;
  }
  if (edit?.op === "change") {
    if (kind !== "variable") return <Masked label={t`New value`} />;
    return (
      <span className="app-env-diff">
        <code className="app-env-from">{row.stored?.value ?? ""}</code>
        <Icon name="arrow-right" size={12} />
        <code className="app-env-to">{edit.value}</code>
      </span>
    );
  }
  if (kind === "own") return <Masked />;
  if (row.stored) {
    if (row.stored.secret) return <SecretValue variable={row.stored} />;
    return <code className="app-env-value">{row.stored.value}</code>;
  }
  if (kind === "secret") return <Masked />;
  return <span className="app-muted">{loading ? "…" : t`Not loaded`}</span>;
}

function Source({ row }: { row: EnvRow }) {
  const { t } = useLingui();
  const edit = row.pending;
  if (edit?.op === "add") return <>{edit.secret ? t`New secret` : t`New variable`}</>;
  if (edit?.op === "use") {
    const from = edit.from;
    return <>{edit.secret ? t`Secret ${from}` : t`Variable ${from}`}</>;
  }
  const now = row.now;
  if (!now) return null;
  const from = now.from;
  const named = from !== now.name;
  switch (now.kind) {
    case "variable":
      return <>{named ? t`Variable ${from}` : t`Variable`}</>;
    case "secret":
      return <>{named ? t`Secret ${from}` : t`Secret`}</>;
    case "own":
      return <>{t`Set on this app`}</>;
  }
}

function Updated({ row }: { row: EnvRow }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  if (row.pending) return <span className="app-muted">{t`Not applied`}</span>;
  const stored = row.stored;
  if (!stored) return <span className="app-muted">—</span>;
  return (
    <span className="app-env-updated">
      {stored.updatedBy?.name ? <span>{stored.updatedBy.name}</span> : null}
      {stored.updateTime ? <span className="app-muted">{words.ago(timestampDate(stored.updateTime), now)}</span> : null}
    </span>
  );
}

interface AddFormProps {
  name: string;
  rows: readonly EnvRow[];
  store: readonly EnvironmentVariable[] | undefined;
  /** The row whose value is changing; a new one when unset. */
  changing: EnvRow | undefined;
  onAdd: (edit: EnvEdit) => void;
  onDone: () => void;
}

/**
 * Adds a variable or secret, or gives one the app has a new value. Neither
 * plans anything yet: each waits in the list for Review plan. A name the app
 * has is changed in its row; one its environment has already is used as it is,
 * rather than changed for every app that uses it.
 */
function AddForm({ name: app, rows, store, changing, onAdd, onDone }: AddFormProps) {
  const { t } = useLingui();
  const [key, setKey] = useState(changing?.name ?? "");
  const [value, setValue] = useState("");
  const [secret, setSecret] = useState(false);
  const [error, setError] = useState<string>();
  const [inStore, setInStore] = useState<EnvironmentVariable>();
  const [use, setUse] = useState("");
  const valueField = useRef<HTMLInputElement>(null);
  const keyField = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (changing) valueField.current?.focus();
  }, [changing]);

  const unused = (store ?? []).filter((v) => !rows.some((row) => row.name === v.name));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setInStore(undefined);
    if (changing) {
      onAdd({ op: "change", name: changing.name, value });
      onDone();
      return;
    }
    const trimmed = key.trim();
    if (!validEnvName(trimmed)) {
      setError(t`Use letters, digits and underscores, starting with a letter or underscore.`);
      keyField.current?.focus();
      return;
    }
    if (rows.some((row) => row.name === trimmed)) {
      setError(t`${app} has ${trimmed} already. Change it in its row below.`);
      keyField.current?.focus();
      return;
    }
    const there = store?.find((v) => v.name === trimmed);
    if (there) {
      setInStore(there);
      setError(
        there.secret
          ? t`The environment has a secret ${trimmed} already. Use it as it is, or choose another name.`
          : t`The environment has a variable ${trimmed} already. Use it as it is, or choose another name.`,
      );
      keyField.current?.focus();
      return;
    }
    onAdd({ op: "add", name: trimmed, value, secret });
    setKey("");
    setValue("");
    setSecret(false);
    setError(undefined);
    keyField.current?.focus();
  };

  const takeStored = (v: EnvironmentVariable) => {
    onAdd({ op: "use", name: v.name, from: v.name, secret: v.secret });
    setKey("");
    setValue("");
    setError(undefined);
    setInStore(undefined);
    setUse("");
  };

  const target = changing?.name ?? "";
  const storedName = inStore?.name ?? "";
  return (
    <Panel flush className="app-env-form-panel">
      <form
        className="app-env-form"
        aria-label={changing ? t`Change ${target}` : t`Add a variable`}
        onSubmit={submit}
        noValidate
      >
        <div className="app-env-field is-key">
          <TextField
            ref={keyField}
            mono
            placeholder="NEW_KEY"
            aria-label={t`Key`}
            value={key}
            disabled={changing !== undefined}
            invalid={error !== undefined}
            onChange={(event) => {
              setKey(event.target.value);
              setError(undefined);
              setInStore(undefined);
            }}
            autoComplete="off"
            spellCheck={false}
          />
        </div>
        <div className="app-env-field is-value">
          <TextField
            ref={valueField}
            mono
            placeholder={changing ? t`new value` : t`value`}
            aria-label={changing ? t`New value of ${target}` : t`Value`}
            value={value}
            onChange={(event) => setValue(event.target.value)}
            autoComplete="off"
            spellCheck={false}
          />
        </div>
        {changing ? null : <Switch label={t`Secret`} checked={secret} onChange={setSecret} />}
        <div className="app-env-form-buttons">
          {changing ? (
            <>
              <Button type="button" variant="ghost" onClick={onDone}>
                <Trans>Cancel</Trans>
              </Button>
              <Button type="submit" icon="check">
                <Trans>Change</Trans>
              </Button>
            </>
          ) : (
            <Button type="submit" icon="plus">
              <Trans>Add</Trans>
            </Button>
          )}
        </div>
        {error ? (
          <p className="app-env-form-error" role="alert">
            {error}
            {inStore ? (
              <Button type="button" size="sm" variant="secondary" onClick={() => takeStored(inStore)}>
                <Trans>Use {storedName}</Trans>
              </Button>
            ) : null}
          </p>
        ) : null}
        {changing ? (
          <p className="app-env-form-note">
            {changing.now?.kind === "own" ? (
              <Trans>What's set on this app isn't shown, so type the whole value it should have.</Trans>
            ) : changing.now?.kind === "secret" ? (
              <Trans>
                The secret's value isn't shown, so type the whole value it should have. Every app that uses it gets it.
              </Trans>
            ) : (
              <Trans>Every app that uses this variable gets the new value.</Trans>
            )}
          </p>
        ) : null}
      </form>
      {!changing && unused.length > 0 ? (
        <div className="app-env-use">
          <SelectField
            aria-label={t`A variable or secret of the environment`}
            value={use}
            onChange={(event) => setUse(event.target.value)}
            options={[
              { value: "", label: t`Use one the environment has…` },
              ...unused.map((v) => {
                const name = v.name;
                return { value: v.id, label: v.secret ? t`${name}, secret` : name };
              }),
            ]}
          />
          <Button
            type="button"
            variant="secondary"
            disabled={use === ""}
            onClick={() => {
              const chosen = unused.find((v) => v.id === use);
              if (chosen) takeStored(chosen);
            }}
          >
            <Trans>Use</Trans>
          </Button>
        </div>
      ) : null}
    </Panel>
  );
}

interface PendingBarProps {
  name: string;
  pending: ReadonlyMap<string, EnvEdit>;
  /** Other apps a changed value restarts too. */
  others: readonly string[];
  /** The app is as good as root on a server, so applying asks for a passkey. */
  root: boolean;
  moving: boolean;
  phone: boolean;
  onDiscard: () => void;
  onReview: () => void;
}

/** What's waiting to be planned, what applying it will do, and the way to plan it. */
function PendingBar({ name, pending, others, root, moving, phone, onDiscard, onReview }: PendingBarProps) {
  const { t } = useLingui();
  const { added, changed, removed } = envCounts(pending);
  const total = pending.size;
  const also = others.join(", ");
  return (
    <section aria-label={t`Unapplied changes`} className="app-env-bar">
      <div className="app-env-bar-text">
        <div className="app-env-bar-head">
          <span className="app-env-bar-title">
            {plural(total, { one: "# unapplied change", other: "# unapplied changes" })}
          </span>
          <span className="app-env-bar-counts">
            {added > 0 ? (
              <span>
                <span className="app-env-mark is-add" aria-hidden="true">
                  +
                </span>{" "}
                <Trans>{added} added</Trans>
              </span>
            ) : null}
            {changed > 0 ? (
              <span>
                <span className="app-env-mark is-change" aria-hidden="true">
                  ~
                </span>{" "}
                <Trans>{changed} changed</Trans>
              </span>
            ) : null}
            {removed > 0 ? (
              <span>
                <span className="app-env-mark is-remove" aria-hidden="true">
                  −
                </span>{" "}
                <Trans>{removed} removed</Trans>
              </span>
            ) : null}
          </span>
        </div>
        <span className="app-env-bar-note">
          {moving
            ? t`A change is applying to ${name}. Review this once it ends.`
            : others.length > 0
              ? t`Review plan shows what this does first. Applying it restarts ${name}, and ${also}, which use what changes too.`
              : t`Review plan shows what this does first. Applying it restarts ${name}.`}
        </span>
        {root ? (
          <span className="app-env-bar-root">
            <Icon name="key" size={14} />
            <Trans>{name} is as good as root on its server, so applying this asks for your passkey.</Trans>
          </span>
        ) : null}
      </div>
      <span className="app-env-spacer" />
      <div className="app-env-bar-buttons">
        <Button variant="ghost" onClick={onDiscard}>
          <Trans>Discard</Trans>
        </Button>
        <Button
          variant="primary"
          icon="commit"
          disabled={moving}
          kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
          onClick={onReview}
        >
          <Trans>Review plan</Trans>
        </Button>
      </div>
    </section>
  );
}

interface ImportSheetProps {
  rows: readonly EnvRow[];
  onClose: () => void;
  onImport: (edits: EnvEdit[]) => void;
}

/**
 * Reads an .env file's text into edits: a name the app has gets the new value,
 * and the rest are added, as variables or, with the switch, as secrets.
 */
function ImportSheet({ rows, onClose, onImport }: ImportSheetProps) {
  const { t } = useLingui();
  const [text, setText] = useState("");
  const [secret, setSecret] = useState(false);
  const [error, setError] = useState<string>();
  const parsed = text.trim() === "" ? undefined : parseEnv(text);
  const count = parsed?.length ?? 0;
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!parsed) {
      setError(t`Each line needs a NAME=value, as an .env file has them. Lines that start with # are skipped.`);
      return;
    }
    onImport(
      parsed.map(({ name, value }): EnvEdit => {
        const has = rows.find((row) => row.name === name && row.now);
        return has ? { op: "change", name, value } : { op: "add", name, value, secret };
      }),
    );
  };
  return (
    <Sheet
      open
      title={t`Import .env`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" type="submit" form="app-env-import" disabled={count === 0}>
            {plural(count, { one: "Add # variable", other: "Add # variables" })}
          </Button>
        </>
      }
    >
      <form id="app-env-import" className="app-env-import" onSubmit={submit} noValidate>
        <label className="app-env-import-label" htmlFor="app-env-import-text">
          <Trans>Paste the file's text</Trans>
        </label>
        <textarea
          id="app-env-import-text"
          className="app-env-import-text"
          rows={10}
          spellCheck={false}
          placeholder={"LOG_LEVEL=info\nDATABASE_URL=postgres://…"}
          value={text}
          aria-invalid={error !== undefined}
          onChange={(event) => {
            setText(event.target.value);
            setError(undefined);
          }}
        />
        {error ? (
          <p className="app-env-form-error" role="alert">
            {error}
          </p>
        ) : (
          <p className="app-env-form-note">
            <Trans>
              A name the app has gets the new value. Nothing changes until you review the plan and apply it.
            </Trans>
          </p>
        )}
        <Switch
          label={t`Keep the new ones as secrets`}
          description={t`Their values are hidden, and revealing one takes your passkey.`}
          checked={secret}
          onChange={setSecret}
        />
      </form>
    </Sheet>
  );
}
