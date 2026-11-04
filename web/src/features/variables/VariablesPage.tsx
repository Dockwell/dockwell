// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  type EditSchema,
  type EnvironmentVariable,
  type Resource,
  ResourceService,
  VariableService,
} from "@dockwell/api";
import {
  Button,
  Icon,
  PHONE_QUERY,
  SegmentedControl,
  Switch,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { passkeysAvailable } from "../../lib/webauthn";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { RevealsProvider, useReveal, useReveals } from "./useReveal";
import "./VariablesPage.css";

/** A name a process's environment takes, as the control plane checks it. */
const NAME = /^[A-Za-z_][A-Za-z0-9_]{0,254}$/;

type Kind = "variable" | "secret";

/** The plan sheet's edit for a variable or secret, or for deleting one. */
function edit(kind: Kind, scope: string, name: string, value?: string): MessageInitShape<typeof EditSchema> {
  const resource = { kind, scope, name };
  if (value === undefined) return { resource, delete: true };
  return {
    resource,
    spec:
      kind === "secret"
        ? { kind: { case: "secret", value: { value } } }
        : { kind: { case: "variable", value: { value } } },
  };
}

/** An environment as the switch shows it: its name, with its project's once there are several. */
function environmentLabel(env: Resource, projects: readonly Resource[]): string {
  const name = env.displayName || env.resource?.name || env.id;
  if (projects.length < 2) return name;
  const project = projects.find((p) => p.id === env.resource?.scope);
  return `${project?.displayName || project?.resource?.name || ""} / ${name}`;
}

/** The environment shown when none is chosen: production in the first project, or the first there is. */
function defaultEnvironment(envs: readonly Resource[], projects: readonly Resource[]): Resource | undefined {
  const first = projects[0]?.id;
  return (
    envs.find((e) => e.resource?.scope === first && e.resource?.name === "production") ??
    envs.find((e) => e.resource?.scope === first) ??
    envs[0]
  );
}

export interface VariablesPageProps {
  /** The environment shown, by ID; the default one when unset. */
  environment?: string | undefined;
  onEnvironment: (id: string) => void;
}

/**
 * An environment's variables and secrets, which its services use by name
 * (App-Environment in the mocks, for the environment as a whole until the
 * app screens show an app's share of it). Adding, changing and removing one
 * each open the plan sheet, which lists every service that restarts with
 * the new value. A secret shows hidden; revealing it takes a passkey, and
 * is in the audit log.
 */
export function VariablesPage({ environment, onEnvironment }: VariablesPageProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const envs = useQuery(ResourceService.method.listResources, { kind: "environment" });
  const projectList = useQuery(ResourceService.method.listResources, { kind: "project" });
  const projects = projectList.data?.resources ?? [];
  const environments = envs.data?.resources ?? [];
  const chosen = environments.find((e) => e.id === environment) ?? defaultEnvironment(environments, projects);
  const list = useQuery(
    VariableService.method.listVariables,
    { environment: chosen?.id ?? "" },
    { enabled: chosen !== undefined },
  );
  const [editing, setEditing] = useState<EnvironmentVariable>();
  // Kept here, so a value revealed stays revealed when the layout changes, as when a phone turns.
  const reveals = useReveals();
  const envName = chosen ? environmentLabel(chosen, projects) : "";
  const variables = list.data?.variables ?? [];
  const secrets = variables.filter((v) => v.secret).length;
  const plain = variables.length - secrets;
  const counts = [
    plural(plain, { one: "# variable", other: "# variables" }),
    plural(secrets, { one: "# secret", other: "# secrets" }),
  ].join(" · ");
  const failed = envs.error ?? list.error;

  return (
    <Page
      title={t`Variables and secrets`}
      phoneTitle={t`Variables`}
      phoneSub={envName || undefined}
      back="/more"
      lead={
        envName ? (
          <Trans>
            What the services in {envName} use, by name. Changing a value restarts every service that uses it, once you
            apply its plan.
          </Trans>
        ) : undefined
      }
      className="variables-page"
    >
      <RevealsProvider value={reveals}>
        <section className="variables-section" aria-label={t`Variables and secrets in ${envName}`}>
          <div className="variables-bar">
            {environments.length > 1 && chosen ? (
              <SegmentedControl
                label={t`Environment`}
                size="sm"
                value={chosen.id}
                options={environments.map((e) => ({ value: e.id, label: environmentLabel(e, projects) }))}
                onChange={onEnvironment}
              />
            ) : null}
            {list.data ? <span className="variables-counts">{counts}</span> : null}
          </div>

          {chosen ? (
            <ValueForm
              key={editing ? `${editing.id}:${editing.revision}` : "add"}
              scope={chosen.id}
              envName={envName}
              editing={editing}
              onDone={() => setEditing(undefined)}
            />
          ) : null}

          {failed ? (
            <p className="variables-note" role="alert">
              {sentence(failed)}
            </p>
          ) : !list.data ? (
            <p className="variables-note" aria-busy="true">
              <Trans>Loading variables and secrets</Trans>
            </p>
          ) : variables.length === 0 ? (
            <p className="variables-note variables-empty">
              <Trans>
                No variables or secrets in {envName} yet. Add the first above; a service then uses it by its name.
              </Trans>
            </p>
          ) : phone ? (
            <ul className="variables-list" aria-label={t`Variables and secrets in ${envName}`}>
              {variables.map((v) => (
                <li key={v.id} className="variables-item">
                  <div className="variables-item-head">
                    <KeyCell variable={v} />
                    <RowActions variable={v} scope={chosen?.id ?? ""} onEdit={setEditing} />
                  </div>
                  <ValueCell variable={v} />
                  <UsedBy variable={v} />
                  <Updated variable={v} />
                </li>
              ))}
            </ul>
          ) : (
            <VariablesTable
              variables={variables}
              scope={chosen?.id ?? ""}
              caption={t`Variables and secrets in ${envName}`}
              onEdit={setEditing}
            />
          )}
        </section>
      </RevealsProvider>
    </Page>
  );
}

interface TableProps {
  variables: readonly EnvironmentVariable[];
  scope: string;
  caption: string;
  onEdit: (v: EnvironmentVariable) => void;
}

function VariablesTable({ variables, scope, caption, onEdit }: TableProps) {
  const { t } = useLingui();
  const columns = useMemo<TableColumn<EnvironmentVariable>[]>(
    () => [
      { key: "name", header: t`Key`, width: 280, render: (v) => <KeyCell variable={v} /> },
      { key: "value", header: t`Value`, render: (v) => <ValueCell variable={v} /> },
      { key: "usedBy", header: t`Used by`, width: 200, render: (v) => <UsedBy variable={v} /> },
      { key: "updated", header: t`Updated`, width: 168, render: (v) => <Updated variable={v} /> },
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 96,
        align: "right",
        render: (v) => <RowActions variable={v} scope={scope} onEdit={onEdit} />,
      },
    ],
    [t, scope, onEdit],
  );
  return <Table className="variables-table" columns={columns} rows={variables} caption={caption} />;
}

function KeyCell({ variable }: { variable: EnvironmentVariable }) {
  const { t } = useLingui();
  return (
    <span className="variables-key">
      <code>{variable.name}</code>
      {variable.secret ? (
        <span className="variables-secret-mark" title={t`Secret`}>
          <Icon name="lock" size={12} />
          <span className="dw-sr">{t`, secret`}</span>
        </span>
      ) : null}
    </span>
  );
}

function ValueCell({ variable }: { variable: EnvironmentVariable }) {
  if (!variable.secret) return <code className="variables-value">{variable.value}</code>;
  return <SecretValue variable={variable} />;
}

/** A secret's value: hidden, until a passkey reveals it, for a minute. */
function SecretValue({ variable }: { variable: EnvironmentVariable }) {
  const { t } = useLingui();
  const name = variable.name;
  const shown = useReveal(variable.id);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
    } catch {
      // Clipboard access was refused; the value stays selectable.
    }
  };

  return (
    <span className="variables-secret">
      {shown.value === undefined ? (
        <>
          <span className="variables-masked" role="img" aria-label={t`Hidden`}>
            ••••••••••••
          </span>
          <Button
            size="sm"
            variant="ghost"
            icon="eye"
            loading={shown.busy}
            disabled={!passkeysAvailable()}
            aria-label={t`Reveal ${name}`}
            onClick={shown.reveal}
          >
            <span className="variables-button-word">
              <Trans>Reveal</Trans>
            </span>
          </Button>
        </>
      ) : (
        <>
          <code className="variables-value variables-revealed">{shown.value}</code>
          <Button
            size="sm"
            variant="ghost"
            icon={copied ? "check" : "copy"}
            aria-label={copied ? t`Copied ${name}` : t`Copy ${name}`}
            onClick={() => void copy(shown.value ?? "")}
          />
          <Button size="sm" variant="ghost" icon="x" aria-label={t`Hide ${name}`} onClick={shown.hide} />
          <span className="dw-sr" aria-live="polite">
            {copied ? t`Copied ${name}` : ""}
          </span>
        </>
      )}
      {shown.problem ? (
        <span className="variables-error" role="alert">
          {shown.problem}
        </span>
      ) : null}
    </span>
  );
}

function UsedBy({ variable }: { variable: EnvironmentVariable }) {
  if (variable.usedBy.length === 0) {
    return (
      <span className="variables-muted">
        <Trans>Not used yet</Trans>
      </span>
    );
  }
  return (
    <ul className="variables-users">
      {variable.usedBy.map((use) => (
        <li key={use.serviceId}>
          <Tag icon="service">{use.service === "" ? use.serviceId : use.service}</Tag>
          {use.name !== variable.name ? (
            <span className="variables-muted">
              <Trans>as {use.name}</Trans>
            </span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function Updated({ variable }: { variable: EnvironmentVariable }) {
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const who = variable.updatedBy?.name;
  return (
    <span className="variables-updated">
      {who ? <span>{who}</span> : null}
      {variable.updateTime ? (
        <span className="variables-muted">{words.ago(timestampDate(variable.updateTime), now)}</span>
      ) : null}
    </span>
  );
}

interface RowActionsProps {
  variable: EnvironmentVariable;
  scope: string;
  onEdit: (v: EnvironmentVariable) => void;
}

function RowActions({ variable, scope, onEdit }: RowActionsProps) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const name = variable.name;
  const kind: Kind = variable.secret ? "secret" : "variable";
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: variable.secret ? t`Remove the secret ${name}` : t`Remove the variable ${name}`,
      edits: [edit(kind, scope, name)],
    });
  return (
    <span className="variables-actions">
      <Button size="sm" variant="ghost" icon="edit" aria-label={t`Change ${name}`} onClick={() => onEdit(variable)} />
      <Button size="sm" variant="ghost" icon="trash" aria-label={t`Remove ${name}`} onClick={remove} />
    </span>
  );
}

interface ValueFormProps {
  scope: string;
  envName: string;
  /** The variable or secret being changed; a new one when unset. */
  editing?: EnvironmentVariable | undefined;
  onDone: () => void;
}

/**
 * Adds a variable or secret, or changes one: either way it opens the plan
 * sheet, and nothing changes until the plan is applied. Making a variable a
 * secret, or a secret a variable, is one change that deletes the one and
 * makes the other. A secret's current value is never filled in.
 */
function ValueForm({ scope, envName, editing, onDone }: ValueFormProps) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const [name, setName] = useState(editing?.name ?? "");
  const [value, setValue] = useState(editing && !editing.secret ? editing.value : "");
  const [secret, setSecret] = useState(editing?.secret ?? false);
  const [tried, setTried] = useState(false);
  const valueRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editing) valueRef.current?.focus();
  }, [editing]);

  const trimmed = name.trim();
  const badName =
    trimmed !== "" && !NAME.test(trimmed)
      ? t`Use letters, digits and underscores, starting with a letter or an underscore, such as DATABASE_URL.`
      : undefined;
  const missingValue = secret && value === "" ? t`A secret needs a value.` : undefined;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    setTried(true);
    if (trimmed === "" || badName || missingValue) return;
    const kind: Kind = secret ? "secret" : "variable";
    const edits = [edit(kind, scope, trimmed, value)];
    let summary: string;
    if (!editing) summary = secret ? t`Add the secret ${trimmed}` : t`Add the variable ${trimmed}`;
    else if (editing.secret === secret)
      summary = secret ? t`Change the secret ${trimmed}` : t`Change the variable ${trimmed}`;
    else {
      // One change makes the one and deletes the other, which share the name.
      edits.unshift(edit(editing.secret ? "secret" : "variable", scope, trimmed));
      summary = secret ? t`Make ${trimmed} a secret` : t`Make ${trimmed} a variable`;
    }
    sheet.open({ kind: "propose", summary, edits });
    // The plan holds it now; a secret's value leaves the page.
    setValue("");
    setTried(false);
    if (editing) onDone();
    else setName("");
  };

  return (
    <form
      className="variables-form"
      aria-label={editing ? t`Change ${trimmed}` : t`Add a variable or secret to ${envName}`}
      onSubmit={submit}
      noValidate
    >
      <div className="variables-form-key">
        <TextField
          mono
          aria-label={t`Key`}
          placeholder="NEW_KEY"
          value={name}
          readOnly={editing !== undefined}
          autoComplete="off"
          spellCheck={false}
          error={badName ?? (tried && trimmed === "" ? t`Name it, such as DATABASE_URL.` : undefined)}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      <div className="variables-form-value">
        <TextField
          ref={valueRef}
          mono
          aria-label={t`Value`}
          type={secret ? "password" : "text"}
          placeholder={editing?.secret && secret ? t`New value` : t`value`}
          value={value}
          autoComplete={secret ? "new-password" : "off"}
          spellCheck={false}
          error={tried ? missingValue : undefined}
          onChange={(e) => setValue(e.target.value)}
        />
      </div>
      <Switch label={t`Secret`} checked={secret} onChange={setSecret} />
      <div className="variables-form-actions">
        <Button type="submit" icon={editing ? "commit" : "plus"}>
          {editing ? <Trans>Review plan</Trans> : <Trans>Add</Trans>}
        </Button>
        {editing ? (
          <Button variant="ghost" onClick={onDone}>
            <Trans>Cancel</Trans>
          </Button>
        ) : null}
      </div>
    </form>
  );
}
