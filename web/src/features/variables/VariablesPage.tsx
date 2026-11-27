// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  DomainService,
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
import { useInfiniteQuery, useQuery as useTanstackQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, type Ref, useEffect, useMemo, useRef, useState } from "react";
import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { passkeysAvailable } from "../../lib/webauthn";
import { nameOf } from "../apps/app";
import { cantMakeSecret, cantShare, type EnvRow, envEdits, envRows } from "../apps/envEdit";
import { useServices } from "../apps/useServices";
import { useInstalls } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useProjectDelete } from "../projects/deleteProject";
import { compareProjects, projectSummary } from "../projects/summary";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import { ListPager, type ListPlace, ListSearchField, ListSortField, SortHeader, useRowMenu } from "../shell/ListTools";
import { nextSort, pagedRows, readListPages, readSort, sortedRows, sortParam } from "../shell/list";
import { Page } from "../shell/Page";
import { RefLink } from "../shell/RefLink";
import { ProjectPicker, type VariablesProject } from "./ProjectPicker";
import { ownValueKey, RevealsProvider, useReveal, useReveals } from "./useReveal";
import "./VariablesPage.css";
import { useActorWords } from "../activity/actor";
import { MayEdit } from "../auth/MayEdit";
import { type Offer, useMay } from "../auth/may";

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
      await readListPages(
        (pageToken) => client.listVariables({ ...input, pageSize: PAGE_SIZE, pageToken }, { signal }),
        (page) => {
          all.environmentId = page.environmentId;
          all.variables.push(...page.variables);
        },
      );
      return all;
    },
    enabled: environment !== undefined,
  });
}

/** The first page of both shared value kinds is enough to find a populated environment in most fleets. */
function useSharedValueResources(complete: boolean) {
  const transport = useTransport();
  const query = useInfiniteQuery({
    queryKey: [
      ...createConnectQueryKey({ schema: ResourceService.method.listResources, transport, cardinality: "infinite" }),
      "shared-values",
    ],
    initialPageParam: { variable: "", secret: "" } as Record<Kind, string | undefined>,
    queryFn: async ({ pageParam, signal }) => {
      const client = createClient(ResourceService, transport);
      const read = (kind: Kind) => {
        const pageToken = pageParam[kind];
        return pageToken === undefined
          ? undefined
          : client.listResources({ kind, pageSize: PAGE_SIZE, pageToken }, { signal });
      };
      const [variables, secrets] = await Promise.all([read("variable"), read("secret")]);
      return {
        resources: [...(variables?.resources ?? []), ...(secrets?.resources ?? [])],
        next: { variable: variables?.nextPageToken || undefined, secret: secrets?.nextPageToken || undefined },
      };
    },
    getNextPageParam: ({ next }) => (next.variable || next.secret ? next : undefined),
  });
  const { data, fetchNextPage, hasNextPage, isFetching, isFetchNextPageError, isPending } = query;
  useEffect(() => {
    if (complete && hasNextPage && !isFetching && !isFetchNextPageError) void fetchNextPage();
  }, [complete, fetchNextPage, hasNextPage, isFetching, isFetchNextPageError]);
  const resources = useMemo(() => data?.pages.flatMap((page) => page.resources), [data]);
  return { resources, isPending, reading: isPending || (complete && hasNextPage && !isFetchNextPageError) };
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
function defaultEnvironment(
  envs: readonly Resource[],
  projects: readonly Resource[],
  apps: ReadonlyMap<string, number>,
  populated: ReadonlySet<string>,
): Resource | undefined {
  const first = projects[0]?.id;
  const usual =
    envs.find((e) => e.resource?.scope === first && e.resource?.name === "production") ??
    envs.find((e) => e.resource?.scope === first) ??
    envs[0];
  const withValues = envs.filter((e) => populated.has(e.id));
  if (withValues.length > 0) return withValues.find((e) => e.id === usual?.id) ?? withValues[0];
  if (usual && (apps.get(usual.id) ?? 0) > 0) return usual;
  // Where the apps are, never an empty default while they live elsewhere (#796): the
  // environment with the most, by name where two have as many.
  const busiest = [...envs]
    .filter((e) => (apps.get(e.id) ?? 0) > 0)
    .sort(
      (a, b) =>
        (apps.get(b.id) ?? 0) - (apps.get(a.id) ?? 0) || (a.resource?.name ?? "").localeCompare(b.resource?.name ?? ""),
    )[0];
  return busiest ?? usual;
}

export interface VariablesPageProps {
  /** The environment shown, by ID; the last one chosen here, or the default, when unset. */
  environment?: string | undefined;
  /** What the list is filtered by, by name. */
  query?: string | undefined;
  /** An app, by ID: only what it runs with is listed. */
  app?: string | undefined;
  /** Where the values listed come from: shared by the environment, or set on its apps; unset for both. */
  from?: Source | undefined;
  /** How the list is sorted, as its address holds it ("name", or "-updated" the other way). */
  sort?: string | undefined;
  /** Which page of the list shows, from 1. */
  page?: number | undefined;
  onEnvironment: (id: string, query?: string, app?: string, from?: Source, sort?: string, page?: number) => void;
}

/** The columns the list sorts by, as its address names them. */
const SORT_KEYS = ["name", "updated"] as const;

/** Where a value comes from: shared by the environment, or set on an app (#618). */
export type Source = "shared" | "apps";

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
export function VariablesPage({ environment, query, app, from, sort, page, onEnvironment }: VariablesPageProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const envs = useQuery(ResourceService.method.listResources, { kind: "environment" });
  const projectList = useQuery(ResourceService.method.listResources, { kind: "project" });
  const projects = projectList.data?.resources ?? [];
  const environments = envs.data?.resources ?? [];
  const [readingIndex, setReadingIndex] = useState(false);
  const shared = useSharedValueResources(readingIndex);
  const domainsQuery = useQuery(
    DomainService.method.listDomains,
    {},
    { enabled: projects.length > 1, staleTime: Number.POSITIVE_INFINITY },
  );
  const installs = useInstalls().data?.installs;
  const [last] = useState(remembered);
  const services = useServices();
  const appsIn = useMemo(() => {
    const out = new Map<string, number>();
    for (const s of services.data ?? []) {
      const env = s.resource?.scope ?? "";
      out.set(env, (out.get(env) ?? 0) + 1);
    }
    return out;
  }, [services.data]);
  const populated = useMemo(() => {
    const out = new Set<string>();
    for (const item of shared.resources ?? []) {
      const ref = item.resource;
      if (ref?.scope && (ref.kind === "variable" || ref.kind === "secret")) out.add(ref.scope);
    }
    for (const s of services.data ?? []) {
      if (s.resource?.scope && rowsOf([], [s]).length > 0) out.add(s.resource.scope);
    }
    return out;
  }, [shared.resources, services.data]);
  const views = useMemo(() => {
    if (!projectList.data || !envs.data) return [];
    const byEnvironment = new Map(envs.data.resources.map((env) => [env.id, env.resource?.scope]));
    const inside = new Map<string, Resource[]>();
    for (const value of shared.resources ?? []) {
      const project = byEnvironment.get(value.resource?.scope ?? "");
      if (!project) continue;
      const values = inside.get(project) ?? [];
      values.push(value);
      inside.set(project, values);
    }
    return projectList.data.resources
      .map((project): VariablesProject => {
        const environmentIds = envs.data.resources
          .filter((env) => env.resource?.scope === project.id)
          .map((env) => env.id);
        return {
          ...projectSummary(project, {
            installs,
            environmentIds,
            services: services.data,
            servicesFailed: services.isError,
            domains: domainsQuery.data?.domains,
            domainsFailed: domainsQuery.isError,
          }),
          inside: inside.get(project.id) ?? [],
        };
      })
      .sort(compareProjects);
  }, [
    projectList.data,
    envs.data,
    domainsQuery.data?.domains,
    domainsQuery.isError,
    shared.resources,
    installs,
    services.data,
    services.isError,
  ]);
  const chosen =
    environments.find((e) => e.id === environment) ??
    environments.find((e) => e.id === last) ??
    (shared.isPending || services.isPending
      ? undefined
      : defaultEnvironment(environments, projects, appsIn, populated));
  const view = views.find((v) => v.id === chosen?.resource?.scope);
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
  const here = useMemo(
    () => (services.data ?? []).filter((s) => s.resource?.scope === chosen?.id && s.spec !== undefined),
    [services.data, chosen?.id],
  );
  const rows = useMemo(() => rowsOf(list.data?.variables ?? [], here), [list.data, here]);
  const only = here.find((s) => s.id === app);
  const onlyName = only ? nameOf(only) : "";
  const q = (query ?? "").trim().toLowerCase();
  // By name and app first, so each source's number says what choosing it would list. Where the
  // environment has no values at all, the source is no filter: its control is hidden, and the page
  // offers the first value (#620's review, L2).
  const source = rows.length > 0 ? from : undefined;
  const named = rows.filter((r) => (!q || r.name.toLowerCase().includes(q)) && (!only || usedByApp(r, only.id)));
  const sharedRows = named.filter((r) => "shared" in r).length;
  const filtered = named.filter((r) => !source || (source === "shared") === "shared" in r);
  // Sorted only where its head was asked to: as read, what the environment shares
  // comes first, then each app's own, which is how the list reads until then.
  const sortState = readSort(sort, SORT_KEYS);
  const shown = sortedRows(filtered, sortState, (r, key) =>
    key === "updated"
      ? "shared" in r && r.shared.updateTime
        ? timestampDate(r.shared.updateTime).getTime()
        : null
      : r.name,
  );
  const paged = pagedRows(shown, (page ?? 1) - 1);
  // This page's place in the shared list tools' shape: its address has held the
  // filter word by word all along, beside its environment, app and source.
  const place: ListPlace = {
    typed: query ?? "",
    setTyped: (words) => filter({ q: words }),
    query: q,
    clearQuery: () => filter({ q: undefined }),
    sort: sortState,
    toggleSort: (key) => {
      if (chosen) onEnvironment(chosen.id, query, only?.id, from, sortParam(nextSort(sortState, key)));
    },
    setSort: (next) => {
      if (chosen) onEnvironment(chosen.id, query, only?.id, from, sortParam(next));
    },
    sortOf: (key) => (sortState?.key === key ? (sortState.dir === "asc" ? "ascending" : "descending") : undefined),
    page: paged.page,
    setPage: (next) => {
      if (chosen) onEnvironment(chosen.id, query, only?.id, from, sort, next > 0 ? next + 1 : undefined);
    },
  };
  // What the rows listed are: a secret is shared, and an app's own value is a variable, even one
  // that reads hidden as it may hold a secret, as it's a value of the app's own environment and no
  // secret of the environment's (ADR 0081); so a hidden row can sit under "0 secrets" (#618).
  const secrets = shown.filter((r) => "shared" in r && r.shared.secret).length;
  const phoneMenuFor = useVariableMenu(chosen?.id ?? "", list.data?.variables, setEditing);
  const counts = [
    plural(shown.length - secrets, { one: "# variable", other: "# variables" }),
    plural(secrets, { one: "# secret", other: "# secrets" }),
  ].join(" · ");
  const failed = envs.error ?? list.error;
  const empty = list.data !== undefined && rows.length === 0;
  const filter = (next: { q?: string | undefined; app?: string | undefined; from?: Source | undefined }) => {
    if (!chosen) return;
    const nq = "q" in next ? next.q : query;
    const na = "app" in next ? next.app : only?.id;
    const nf = "from" in next ? next.from : from;
    // A new filter keeps the sort and starts again at the first page.
    onEnvironment(chosen.id, nq || undefined, na || undefined, nf || undefined, sort);
  };
  const opened = chosen !== undefined && adding === chosen.id;
  const showForm = chosen !== undefined && list.data !== undefined && (!empty || opened || editing !== undefined);
  useEffect(() => {
    if (opened && showForm) keyRef.current?.focus();
  }, [opened, showForm]);

  const mayAdd = chosen ? may(ChangeService.method.proposeChange, { environment: chosen.id }) : undefined;
  const add = () => {
    if (!chosen) return;
    setEditing(undefined);
    setAdding(chosen.id);
    if (opened) keyRef.current?.focus();
  };

  return (
    <Page
      title={t`Variables`}
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
      actions={
        chosen ? (
          <Button variant="primary" icon="plus" aria-label={t`New variable or secret`} onClick={add} {...mayAdd?.off}>
            <Trans>New</Trans>
          </Button>
        ) : undefined
      }
      phoneAction={
        chosen ? (
          <Button
            variant="ghost"
            icon="plus"
            className="mobile-header-button"
            aria-label={t`New variable or secret`}
            onClick={add}
            {...mayAdd?.off}
          />
        ) : undefined
      }
      className="variables-page"
    >
      <RevealsProvider value={reveals}>
        <section className="variables-section" aria-label={t`Variables in ${envName}`}>
          <div className="variables-bar">
            {/* A project to choose, from the second, searchable, in place of a switch with every environment (ADR 0074). */}
            {projects.length > 1 ? (
              <ProjectPicker
                views={views}
                chosen={view}
                onOpen={() => setReadingIndex(true)}
                reading={shared.reading || services.isPending || domainsQuery.isPending}
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
            {chosen && rows.length > 0 ? (
              <SegmentedControl<"all" | Source>
                label={t`Where values come from`}
                size="sm"
                value={source ?? "all"}
                options={[
                  {
                    value: "all",
                    label: t({ message: "All", context: "values, from wherever they come" }),
                    count: named.length,
                  },
                  {
                    value: "shared",
                    label: t({ message: "Shared", context: "values the environment shares" }),
                    count: sharedRows,
                  },
                  { value: "apps", label: t`Set on apps`, count: named.length - sharedRows },
                ]}
                onChange={(next) => filter({ from: next === "all" ? undefined : next })}
              />
            ) : null}
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
              <ListSearchField
                className="variables-filter"
                size={phone ? "lg" : "sm"}
                label={t`Filter by name`}
                placeholder={t`Filter by name`}
                value={query ?? ""}
                onChange={(words) => filter({ q: words })}
              />
            ) : null}
            {phone && rows.length > 0 ? (
              <ListSortField place={place} columns={{ name: t`Key`, updated: t`Updated` }} />
            ) : null}
          </div>
          {view?.idle === true && !view.isDefault ? (
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
            <Table
              className="variables-table"
              columns={[
                { key: "name", header: t`Key`, width: 240, render: () => null, skeleton: "mono" },
                { key: "value", header: t`Value`, render: () => null, skeleton: "mono" },
                { key: "source", header: t`Source`, width: 120, render: () => null, skeleton: "short" },
                { key: "usedBy", header: t`Used by`, width: 200, render: () => null },
                { key: "updated", header: t`Updated`, width: 160, render: () => null, skeleton: "short" },
              ]}
              rows={[]}
              loading
              loadingRows={4}
              loadingLabel={t`Loading variables and secrets`}
            />
          ) : source && named.length > 0 && shown.length === 0 && chosen ? (
            // Only the source emptied the list: say so, and show the rest (#620's review, M1).
            <EmptyState
              icon="search"
              title={
                source === "shared"
                  ? only
                    ? t`${onlyName} uses nothing shared`
                    : t`Nothing in ${envName} is shared`
                  : only
                    ? t`${onlyName} sets no value of its own`
                    : t`No app in ${envName} sets a value of its own`
              }
              action={
                <Button onClick={() => filter({ from: undefined })}>
                  <Trans>Show all</Trans>
                </Button>
              }
            />
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
                  <Button icon="plus" onClick={add} {...mayAdd?.off}>
                    <Trans>New variable or secret</Trans>
                  </Button>
                }
              />
            )
          ) : phone ? (
            <>
              <ul className="variables-list" aria-label={t`Variables in ${envName}`}>
                {paged.rows.map((r) => (
                  <li key={r.id} className="variables-item">
                    <div className="variables-item-head">
                      <KeyCell row={r} />
                      <ActionMenu label={t`Actions for ${r.name}`} actions={phoneMenuFor(r)} />
                    </div>
                    <ValueCell row={r} />
                    <span className="variables-item-source">
                      <Source row={r} /> <UsedBy row={r} />
                    </span>
                    <Updated row={r} />
                  </li>
                ))}
              </ul>
              <ListPager page={paged} onPage={place.setPage} />
            </>
          ) : (
            <>
              <VariablesTable
                rows={paged.rows}
                scope={chosen?.id ?? ""}
                store={list.data?.variables}
                caption={t`Variables in ${envName}`}
                onEdit={setEditing}
                place={place}
              />
              <ListPager page={paged} onPage={place.setPage} />
            </>
          )}
        </section>
      </RevealsProvider>
    </Page>
  );
}

interface TableProps {
  rows: readonly Row[];
  scope: string;
  store: readonly EnvironmentVariable[] | undefined;
  caption: string;
  onEdit: (v: EnvironmentVariable) => void;
  place: ListPlace;
}

function useVariableMenu(
  scope: string,
  store: readonly EnvironmentVariable[] | undefined,
  onEdit: (v: EnvironmentVariable) => void,
) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { environment: scope });
  const ownActionsFor = useOwnValueActions(scope, store);
  // A row's menu on a right-click or Shift+F10: what its own buttons do, for a shared
  // value. An app's own changes on its app's page, as its button there says.
  return (row: Row): MenuAction[] => {
    const off = mayPlan.allowed ? {} : { off: mayPlan.reason || undefined, disabled: !mayPlan.reason };
    if ("own" in row) {
      const app = nameOf(row.own.service);
      return row.own.row.now?.kind === "runtime" && row.name === "PORT"
        ? []
        : [
            {
              id: "change",
              icon: "edit",
              label: t`Change ${row.name} on ${app}`,
              onSelect: () =>
                void navigate({
                  to: "/apps/$serviceId/environment",
                  params: { serviceId: row.own.service.id },
                  search: { change: row.name },
                }),
            },
            ...ownActionsFor(row.own.service, row.own.row),
          ];
    }
    const variable = row.shared;
    const kind: Kind = variable.secret ? "secret" : "variable";
    return [
      { id: "change", icon: "edit", label: t`Change ${row.name}`, ...off, onSelect: () => onEdit(variable) },
      {
        id: "remove",
        icon: "trash",
        label: t`Remove ${row.name}`,
        danger: true,
        ...off,
        onSelect: () =>
          sheet.open({
            kind: "propose",
            summary: variable.secret ? t`Remove the secret ${row.name}` : t`Remove the variable ${row.name}`,
            edits: [edit(kind, scope, row.name)],
          }),
      },
    ];
  };
}

function VariablesTable({ rows, scope, store, caption, onEdit, place }: TableProps) {
  const { t } = useLingui();
  const menuFor = useVariableMenu(scope, store, onEdit);
  const menu = useRowMenu<Row>();
  const columns = useMemo<TableColumn<Row>[]>(
    () => [
      {
        key: "name",
        header: <SortHeader label={t`Key`} sortKey="name" place={place} />,
        sort: place.sortOf("name"),
        width: 240,
        render: (r) => <KeyCell row={r} />,
      },
      { key: "value", header: t`Value`, render: (r) => <ValueCell row={r} /> },
      { key: "source", header: t`Source`, width: 120, muted: true, render: (r) => <Source row={r} /> },
      { key: "usedBy", header: t`Used by`, width: 200, render: (r) => <UsedBy row={r} /> },
      {
        key: "updated",
        header: <SortHeader label={t`Updated`} sortKey="updated" place={place} />,
        sort: place.sortOf("updated"),
        width: 176,
        render: (r) => <Updated row={r} />,
      },
      {
        key: "actions",
        header: <span className="dw-sr">{t`Actions`}</span>,
        width: 104,
        align: "right",
        render: (r) => <RowActions row={r} scope={scope} store={store} onEdit={onEdit} />,
      },
    ],
    [t, scope, store, onEdit, place],
  );
  return (
    <>
      <Table
        className="variables-table"
        columns={columns}
        rows={rows}
        caption={caption}
        rowHasContextMenu={(row) => menuFor(row).length > 0}
        onRowContextMenu={(row, at, el) =>
          menu.open(row, at, el, { label: t`Actions for ${row.name}`, actions: menuFor(row) })
        }
      />
      {menu.element}
    </>
  );
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
  if ("own" in row) {
    const value = row.own.row.now?.value ?? row.own.row.runtimeValue;
    if (value !== undefined) return <code className="variables-value">{value}</code>;
    return <OwnHidden service={row.own.service} name={row.name} />;
  }
  if (!row.shared.secret) return <code className="variables-value">{row.shared.value}</code>;
  return <SecretValue variable={row.shared} />;
}

/**
 * One of an app's own values that may be a secret: a passkey reveals it, and
 * once seen, saying it's none shows it, in one plan (ADR 0081).
 */
function OwnHidden({ service, name }: { service: ServiceInfo; name: string }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const app = nameOf(service);
  return (
    <OwnHiddenValue
      service={service.id}
      name={name}
      onShow={(value) =>
        sheet.open({
          kind: "propose",
          summary: t`Show ${name} on ${app}`,
          edits: envEdits(service, new Map([[name, { op: "change", name, value }]])),
        })
      }
    />
  );
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
  const shown = useReveal(variable.id);
  // Revealing is a developer's, and an admin's in a protected environment: off, with why, otherwise (ADR 0059).
  const mayReveal = useMay()(
    VariableService.method.revealSecret,
    shown.environment ? { environment: shown.environment } : undefined,
  );
  return <Revealable name={variable.name} shown={shown} offer={mayReveal} />;
}

/**
 * One of an app's own values that reads hidden, as it may be a secret: hidden,
 * until a passkey reveals it, for a minute, on record, as a secret is (ADR 0081).
 * Revealed, onShow, where given, lets a person say it's no secret, so it shows.
 */
export function OwnHiddenValue({
  service,
  name,
  onShow,
}: {
  service: string;
  name: string;
  onShow?: ((value: string) => void) | undefined;
}) {
  const { t } = useLingui();
  const shown = useReveal(ownValueKey(service, name));
  const mayReveal = useMay()(
    VariableService.method.revealServiceValue,
    shown.environment ? { environment: shown.environment } : undefined,
  );
  const value = shown.value;
  return (
    <Revealable
      name={name}
      shown={shown}
      offer={mayReveal}
      after={
        onShow && value !== undefined ? (
          <Button size="sm" variant="ghost" aria-label={t`${name} is no secret: show it`} onClick={() => onShow(value)}>
            <Trans>Not a secret</Trans>
          </Button>
        ) : null
      }
    />
  );
}

/** A value hidden until revealed: Reveal, then the value with Copy and Hide, and what goes after. */
function Revealable({
  name,
  shown,
  offer,
  after,
}: {
  name: string;
  shown: ReturnType<typeof useReveal>;
  offer: Offer;
  after?: ReactNode;
}) {
  const { t } = useLingui();
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
            {...offer.off}
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
          {after}
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
  const actor = useActorWords();
  const now = useControlPlaneNow(60_000);
  // What an app sets on itself changes with the app, which its page dates.
  if ("own" in row) return <span className="variables-muted">—</span>;
  const variable = row.shared;
  // Who, as Activity says it: a person by name, and a token the CLI made by where it ran.
  const by = variable.updatedBy ? actor(variable.updatedBy) : undefined;
  return (
    <span className="variables-updated">
      {by ? <span title={by.full === by.name ? undefined : by.full}>{by.name}</span> : null}
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
  store: readonly EnvironmentVariable[] | undefined;
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
function useOwnValueActions(scope: string, store: readonly EnvironmentVariable[] | undefined) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { environment: scope });
  return (service: ServiceInfo, row: EnvRow): MenuAction[] => {
    if (row.now?.kind !== "own") return [];
    const name = row.name;
    const app = nameOf(service);
    const why = cantShare(row, store);
    const share = () =>
      sheet.open({
        kind: "propose",
        summary: t`Make ${name} on ${app} a shared variable`,
        edits: envEdits(service, new Map([[name, { op: "share", name, value: row.now?.value ?? "" }]]), store),
      });
    const makeSecret = () =>
      sheet.open({
        kind: "propose",
        summary: t`Make ${name} on ${app} a secret`,
        edits: envEdits(service, new Map([[name, { op: "secret", name }]]), store),
      });
    const blocked = cantMakeSecret(row, store);
    const secretOff =
      blocked === "secret"
        ? t`The environment has a secret ${name} already. Rename one to keep this as a secret.`
        : blocked === "variable"
          ? t`The environment has a variable ${name} already. Rename one to keep this as a secret.`
          : blocked === "reading"
            ? t`Waits until the environment's variables and secrets load.`
            : mayPlan.reason;
    const off =
      why === "secret"
        ? t`The environment has a secret ${name} already. Rename one to share this.`
        : why === "other"
          ? t`The environment has a variable ${name} with another value. Rename one to share this.`
          : why === "reading"
            ? t`Waits until the environment's variables and secrets load.`
            : mayPlan.reason;
    return [
      why === "hidden"
        ? {
            id: "secret",
            icon: "lock",
            label: t`Make ${name} on ${app} a secret`,
            ...mayPlan.off,
            off: secretOff || undefined,
            onSelect: makeSecret,
          }
        : {
            id: "share",
            icon: "layers",
            label: t`Make ${name} on ${app} shared`,
            ...mayPlan.off,
            off: off || undefined,
            onSelect: share,
          },
    ];
  };
}

function OwnActions({
  service,
  row,
  scope,
  store,
}: {
  service: ServiceInfo;
  row: EnvRow;
  scope: string;
  store: readonly EnvironmentVariable[] | undefined;
}) {
  const { t } = useLingui();
  const { name } = row;
  const app = nameOf(service);
  const [action] = useOwnValueActions(scope, store)(service, row);
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
      {action ? (
        <Button
          className="variables-act-share"
          size="sm"
          variant="ghost"
          icon={action.icon}
          aria-label={String(action.label)}
          onClick={action.onSelect}
          off={action.off}
          disabled={action.disabled}
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
