// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type EditSchema,
  type EnvironmentVariable,
  ListVariablesResponseSchema,
  type Resource,
  ResourceService,
  type ServiceInfo,
  VariableService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  StatusBadge,
  Switch,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery as useTanstackQuery } from "@tanstack/react-query";
import { type FormEvent, type Ref, useEffect, useMemo, useRef, useState } from "react";
import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { passkeysAvailable } from "../../lib/webauthn";
import { nameOf } from "../apps/app";
import { cantShare, type EnvRow, envEdits, envRows } from "../apps/envEdit";
import { useServices } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useProjectDelete } from "../projects/deleteProject";
import { useProjects } from "../projects/useProjects";
import { useControlPlaneNow } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { ProjectPicker } from "./ProjectPicker";
import { RevealsProvider, useReveal, useReveals } from "./useReveal";
import "./VariablesPage.css";
import { MayEdit } from "../auth/MayEdit";
import { useMay } from "../auth/may";

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

/** The most ListVariables returns at once. */
const PAGE_SIZE = 1000;

/**
 * Every variable and secret of an environment, read a page at a time and
 * kept as one list, under ListVariables' own query key, so a change applied
 * in the plan sheet reads it again.
 */
export function useAllVariables(environment: string | undefined) {
  const transport = useTransport();
  const input = { environment: environment ?? "" };
  return useTanstackQuery({
    queryKey: createConnectQueryKey({
      schema: VariableService.method.listVariables,
      input,
      transport,
      cardinality: "finite",
    }),
    queryFn: async ({ signal }) => {
      const client = createClient(VariableService, transport);
      const all = create(ListVariablesResponseSchema);
      let pageToken = "";
      do {
        const page = await client.listVariables({ ...input, pageSize: PAGE_SIZE, pageToken }, { signal });
        all.environmentId = page.environmentId;
        all.variables.push(...page.variables);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return all;
    },
    enabled: environment !== undefined,
  });
}

/**
 * An environment as the page names it: its project's name once there are several projects, and
 * its own once its project has several environments, as there's a choice of each only then.
 */
function environmentLabel(env: Resource, projects: readonly Resource[], environments: readonly Resource[]): string {
  const name = env.displayName || env.resource?.name || env.id;
  if (projects.length < 2) return name;
  const project = projects.find((p) => p.id === env.resource?.scope);
  const label = project?.displayName || project?.resource?.name || "";
  const siblings = environments.filter((e) => e.resource?.scope === env.resource?.scope).length;
  return siblings > 1 ? `${label} / ${name}` : label;
}

/** The environment a project opens on: its production, or its first. */
function firstEnvironment(project: string, environments: readonly Resource[]): Resource | undefined {
  const own = environments.filter((e) => e.resource?.scope === project);
  return own.find((e) => e.resource?.name === "production") ?? own[0];
}

/** Where the page remembers the environment its viewer last chose, for the next visit with none named. */
const REMEMBERED = "dockwell.variables.environment";

function remembered(): string | undefined {
  try {
    return window.localStorage.getItem(REMEMBERED) ?? undefined;
  } catch {
    return undefined;
  }
}

function remember(id: string): void {
  try {
    window.localStorage.setItem(REMEMBERED, id);
  } catch {
    // A private window or blocked storage: the next visit opens on the default, which is fine.
  }
}

/** From how many variables and secrets the list has a box to filter them by name. */
const FILTER_FROM = 8;

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
  /** The environment shown, by ID; the last one chosen here, or the default, when unset. */
  environment?: string | undefined;
  /** What the list is filtered by, by name. */
  query?: string | undefined;
  /** An app, by ID: only what it runs with is listed. */
  app?: string | undefined;
  onEnvironment: (id: string, query?: string, app?: string) => void;
}

/**
 * One row of the list: a variable or secret of the environment, which its apps
 * share, or one value an app sets on itself, shown when it holds no secret
 * (ADR 0081). Both lists read from what the app's Environment tab reads, so the
 * two agree on every row.
 */
type Row =
  | { id: string; name: string; shared: EnvironmentVariable }
  | { id: string; name: string; own: { service: ServiceInfo; row: EnvRow } };

/** The rows of an environment: what it shares by name, then each app's own, by app. */
function rowsOf(variables: readonly EnvironmentVariable[], services: readonly ServiceInfo[]): Row[] {
  const out: Row[] = variables.map((v) => ({ id: v.id, name: v.name, shared: v }));
  const apps = [...services].sort((a, b) => nameOf(a).localeCompare(nameOf(b)));
  for (const service of apps) {
    for (const row of envRows(service, new Map(), variables)) {
      if (row.now?.kind === "own" || row.now?.kind === "runtime") {
        out.push({ id: `${service.id}:${row.name}`, name: row.name, own: { service, row } });
      }
    }
  }
  return out;
}

/** Whether a row is what an app runs with. */
function usedByApp(row: Row, app: string): boolean {
  return "shared" in row ? row.shared.usedBy.some((u) => u.serviceId === app) : row.own.service.id === app;
}

/**
 * What an environment's apps run with: its variables and secrets, which they
 * use by name, and each app's own values, by app (App-Environment in the mocks,
 * for the environment as a whole), with where each comes from and who uses it.
 * Adding, changing and removing a shared one each open the plan sheet, which
 * lists every app that restarts with the new value; an app's own value made
 * shared is one plan too, and one changes on its app's page. A secret shows
 * hidden; revealing it takes a passkey, and is in the audit log. One of an
 * app's own that may be a secret shows hidden too, as no response carries it
 * (ADR 0081).
 */
export function VariablesPage({ environment, query, app, onEnvironment }: VariablesPageProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const envs = useQuery(ResourceService.method.listResources, { kind: "environment" });
  const projectList = useQuery(ResourceService.method.listResources, { kind: "project" });
  const projects = projectList.data?.resources ?? [];
  const environments = envs.data?.resources ?? [];
  const views = useProjects().projects;
  const [last] = useState(remembered);
  const chosen =
    environments.find((e) => e.id === environment) ??
    environments.find((e) => e.id === last) ??
    defaultEnvironment(environments, projects);
  const view = views?.find((v) => v.id === chosen?.resource?.scope);
  const siblings = environments.filter((e) => e.resource?.scope === chosen?.resource?.scope);
  // A choice made here is remembered; a link that names an environment isn't one.
  const choose = (id: string, q?: string) => {
    remember(id);
    onEnvironment(id, q);
  };
  const remove = useProjectDelete(projects.length);
  const list = useAllVariables(chosen?.id);
  const [editing, setEditing] = useState<EnvironmentVariable>();
  // Kept here, so a value revealed stays revealed when the layout changes, as when a phone turns.
  const reveals = useReveals(chosen?.id);
  const may = useMay();
  const keyRef = useRef<HTMLInputElement>(null);
  // An empty environment shows its empty state, and the form only once Add the first opens it, for this
  // environment alone, so nothing is said twice.
  const [adding, setAdding] = useState<string>();
  const envName = chosen ? environmentLabel(chosen, projects, environments) : "";
  const services = useServices();
  const here = useMemo(
    () => (services.data ?? []).filter((s) => s.resource?.scope === chosen?.id && s.spec !== undefined),
    [services.data, chosen?.id],
  );
  const all = list.data?.variables ?? [];
  const rows = useMemo(() => rowsOf(list.data?.variables ?? [], here), [list.data, here]);
  const only = here.find((s) => s.id === app);
  const onlyName = only ? nameOf(only) : "";
  const q = (query ?? "").trim().toLowerCase();
  const shown = rows.filter((r) => (!q || r.name.toLowerCase().includes(q)) && (!only || usedByApp(r, only.id)));
  const secrets = all.filter((v) => v.secret).length;
  const plain = all.length - secrets;
  const own = rows.length - all.length;
  const counts = [
    plural(plain, { one: "# variable", other: "# variables" }),
    plural(secrets, { one: "# secret", other: "# secrets" }),
    ...(own > 0 ? [plural(own, { one: "# set on an app", other: "# set on apps" })] : []),
  ].join(" · ");
  const failed = envs.error ?? list.error;
  const empty = list.data !== undefined && rows.length === 0;
  const filter = (next: { q?: string | undefined; app?: string | undefined }) => {
    if (!chosen) return;
    const nq = "q" in next ? next.q : query;
    const na = "app" in next ? next.app : only?.id;
    onEnvironment(chosen.id, nq || undefined, na || undefined);
  };
  const opened = chosen !== undefined && adding === chosen.id;
  const showForm = chosen !== undefined && list.data !== undefined && (!empty || opened || editing !== undefined);
  useEffect(() => {
    if (opened) keyRef.current?.focus();
  }, [opened]);

  return (
    <Page
      title={t`Variables and secrets`}
      phoneTitle={t`Variables`}
      phoneSub={envName || undefined}
      back="/more"
      lead={
        envName ? (
          <Trans>
            What the apps in {envName} run with, shared and their own. Changing a value restarts each app that uses it,
            once you apply its plan.
          </Trans>
        ) : undefined
      }
      className="variables-page"
    >
      <RevealsProvider value={reveals}>
        <section className="variables-section" aria-label={t`Variables and secrets in ${envName}`}>
          <div className="variables-bar">
            {/* A project to choose, from the second, searchable, in place of a switch with every environment (ADR 0074). */}
            {projects.length > 1 && views ? (
              <ProjectPicker
                views={views}
                chosen={view}
                onChoose={(next) => {
                  const env = firstEnvironment(next.id, environments);
                  if (env) choose(env.id);
                }}
                onFind={(found) => choose(found.environment, found.name)}
              />
            ) : null}
            {siblings.length > 1 && chosen ? (
              <SegmentedControl
                label={t`Environment`}
                size="sm"
                value={chosen.id}
                options={siblings.map((e) => ({ value: e.id, label: e.displayName || e.resource?.name || e.id }))}
                onChange={(id) => choose(id)}
              />
            ) : null}
            {list.data ? <span className="variables-counts">{counts}</span> : null}
            {chosen && here.length > 1 ? (
              <SelectField
                className="variables-app-filter"
                size={phone ? "lg" : "sm"}
                aria-label={t`App`}
                value={only?.id ?? ""}
                onChange={(event) => filter({ app: event.target.value })}
                options={[{ value: "", label: t`All apps` }, ...here.map((s) => ({ value: s.id, label: nameOf(s) }))]}
              />
            ) : null}
            {chosen && (rows.length >= FILTER_FROM || q) ? (
              <TextField
                className="variables-filter"
                icon="search"
                type="search"
                size={phone ? "lg" : "sm"}
                aria-label={t`Filter by name`}
                placeholder={t`Filter by name`}
                value={query ?? ""}
                onChange={(event) => filter({ q: event.target.value })}
              />
            ) : null}
          </div>
          {view?.idle && !view.isDefault ? (
            // What a deleted app left: said as a state, with Delete beside it, as on Projects.
            <div className="variables-idle">
              <StatusBadge status="stopped" label={<Trans>Nothing running in {view.label}</Trans>} labelIsState />
              <Button
                size="sm"
                variant="ghost"
                icon="trash"
                onClick={() => remove.open(view, () => onEnvironment(""))}
                off={remove.offer(view).reason || undefined}
              >
                <Trans>Delete the project</Trans>
              </Button>
            </div>
          ) : null}

          {chosen && showForm ? (
            // For someone who may not plan here, the form is there to read, with why (ADR 0059).
            <MayEdit offer={may(ChangeService.method.proposeChange, { environment: chosen.id })}>
              <ValueForm
                key={editing ? `${editing.id}:${editing.revision}` : "add"}
                keyRef={keyRef}
                scope={chosen.id}
                envName={envName}
                editing={editing}
                onDone={() => setEditing(undefined)}
              />
            </MayEdit>
          ) : null}

          {failed ? (
            <p className="variables-note" role="alert">
              {sentence(failed)}
            </p>
          ) : !list.data ? (
            <p className="variables-note" aria-busy="true">
              <Trans>Loading variables and secrets</Trans>
            </p>
          ) : (q || only) && shown.length === 0 && chosen ? (
            <EmptyState
              icon="search"
              title={
                q ? t`Nothing in ${envName} is named like that` : t`${onlyName} runs with nothing of its own or shared`
              }
              action={
                <Button onClick={() => onEnvironment(chosen.id)}>
                  <Trans>Clear the filter</Trans>
                </Button>
              }
            />
          ) : shown.length === 0 && chosen ? (
            opened ? null : (
              <EmptyState
                icon="key"
                title={t`No variables or secrets in ${envName} yet`}
                action={
                  // Opens the form, with the cursor in its key, or says why you can't.
                  <Button
                    icon="plus"
                    onClick={() => setAdding(chosen.id)}
                    off={may(ChangeService.method.proposeChange, { environment: chosen.id }).reason || undefined}
                  >
                    <Trans>Add the first</Trans>
                  </Button>
                }
              />
            )
          ) : phone ? (
            <ul className="variables-list" aria-label={t`Variables and secrets in ${envName}`}>
              {shown.map((r) => (
                <li key={r.id} className="variables-item">
                  <div className="variables-item-head">
                    <KeyCell row={r} />
                    <RowActions row={r} scope={chosen?.id ?? ""} store={all} onEdit={setEditing} />
                  </div>
                  <ValueCell row={r} />
                  <span className="variables-item-source">
                    <Source row={r} /> <UsedBy row={r} />
                  </span>
                  <Updated row={r} />
                </li>
              ))}
            </ul>
          ) : (
            <VariablesTable
              rows={shown}
              scope={chosen?.id ?? ""}
              store={all}
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
  rows: readonly Row[];
  scope: string;
  store: readonly EnvironmentVariable[];
  caption: string;
  onEdit: (v: EnvironmentVariable) => void;
}

function VariablesTable({ rows, scope, store, caption, onEdit }: TableProps) {
  const { t } = useLingui();
  const columns = useMemo<TableColumn<Row>[]>(
    () => [
      { key: "name", header: t`Key`, width: 240, render: (r) => <KeyCell row={r} /> },
      { key: "value", header: t`Value`, render: (r) => <ValueCell row={r} /> },
      { key: "source", header: t`Source`, width: 120, muted: true, render: (r) => <Source row={r} /> },
      { key: "usedBy", header: t`Used by`, width: 200, render: (r) => <UsedBy row={r} /> },
      { key: "updated", header: t`Updated`, width: 160, render: (r) => <Updated row={r} /> },
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 104,
        align: "right",
        render: (r) => <RowActions row={r} scope={scope} store={store} onEdit={onEdit} />,
      },
    ],
    [t, scope, store, onEdit],
  );
  return <Table className="variables-table" columns={columns} rows={rows} caption={caption} />;
}

function KeyCell({ row }: { row: Row }) {
  const { t } = useLingui();
  return (
    <span className="variables-key">
      <code>{row.name}</code>
      {"shared" in row && row.shared.secret ? (
        <span className="variables-secret-mark" title={t`Secret`}>
          <Icon name="lock" size={12} />
          <span className="dw-sr">{t`, secret`}</span>
        </span>
      ) : null}
    </span>
  );
}

function ValueCell({ row }: { row: Row }) {
  const { t } = useLingui();
  if ("own" in row) {
    const value = row.own.row.now?.value ?? row.own.row.runtimeValue;
    if (value !== undefined) return <code className="variables-value">{value}</code>;
    // One of its own that may be a secret: no response carries it, and nothing reveals it.
    return (
      <span className="variables-masked" role="img" aria-label={t`Hidden`} title={t`Hidden, as it may be a secret`}>
        ••••••••••••
      </span>
    );
  }
  if (!row.shared.secret) return <code className="variables-value">{row.shared.value}</code>;
  return <SecretValue variable={row.shared} />;
}

/** Where a value comes from: the environment, shared by name, or the app itself. */
function Source({ row }: { row: Row }) {
  const { t } = useLingui();
  if ("shared" in row) return <span className="variables-muted">{t`Shared`}</span>;
  return (
    <span className="variables-muted">
      {row.own.row.now?.kind === "runtime" ? t`Set by its runtime` : t`Set on the app`}
    </span>
  );
}

/** A secret's value: hidden, until a passkey reveals it, for a minute. */
export function SecretValue({ variable }: { variable: EnvironmentVariable }) {
  const { t } = useLingui();
  const name = variable.name;
  const shown = useReveal(variable.id);
  // Revealing is a developer's, and an admin's in a protected environment: off, with why, otherwise (ADR 0059).
  const mayReveal = useMay()(
    VariableService.method.revealSecret,
    shown.environment ? { environment: shown.environment } : undefined,
  );
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
            {...mayReveal.off}
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

function UsedBy({ row }: { row: Row }) {
  if ("own" in row) {
    const service = row.own.service;
    return (
      <ul className="variables-users">
        <li>
          <RefLink kind="app" id={service.id}>
            {nameOf(service)}
          </RefLink>
        </li>
      </ul>
    );
  }
  const variable = row.shared;
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
        // A service may use one value under two names.
        <li key={`${use.serviceId}:${use.name}`}>
          <RefLink kind="app" id={use.serviceId}>
            {use.service === "" ? use.serviceId : use.service}
          </RefLink>
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

function Updated({ row }: { row: Row }) {
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  // What an app sets on itself changes with the app, which its page dates.
  if ("own" in row) return <span className="variables-muted">—</span>;
  const variable = row.shared;
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
  row: Row;
  scope: string;
  /** What the environment has, which a value made shared may be already. */
  store: readonly EnvironmentVariable[];
  onEdit: (v: EnvironmentVariable) => void;
}

function RowActions({ row, scope, store, onEdit }: RowActionsProps) {
  if ("own" in row) return <OwnActions service={row.own.service} row={row.own.row} scope={scope} store={store} />;
  return <SharedActions variable={row.shared} scope={scope} onEdit={onEdit} />;
}

/**
 * What one of an app's own values offers here: Make shared, in one plan, for a
 * value it shows, and Change, on the app's own page, where the rest of what it
 * runs with is.
 */
function OwnActions({
  service,
  row,
  scope,
  store,
}: {
  service: ServiceInfo;
  row: EnvRow;
  scope: string;
  store: readonly EnvironmentVariable[];
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const name = row.name;
  const app = nameOf(service);
  const mayPlan = useMay()(ChangeService.method.proposeChange, { environment: scope });
  const why = cantShare(row, store);
  const share = () =>
    sheet.open({
      kind: "propose",
      summary: t`Make ${name} on ${app} a shared variable`,
      edits: envEdits(service, new Map([[name, { op: "share", name, value: row.now?.value ?? "" }]]), store),
    });
  const off =
    why === "secret"
      ? t`The environment has a secret ${name} already. Rename one to share this.`
      : why === "other"
        ? t`The environment has a variable ${name} with another value. Rename one to share this.`
        : mayPlan.reason;
  return (
    <span className="variables-actions">
      {row.now?.kind === "runtime" && name === "PORT" ? null : (
        <RouterButtonLink
          className="variables-act-edit"
          size="sm"
          variant="ghost"
          icon="edit"
          to="/apps/$serviceId/environment"
          params={{ serviceId: service.id }}
          search={{ change: name }}
          aria-label={t`Change ${name} on ${app}`}
        />
      )}
      {row.now?.kind === "own" && why !== "hidden" ? (
        <Button
          className="variables-act-share"
          size="sm"
          variant="ghost"
          icon="layers"
          aria-label={t`Make ${name} on ${app} shared`}
          onClick={share}
          off={off || undefined}
        />
      ) : null}
    </span>
  );
}

function SharedActions({
  variable,
  scope,
  onEdit,
}: {
  variable: EnvironmentVariable;
  scope: string;
  onEdit: (v: EnvironmentVariable) => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const name = variable.name;
  const kind: Kind = variable.secret ? "secret" : "variable";
  const mayPlan = useMay()(ChangeService.method.proposeChange, { environment: scope });
  const remove = () =>
    sheet.open({
      kind: "propose",
      summary: variable.secret ? t`Remove the secret ${name}` : t`Remove the variable ${name}`,
      edits: [edit(kind, scope, name)],
    });
  return (
    <span className="variables-actions">
      <Button
        className="variables-act-edit"
        size="sm"
        variant="ghost"
        icon="edit"
        aria-label={t`Change ${name}`}
        onClick={() => onEdit(variable)}
        {...mayPlan.off}
      />
      <Button
        className="variables-act-remove"
        size="sm"
        variant="ghost"
        icon="trash"
        aria-label={t`Remove ${name}`}
        onClick={remove}
        {...mayPlan.off}
      />
    </span>
  );
}

interface ValueFormProps {
  /** The key's field, which the empty list's action puts the cursor in. */
  keyRef?: Ref<HTMLInputElement>;
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
 * makes the other; the control plane takes in every service that uses it,
 * moved to the new kind. A secret's current value is never filled in, and
 * neither is a variable's once it is to become a secret: a value that was
 * a plain variable stays in its history, so the secret takes a new one.
 */
function ValueForm({ keyRef, scope, envName, editing, onDone }: ValueFormProps) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const [name, setName] = useState(editing?.name ?? "");
  const [value, setValue] = useState(editing && !editing.secret ? editing.value : "");
  const [secret, setSecret] = useState(editing?.secret ?? false);
  const [tried, setTried] = useState(false);
  // A variable on its way to becoming a secret.
  const toSecret = editing !== undefined && !editing.secret && secret;
  const switchSecret = (on: boolean) => {
    setSecret(on);
    // Its value as a variable isn't carried over: the secret takes a new one.
    if (editing && !editing.secret) setValue(on ? "" : editing.value);
  };
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
          ref={keyRef}
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
          placeholder={(editing?.secret && secret) || toSecret ? t`New value` : t`value`}
          value={value}
          autoComplete="off"
          spellCheck={false}
          error={tried ? missingValue : undefined}
          onChange={(e) => setValue(e.target.value)}
        />
      </div>
      <Switch label={t`Secret`} checked={secret} onChange={switchSecret} />
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
      {toSecret ? (
        <Callout
          className="variables-form-note"
          tone="warning"
          title={t`The old value stays in this variable's history`}
        >
          <Trans>
            It was a plain variable, so anyone who can read this environment can still see it. Enter a new value for the
            secret, and rotate the old one wherever it was issued.
          </Trans>
        </Callout>
      ) : null}
    </form>
  );
}
