// SPDX-License-Identifier: AGPL-3.0-only

import type { ServiceInfo } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useDeferredValue, useEffect, useMemo, useState } from "react";
import { sentence } from "../../lib/errors";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { Reconnecting } from "../servers/FleetNotes";
import { useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import {
  type AppGroup,
  type AppState,
  appState,
  deployedAt,
  environmentOf,
  groupApps,
  groupDeployedAt,
  groupState,
  imageOf,
  matches,
  nameOf,
  revisionName,
  sortGroups,
} from "./app";
import { ExportSheet } from "./ExportSheet";
import { usePlaces, useServices, useServicesLive } from "./useServices";
import { useAppWords } from "./words";
import "./Apps.css";

/** The list's place, in its URL: its filters. */
export interface AppsSearch {
  q?: string;
  environment?: string;
  server?: string;
}

/** A filter earns its place once there are more apps than fit on a screen at a glance. */
const FILTER_FROM = 6;

/** A SegmentedControl holds up to five; past that, a select. */
const SEGMENTS = 5;

/** The names a stack shows in its title before it says how many more. */
const STACK_NAMES = 3;

/** One row of the list: an app, a stack's head, or a service in a stack. */
interface Row {
  id: string;
  kind: "app" | "stack" | "member";
  group: AppGroup;
  service?: ServiceInfo;
  /** The row after this one is still in the same stack. */
  last: boolean;
}

/**
 * Every app: what it runs, how its latest deploy went, its revision and when
 * it deployed, live (Apps-List, M-Apps). Columns follow what the API has and
 * how many servers there are: no server column with one server. A Compose
 * file's services show as one stack, its head first (ADR 0026). What needs
 * you comes first; the rest are newest first.
 */
export function AppsPage({
  search = {},
  onSearch,
  children,
}: {
  search?: AppsSearch;
  onSearch?: (next: AppsSearch) => void;
  /** New app's sheet, over the list. */
  children?: ReactNode;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const services = useServices();
  const { reconnecting } = useServicesLive();
  const { servers, fleet } = useFleetView();
  const places = usePlaces();
  const navigate = useNavigate();
  const [exporting, setExporting] = useState(false);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [typed, setTyped] = useState(search.q ?? "");
  const query = useDeferredValue(typed);

  // The filter goes in the address a moment after you stop typing, so a link
  // to the list keeps it, without a navigation for each key.
  useEffect(() => {
    if (query === (search.q ?? "")) return;
    const timer = setTimeout(() => onSearch?.({ ...search, q: query || undefined }), 300);
    return () => clearTimeout(timer);
  }, [query, search, onSearch]);

  const all = services.data;
  const environments = useMemo(() => {
    const counts = new Map<string, number>();
    for (const s of all ?? []) counts.set(environmentOf(s), (counts.get(environmentOf(s)) ?? 0) + 1);
    return places.environments.map((e) => ({ ...e, count: counts.get(e.id) ?? 0 }));
  }, [all, places.environments]);
  // Environments stay out of sight until there is a choice (PRD principle 4).
  const choosing = environments.length > 1;
  const environment = choosing ? (search.environment ?? "") : "";
  const shown = useMemo(
    () =>
      (all ?? []).filter(
        (s) =>
          (environment === "" || environmentOf(s) === environment) &&
          (!search.server || s.deployments.some((d) => d.serverId === search.server)),
      ),
    [all, environment, search.server],
  );
  const groups = useMemo(() => sortGroups(groupApps(shown), servers), [shown, servers]);
  const filtering = query.trim() !== "";
  const rows = useMemo(() => {
    const out: Row[] = [];
    for (const group of groups) {
      const members = group.services.filter((s) => matches(s, query));
      if (members.length === 0) continue;
      const [first] = group.services;
      if (!group.stack && first) {
        out.push({ id: first.id, kind: "app", group, service: first, last: true });
        continue;
      }
      out.push({ id: `stack:${group.key}`, kind: "stack", group, last: false });
      // A filter shows what it finds, whatever was folded away.
      if (collapsed.has(group.key) && !filtering) continue;
      members.forEach((service, i) => {
        out.push({ id: service.id, kind: "member", group, service, last: i === members.length - 1 });
      });
    }
    return out;
  }, [groups, query, collapsed, filtering]);

  const open = (id: string) => void navigate({ to: "/apps/$serviceId", params: { serviceId: id } });
  const toggle = (key: string) =>
    setCollapsed((was) => {
      const next = new Set(was);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const elsewhere = (all?.length ?? 0) - shown.length;
  const lead = useLead(shown, environment ? places.label(environment) : "");
  const newApp = (
    <RouterButtonLink to="/apps/new" variant="primary" icon="plus" className="apps-new">
      <Trans>New app</Trans>
    </RouterButtonLink>
  );

  let body: ReactNode;
  if (services.isPending) {
    body = <ListSkeleton label={t`Loading apps`} />;
  } else if (services.error) {
    body = (
      <Callout
        tone="critical"
        title={t`Can't load the apps`}
        actions={
          <Button onClick={() => void services.refetch()} loading={services.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {sentence(services.error)} <Trans>What runs on your servers isn't affected.</Trans>
      </Callout>
    );
  } else if ((all?.length ?? 0) === 0) {
    body = (
      <EmptyState
        icon="service"
        title={<Trans>No apps yet</Trans>}
        action={
          <RouterButtonLink to="/apps/new" variant="primary" icon="plus" size="lg">
            <Trans>New app</Trans>
          </RouterButtonLink>
        }
      >
        <p>
          <Trans>Run an image or a Compose file on your server. You review a plan before anything starts.</Trans>
        </p>
      </EmptyState>
    );
  } else if (rows.length === 0) {
    body = (
      <EmptyState icon="search" title={<Trans>Nothing matches</Trans>}>
        <p>
          {filtering ? (
            <Trans>
              No app here has <code>{query}</code> in its name or image. Try part of a name, or clear the filter.
            </Trans>
          ) : (
            <Trans>There are no apps here yet. Choose another environment, or make one here with New app.</Trans>
          )}
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = <PhoneList rows={rows} collapsed={collapsed} onToggle={toggle} />;
  } else {
    body = (
      <AppsTable rows={rows} fleet={fleet} caption={t`Apps`} collapsed={collapsed} onToggle={toggle} onOpen={open} />
    );
  }

  const envOptions = environments.map((e) => ({ value: e.id, label: e.label, count: e.count }));
  return (
    <Page
      title={t`Apps`}
      lead={all && all.length > 0 ? lead : undefined}
      status={reconnecting ? <Reconnecting /> : undefined}
      className="apps-page"
      actions={
        <>
          {/* One environment is one Compose file, so it has to be clear which. */}
          {(all?.length ?? 0) > 0 && (!choosing || environment !== "") ? (
            <Button icon="download" onClick={() => setExporting(true)}>
              <Trans>Export</Trans>
            </Button>
          ) : null}
          {/* The empty state offers it, and a view has one primary button. */}
          {all?.length === 0 ? null : newApp}
        </>
      }
    >
      <section className="apps-section" aria-label={t`Apps`}>
        {choosing || fleet || (all?.length ?? 0) >= FILTER_FROM ? (
          <div className="apps-filters">
            {choosing && envOptions.length <= SEGMENTS ? (
              <SegmentedControl
                options={[{ value: "", label: t`All` }, ...envOptions]}
                value={environment}
                label={t`Environment`}
                size="sm"
                onChange={(value) => onSearch?.({ ...search, environment: value || undefined })}
              />
            ) : null}
            {choosing && envOptions.length > SEGMENTS ? (
              <SelectField
                aria-label={t`Environment`}
                size="sm"
                value={environment}
                options={[
                  { value: "", label: t`All environments` },
                  ...envOptions.map((e) => ({ value: e.value, label: `${e.label} (${e.count})` })),
                ]}
                onChange={(event) => onSearch?.({ ...search, environment: event.target.value || undefined })}
              />
            ) : null}
            {fleet ? (
              <SelectField
                aria-label={t`Server`}
                size="sm"
                className="apps-server-filter"
                value={search.server ?? ""}
                options={[
                  { value: "", label: t`All servers` },
                  ...(servers ?? []).map((s) => ({ value: s.id, label: s.name })),
                ]}
                onChange={(event) => onSearch?.({ ...search, server: event.target.value || undefined })}
              />
            ) : null}
            <span className="apps-fill" />
            {(all?.length ?? 0) >= FILTER_FROM ? (
              <TextField
                className="apps-filter"
                size={phone ? "lg" : "sm"}
                icon="search"
                type="search"
                placeholder={t`Filter by name or image`}
                aria-label={t`Filter apps`}
                value={typed}
                onChange={(event) => setTyped(event.target.value)}
              />
            ) : null}
          </div>
        ) : null}
        {phone && all && all.length > 0 ? (
          <div className="apps-phone-head">
            <h2 className="dw-text-heading apps-phone-count">{lead}</h2>
            {newApp}
          </div>
        ) : null}
        {body}
        {elsewhere > 0 && all ? (
          <p className="apps-note">
            {plural(elsewhere, {
              one: "# more app lives outside this view",
              other: "# more apps live outside this view",
            })}
          </p>
        ) : null}
      </section>
      {children}
      {exporting ? (
        <ExportSheet
          environment={environment || (all?.[0] ? environmentOf(all[0]) : "")}
          onClose={() => setExporting(false)}
        />
      ) : null}
    </Page>
  );
}

/** The line under the title: how many, and what is going on with them. */
function useLead(services: readonly ServiceInfo[], where: string): string {
  const { servers } = useFleetView();
  const { t } = useLingui();
  const count = services.length;
  const total = where
    ? plural(count, { one: `# app in ${where}`, other: `# apps in ${where}` })
    : plural(count, { one: "# app", other: "# apps" });
  let moving = 0;
  let trouble = 0;
  for (const s of services) {
    const kind = appState(s, servers).kind;
    if (kind === "deploying") moving++;
    else if (kind !== "live") trouble++;
  }
  return [total, trouble > 0 ? t`${trouble} need a look` : "", moving > 0 ? t`${moving} deploying` : ""]
    .filter(Boolean)
    .join(" · ");
}

/** A stack's title, from its services' names. */
function useStackTitle() {
  const { t } = useLingui();
  return (group: AppGroup): string => {
    const names = group.services.slice(0, STACK_NAMES).map(nameOf).join(", ");
    const more = group.services.length - STACK_NAMES;
    return more > 0 ? t`${names} and ${more} more` : names;
  };
}

interface TableProps {
  rows: readonly Row[];
  fleet: boolean;
  caption: string;
  collapsed: ReadonlySet<string>;
  onToggle: (key: string) => void;
  onOpen: (id: string) => void;
}

function AppsTable({ rows, fleet, caption, collapsed, onToggle, onOpen }: TableProps) {
  const { t, i18n } = useLingui();
  const { servers } = useFleetView();
  const words = useAppWords();
  const time = useTimeWords();
  const now = useControlPlaneNow(30_000);
  const stackTitle = useStackTitle();
  const columns = useMemo<TableColumn<Row>[]>(() => {
    const out: TableColumn<Row>[] = [
      {
        key: "app",
        header: t`App`,
        render: (row) => {
          if (row.kind === "stack") {
            const count = row.group.services.length;
            const expanded = !collapsed.has(row.group.key);
            const title = stackTitle(row.group);
            return (
              <div className="apps-stack">
                <button
                  type="button"
                  className="apps-stack-toggle"
                  aria-expanded={expanded}
                  aria-label={expanded ? t`Fold ${title}` : t`Unfold ${title}`}
                  onClick={() => onToggle(row.group.key)}
                >
                  <Icon name={expanded ? "chevron-down" : "chevron-right"} size={14} />
                </button>
                <span className="dw-cell">
                  <span className="dw-cell-title apps-clip">{title}</span>
                  <span className="apps-stack-sub">
                    <Icon name="layers" size={12} />
                    {plural(count, { one: "Stack of # service", other: "Stack of # services" })}
                  </span>
                </span>
              </div>
            );
          }
          const service = row.service as ServiceInfo;
          return (
            <span className="dw-cell">
              {/* The row takes the keyboard; the link is for a new tab, and for screen readers' lists of links. */}
              <Link
                to="/apps/$serviceId"
                params={{ serviceId: service.id }}
                className="dw-cell-title apps-clip apps-name"
                tabIndex={-1}
              >
                {nameOf(service)}
              </Link>
              <span className="dw-cell-sub apps-clip" title={imageOf(service)}>
                {imageOf(service)}
              </span>
            </span>
          );
        },
      },
      {
        key: "status",
        header: t`Status`,
        width: 152,
        render: (row) => {
          const state: AppState =
            row.kind === "stack" ? groupState(row.group, servers) : appState(row.service as ServiceInfo, servers);
          return <StatusBadge status={state.status} label={words.state(state)} labelIsState className="apps-status" />;
        },
      },
      {
        key: "revision",
        header: t`Revision`,
        width: 88,
        mono: true,
        render: (row) => (row.service ? revisionName(row.service.revision) : null),
      },
    ];
    if (fleet) {
      out.push({
        key: "servers",
        header: t`Runs on`,
        width: 168,
        muted: true,
        render: (row) => (row.service ? <RunsOn service={row.service} /> : <StackServers group={row.group} />),
      });
    }
    out.push({
      key: "deployed",
      header: t`Deployed`,
      width: 104,
      align: "right",
      muted: true,
      render: (row) => {
        const when = row.service ? deployedAt(row.service) : groupDeployedAt(row.group);
        return (
          <time dateTime={when.toISOString()} title={formatDateTime(when, i18n.locale)}>
            {time.ago(when, now)}
          </time>
        );
      },
    });
    return out;
  }, [t, i18n, words, servers, fleet, time, now, collapsed, onToggle, stackTitle]);

  return (
    <Table<Row>
      className="apps-table"
      columns={columns}
      rows={rows}
      caption={caption}
      rowClassName={(row) =>
        cx(row.kind === "stack" && "is-stack", row.kind === "member" && "is-member", row.last && "is-last")
      }
      onRowClick={(row) => {
        if (row.kind === "stack") onToggle(row.group.key);
        else if (row.service) onOpen(row.service.id);
      }}
    />
  );
}

/** Where a service runs: its servers' names, or how many once they don't fit. */
function RunsOn({ service }: { service: ServiceInfo }) {
  const names = service.deployments.map((d) => d.serverName);
  if (names.length > 2) {
    const count = names.length;
    return <span title={names.join(", ")}>{plural(count, { one: "# server", other: "# servers" })}</span>;
  }
  return <code className="apps-servers">{names.join(", ")}</code>;
}

function StackServers({ group }: { group: AppGroup }) {
  const names = [...new Set(group.services.flatMap((s) => s.deployments.map((d) => d.serverName)))];
  if (names.length === 1) return <code className="apps-servers">{names[0]}</code>;
  const count = names.length;
  return <span title={names.join(", ")}>{plural(count, { one: "# server", other: "# servers" })}</span>;
}

/** The list on a phone: stacked rows of a name and a line of state, as M-Apps draws them. */
function PhoneList({
  rows,
  collapsed,
  onToggle,
}: {
  rows: readonly Row[];
  collapsed: ReadonlySet<string>;
  onToggle: (key: string) => void;
}) {
  const { servers, fleet } = useFleetView();
  const words = useAppWords();
  const stackTitle = useStackTitle();
  return (
    <Panel flush className="apps-phone">
      {rows.map((row) => {
        if (row.kind === "stack") {
          const state = groupState(row.group, servers);
          const count = row.group.services.length;
          const expanded = !collapsed.has(row.group.key);
          return (
            <button
              type="button"
              key={row.id}
              className="apps-phone-stack"
              aria-expanded={expanded}
              onClick={() => onToggle(row.group.key)}
            >
              <span className={cx("apps-phone-glyph", `dw-tone-${STATUS[state.status].tone}`)}>
                <Glyph shape={STATUS[state.status].shape} size={10} />
              </span>
              <span className="apps-phone-text">
                <span className="apps-phone-name">{stackTitle(row.group)}</span>
                <span className="apps-phone-sub">
                  <span className={cx(state.kind !== "live" && `apps-phone-word dw-tone-${STATUS[state.status].tone}`)}>
                    {words.state(state)}
                  </span>
                  <span aria-hidden="true">·</span>
                  <span>{plural(count, { one: "Stack of # service", other: "Stack of # services" })}</span>
                </span>
              </span>
              <Icon name={expanded ? "chevron-down" : "chevron-right"} size={16} className="apps-phone-chevron" />
            </button>
          );
        }
        const service = row.service as ServiceInfo;
        const state = appState(service, servers);
        return (
          <Link
            key={row.id}
            to="/apps/$serviceId"
            params={{ serviceId: service.id }}
            className={cx("apps-phone-row", row.kind === "member" && "is-member")}
          >
            <span className={cx("apps-phone-glyph", `dw-tone-${STATUS[state.status].tone}`)}>
              <Glyph shape={STATUS[state.status].shape} size={10} />
            </span>
            <span className="apps-phone-text">
              <span className="apps-phone-name">{nameOf(service)}</span>
              <span className="apps-phone-sub">
                <span className={cx(state.kind !== "live" && `apps-phone-word dw-tone-${STATUS[state.status].tone}`)}>
                  {words.state(state)}
                </span>
                <span aria-hidden="true">·</span>
                <code className="apps-phone-rev">{revisionName(service.revision)}</code>
                <span aria-hidden="true">·</span>
                <span className="apps-phone-image">
                  {fleet ? service.deployments.map((d) => d.serverName).join(", ") : imageOf(service)}
                </span>
              </span>
            </span>
            <Icon name="chevron-right" size={16} className="apps-phone-chevron" />
          </Link>
        );
      })}
    </Panel>
  );
}

/** While the first list is on its way: the shape of the table, so nothing moves when it lands. */
export function ListSkeleton({ label }: { label: string }) {
  const widths = ["44%", "32%", "38%"];
  return (
    <Panel flush className="apps-skeleton">
      <div aria-busy="true">
        <span className="dw-sr">{label}</span>
        {widths.map((width) => (
          <div key={width} className="apps-skeleton-row" aria-hidden="true">
            <span className="apps-skeleton-bar" style={{ width }} />
            <span className="apps-skeleton-bar is-short" />
          </div>
        ))}
      </div>
    </Panel>
  );
}
