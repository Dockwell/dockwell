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
import { type ReactNode, useDeferredValue, useEffect, useMemo, useRef, useState } from "react";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useTimeWords } from "../../lib/time";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import {
  type AppGroup,
  appState,
  deployedAt,
  environmentOf,
  groupApps,
  groupDeployedAt,
  groupNeedsALook,
  groupState,
  imageOf,
  keepOrder,
  matches,
  nameOf,
  needsALook,
  recordSaysMore,
  revisionName,
  shortImage,
  sortGroups,
} from "./app";
import { ExportSheet } from "./ExportSheet";
import { HealthBadge, phoneWordClass, useHealthWords } from "./Health";
import { type AppHealth, type HealthView, healthNeedsALook, type Unread, useHealth, worstHealth } from "./health";
import { ImageName } from "./ImageName";
import { usePlaces, useServices } from "./useServices";
import { useAppWords } from "./words";
import "./Apps.css";
import { ReadFailed } from "../shell/ReadFailed";
import { useServicesWatch } from "./ServicesLive";

/** The list's place, in its URL: its filters. */
export interface AppsSearch {
  q?: string;
  environment?: string;
  server?: string;
}

/** A filter earns its place once there are more apps than fit on a screen at a glance. */
const FILTER_FROM = 6;

/** How long the list waits for how its apps run and where, to order them by it, before it orders them by what it has. */
const ORDER_WAIT_MS = 3_000;

/** The most rows the list holds the place of while it waits. */
const HELD_ROWS = 12;

/** Room for a revision, such as r12, and for what a record most often says beside it, Rolled back or Deploying, on one line. */
const REVISION_WIDTH = 152;

/** A SegmentedControl holds up to five; past that, a select. */
const SEGMENTS = 5;

/** From this wide, the image has a column of its own, as the eye shouldn't have to cross the row for it. */
const IMAGE_COLUMN_QUERY = "(min-width: 1100px)";

/** Where the list remembers what is folded, and how many apps there were. Conveniences of this browser alone. */
const FOLDED_KEY = "dockwell.apps.folded";
const COUNT_KEY = "dockwell.apps.count";

function remembered(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function remember(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Private windows and full disks forget, and the list works without it.
  }
}

/** What was folded, as it was left, so moving away and back keeps it. */
function useFolded() {
  const [folded, setFolded] = useState<ReadonlySet<string>>(() => {
    try {
      const stored: unknown = JSON.parse(remembered(FOLDED_KEY) ?? "[]");
      return new Set(Array.isArray(stored) ? stored.filter((k) => typeof k === "string") : []);
    } catch {
      return new Set();
    }
  });
  const toggle = (key: string) =>
    setFolded((was) => {
      const next = new Set(was);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      remember(FOLDED_KEY, JSON.stringify([...next]));
      return next;
    });
  return [folded, toggle] as const;
}

/** One row of the list: an app, a project's head, or a service in a project. */
interface Row {
  id: string;
  kind: "app" | "stack" | "member";
  group: AppGroup;
  service?: ServiceInfo;
  /** The last service of its project. */
  last: boolean;
}

/**
 * Every app: how it runs now, from what its server says; what was deployed and how
 * that went, kept apart; and when (Apps-List, M-Apps). Columns follow what the API has
 * and how many servers there are: no server column with one server. A project's
 * services show as one row with its name that folds, and an app in the default project
 * as a row of its own (ADR 0026). What needs you comes first; the rest are newest first.
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
  const { reconnecting } = useServicesWatch();
  const { servers, fleet } = useFleetView();
  const places = usePlaces();
  const health = useHealth();
  const navigate = useNavigate();
  const [exporting, setExporting] = useState(false);
  const [collapsed, toggle] = useFolded();
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
  // Environments stay out of sight until a project has a choice of them (ADR 0014).
  const choosing = places.showsEnvironment;
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
  const sorted = useMemo(
    () => sortGroups(groupApps(shown, places.place), servers, (s) => health.of(s)),
    [shown, places.place, servers, health],
  );
  // What needs a look comes first, which takes how each runs and where: the list is
  // ordered once both have come, or once it has waited long enough for them, and its
  // rows hold their place until then. After that each keeps its place, so nothing
  // moves under the pointer as a deploy starts or ends, or as something goes wrong,
  // which the lead offers to bring up.
  const settled = health.settled && servers !== undefined;
  const [waited, setWaited] = useState(false);
  useEffect(() => {
    if (settled) return;
    const timer = setTimeout(() => setWaited(true), ORDER_WAIT_MS);
    return () => clearTimeout(timer);
  }, [settled]);
  // Once ordered, it stays so, as a read back from the background is Checking for a moment.
  const orderedOnce = useRef(false);
  if (settled || waited) orderedOnce.current = true;
  const ordered = orderedOnce.current;
  // Each time someone asks to see what needs a look first, the order starts over.
  const [epoch, setEpoch] = useState(0);
  const order = useRef<{ epoch: number; keys: string[] }>({ epoch: 0, keys: [] });
  const groups = useMemo(() => {
    if (!ordered) return sorted;
    const kept = keepOrder(order.current.epoch === epoch ? order.current.keys : [], sorted);
    order.current = { epoch, keys: kept.order };
    return kept.groups;
  }, [sorted, ordered, epoch]);
  // Something that needs a look below something that doesn't, as it went wrong since the list was ordered.
  const outOfPlace = useMemo(() => {
    let calm = false;
    for (const group of groups) {
      if (!groupNeedsALook(group, servers, (s) => health.of(s))) calm = true;
      else if (calm) return true;
    }
    return false;
  }, [groups, servers, health]);
  const bringUp = () => setEpoch((n) => n + 1);
  const filtering = query.trim() !== "";
  const rows = useMemo(() => {
    const out: Row[] = [];
    for (const group of groups) {
      // A project's name finds all of it; otherwise what matches is what shows.
      const whole = group.name !== "" && group.name.toLowerCase().includes(query.trim().toLowerCase());
      const members = whole ? group.services : group.services.filter((s) => matches(s, query));
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

  const pending = services.isPending || places.isPending || (!ordered && (all?.length ?? 0) > 0);
  const count = all?.length ?? 0;
  // The filter's row is there or not by how many apps there are: remembering
  // last time's count keeps the list from moving when they land.
  useEffect(() => {
    if (all) remember(COUNT_KEY, String(all.length));
  }, [all]);
  const filters = pending ? Number(remembered(COUNT_KEY) ?? 0) >= FILTER_FROM : count >= FILTER_FROM;
  const elsewhere = count - shown.length;
  const { lead, total, going, trouble, needing } = useAppsLead(
    shown,
    environment ? places.label(environment) : "",
    health,
  );
  const later = useLaterTrouble(needing, trouble, settled);
  const showFirst = outOfPlace ? (
    <Button variant="ghost" size="sm" className="apps-show-first" onClick={bringUp}>
      <Trans>Show them first</Trans>
    </Button>
  ) : null;
  const newApp = (
    <RouterButtonLink to="/apps/new" variant="primary" icon="plus" className="apps-new">
      <Trans>New app</Trans>
    </RouterButtonLink>
  );
  // One environment is one Compose file, so export where it is clear which.
  const oneEnvironment = environment !== "" || places.environments.length === 1;

  let body: ReactNode;
  if (pending) {
    body = (
      <AppsSkeleton label={t`Loading apps`} fleet={fleet} rows={Math.min(Math.max(all?.length ?? 3, 1), HELD_ROWS)} />
    );
  } else if (services.error) {
    body = (
      <ReadFailed
        error={services.error}
        title={t`Can't load the apps`}
        actions={
          <Button onClick={() => void services.refetch()} loading={services.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        <Trans>What runs on your servers isn't affected.</Trans>
      </ReadFailed>
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
            <Trans>There are no apps here yet. Choose another environment, or make one with New app.</Trans>
          )}
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = <PhoneList rows={rows} collapsed={collapsed} onToggle={toggle} health={health} />;
  } else {
    body = (
      <AppsTable
        rows={rows}
        fleet={fleet}
        caption={t`Apps`}
        collapsed={collapsed}
        onToggle={toggle}
        onOpen={open}
        health={health}
      />
    );
  }

  const envOptions = environments.map((e) => ({ value: e.id, label: e.label, count: e.count }));
  return (
    <Page
      title={t`Apps`}
      // The line is held while they load, so the list doesn't move when it lands.
      lead={
        pending || count === 0 ? (
          <span aria-hidden="true">&nbsp;</span>
        ) : (
          <>
            {lead}
            {showFirst ? <> {showFirst}</> : null}
          </>
        )
      }
      status={reconnecting ? <Reconnecting /> : undefined}
      className="apps-page"
      actions={
        <>
          {count > 0 && oneEnvironment ? (
            <Button icon="download" onClick={() => setExporting(true)}>
              <Trans>Export</Trans>
            </Button>
          ) : null}
          {/* The empty state offers it, a view has one primary button, and until the list lands which it is isn't known. */}
          {pending || count === 0 ? null : newApp}
        </>
      }
    >
      <section className={cx("apps-section", filters && "has-filters")} aria-label={t`Apps`}>
        {choosing || fleet || filters ? (
          <div className="apps-filters">
            {filters ? (
              <TextField
                className="apps-filter"
                size={phone ? "lg" : "sm"}
                icon="search"
                type="search"
                placeholder={t`Filter by name or image`}
                aria-label={t`Filter apps`}
                value={typed}
                disabled={pending}
                onChange={(event) => setTyped(event.target.value)}
              />
            ) : null}
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
          </div>
        ) : null}
        {phone && (pending || count > 0) ? (
          <div className="apps-phone-head">
            <h2 className="dw-text-heading apps-phone-count">
              {pending ? (
                <>&nbsp;</>
              ) : (
                <>
                  <span>{total}</span>
                  {going ? <span className="apps-phone-trouble">{going}</span> : null}
                </>
              )}
            </h2>
            {pending ? null : showFirst}
            {pending ? null : newApp}
          </div>
        ) : null}
        {/* What goes wrong later moves nothing on the screen, so someone not looking at it has to be told. */}
        <p className="dw-sr" aria-live="polite">
          {later && outOfPlace ? t`${later}. Show them first puts them at the top of the list.` : later}
        </p>
        {health.unread && !pending && count > 0 ? (
          <UnreadNote why={health.unread.why} heard={health.unread.heard} />
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

/** Why nothing here says how its apps run, said once above them rather than on each row. */
function UnreadNote({ why, heard }: { why: Unread; heard: Date | undefined }) {
  const words = useHealthWords();
  const now = useAgoNow(heard);
  return (
    <Callout tone="info" shape="dashed-ring" className="apps-unread">
      {words.unreadAll(why, heard, now)}
    </Callout>
  );
}

/** The line under the title: how many, and what is going on with them. The overview's apps say it too. */
export function useAppsLead(
  services: readonly ServiceInfo[],
  where: string,
  health: HealthView,
): { lead: string; total: string; going: string; trouble: string; needing: number } {
  const { servers } = useFleetView();
  const { t } = useLingui();
  const count = services.length;
  const total = where
    ? plural(count, { one: `# app in ${where}`, other: `# apps in ${where}` })
    : plural(count, { one: "# app", other: "# apps" });
  let moving = 0;
  let needing = 0;
  for (const s of services) {
    const state = appState(s, servers);
    if (state.kind === "deploying") moving++;
    else if (needsALook(state) || healthNeedsALook(health.of(s))) needing++;
  }
  const trouble = needing > 0 ? plural(needing, { one: "# needs a look", other: "# need a look" }) : "";
  const going = [trouble, moving > 0 ? t`${moving} deploying` : ""].filter(Boolean).join(" · ");
  return { lead: going ? `${total} · ${going}` : total, total, going, trouble, needing };
}

/**
 * What to tell someone who isn't looking at the list, in words, once more of it
 * needs a look than when they last heard. The first read isn't news, as the page
 * says that as it loads, and nothing is said again until there's more to say.
 */
export function useLaterTrouble(needing: number, trouble: string, settled: boolean): string {
  const [told, setTold] = useState("");
  const known = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (!settled) return;
    if (known.current !== undefined && needing > known.current) setTold(trouble);
    if (needing === 0) setTold("");
    known.current = needing;
  }, [needing, trouble, settled]);
  return told;
}

/**
 * The columns' heads, which the list and its skeleton share so nothing moves when it
 * lands. Nothing a row says moves them either: a revision is short, and what its record
 * says beside it wraps under it rather than widen the column.
 */
function useColumnHeads(fleet: boolean, wide: boolean) {
  const { t } = useLingui();
  return useMemo(() => {
    const heads: { key: string; header: string; width?: number | string; align?: "right" }[] = [
      { key: "app", header: t`App` },
    ];
    // On a fleet, Runs on takes what's left at the narrow end of this: App and Image share it.
    if (wide) heads.push({ key: "image", header: t`Image`, width: fleet ? undefined : "42%" });
    heads.push(
      { key: "status", header: t`Status`, width: 128 },
      { key: "revision", header: t`Revision`, width: REVISION_WIDTH },
    );
    if (fleet) heads.push({ key: "servers", header: t`Runs on`, width: 168 });
    heads.push({ key: "deployed", header: t`Deployed`, width: 104, align: "right" });
    return heads;
  }, [t, fleet, wide]);
}

interface TableProps {
  rows: readonly Row[];
  fleet: boolean;
  caption: string;
  collapsed: ReadonlySet<string>;
  onToggle: (key: string) => void;
  onOpen: (id: string) => void;
  health: HealthView;
}

/** How a row runs: an app's, or the worst of a project's. */
function rowHealth(row: Row, health: HealthView): AppHealth {
  return row.service ? health.of(row.service) : worstHealth(row.group.services.map((s) => health.of(s)));
}

/** A word for the tooltip of a row: how it runs and, where it says one thing, why. */
function useRowTitle() {
  const words = useHealthWords();
  return (health: AppHealth, now: number): string => {
    if (health.read === "failed") return words.unread(health, now);
    const [only] = health.servers;
    return only && health.servers.length === 1 ? `${words.word(health)}: ${words.says(only, now)}` : words.word(health);
  };
}

function AppsTable({ rows, fleet, caption, collapsed, onToggle, onOpen, health }: TableProps) {
  const { t, i18n } = useLingui();
  const { servers } = useFleetView();
  const words = useAppWords();
  const titleOf = useRowTitle();
  const time = useTimeWords();
  const wide = useMediaQuery(IMAGE_COLUMN_QUERY);
  const heads = useColumnHeads(fleet, wide);
  const newest = useMemo(() => {
    let out: Date | undefined;
    for (const row of rows) {
      const when = row.service ? deployedAt(row.service) : groupDeployedAt(row.group);
      if (!out || when > out) out = when;
    }
    return out;
  }, [rows]);
  const now = useAgoNow(newest);
  const columns = useMemo<TableColumn<Row>[]>(() => {
    const render: Record<string, (row: Row) => ReactNode> = {
      app: (row) => {
        if (row.kind === "stack") {
          const count = row.group.services.length;
          const expanded = !collapsed.has(row.group.key);
          const title = row.group.name;
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
                <span className="dw-cell-title apps-clip" title={title}>
                  {title}
                </span>
                {wide ? null : (
                  <span className="apps-stack-sub">
                    <Icon name="layers" size={12} />
                    {plural(count, { one: "# app", other: "# apps" })}
                  </span>
                )}
              </span>
            </div>
          );
        }
        const service = row.service as ServiceInfo;
        const name = row.kind === "app" && row.group.name ? row.group.name : nameOf(service);
        return (
          <span className="dw-cell">
            {/* The row takes the keyboard; the link is for a new tab, and for screen readers' lists of links. */}
            <Link
              to="/apps/$serviceId"
              params={{ serviceId: service.id }}
              className="dw-cell-title apps-clip apps-name"
              title={name}
              tabIndex={-1}
            >
              {name}
            </Link>
            {wide ? null : <ImageName image={imageOf(service)} className="dw-cell-sub" />}
          </span>
        );
      },
      image: (row) => {
        if (row.service) return <ImageName image={imageOf(row.service)} className="apps-image" />;
        const count = row.group.services.length;
        return (
          <span className="apps-stack-sub">
            <Icon name="layers" size={12} />
            {plural(count, { one: "# app", other: "# apps" })}
          </span>
        );
      },
      status: (row) => {
        const runs = rowHealth(row, health);
        return <HealthBadge health={runs} className="apps-status" title={titleOf(runs, now)} />;
      },
      revision: (row) => {
        const state = row.service ? appState(row.service, servers) : groupState(row.group, servers);
        const word = words.state(state);
        return (
          <span className="apps-record">
            {row.service ? revisionName(row.service.revision) : <span aria-hidden="true">—</span>}
            {recordSaysMore(state) ? (
              <StatusBadge status={state.status} label={word} labelIsState className="apps-record-state" title={word} />
            ) : null}
          </span>
        );
      },
      servers: (row) => (row.service ? <RunsOn service={row.service} /> : <StackServers group={row.group} />),
      deployed: (row) => {
        const when = row.service ? deployedAt(row.service) : groupDeployedAt(row.group);
        return (
          <time dateTime={when.toISOString()} title={formatDateTime(when, i18n.locale)}>
            {time.ago(when, now)}
          </time>
        );
      },
    };
    return heads.map((head) => ({
      ...head,
      ...(head.key === "revision" ? { mono: true } : {}),
      ...(head.key === "servers" || head.key === "deployed" ? { muted: true } : {}),
      render: render[head.key] as (row: Row) => ReactNode,
    }));
  }, [t, i18n, words, servers, time, now, collapsed, onToggle, wide, heads, health, titleOf]);

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
  return (
    <code className="apps-servers apps-clip" title={names.join(", ")}>
      {names.join(", ")}
    </code>
  );
}

function StackServers({ group }: { group: AppGroup }) {
  const names = [...new Set(group.services.flatMap((s) => s.deployments.map((d) => d.serverName)))];
  if (names.length === 1) return <code className="apps-servers apps-clip">{names[0]}</code>;
  const count = names.length;
  return <span title={names.join(", ")}>{plural(count, { one: "# server", other: "# servers" })}</span>;
}

/** The list on a phone: stacked rows of a name and a line of state, as M-Apps draws them. */
function PhoneList({
  rows,
  collapsed,
  onToggle,
  health,
}: {
  rows: readonly Row[];
  collapsed: ReadonlySet<string>;
  onToggle: (key: string) => void;
  health: HealthView;
}) {
  const { servers, fleet } = useFleetView();
  const words = useAppWords();
  const healthWords = useHealthWords();
  return (
    <Panel flush className="apps-phone">
      {rows.map((row) => {
        if (row.kind === "stack") {
          const state = groupState(row.group, servers);
          const runs = rowHealth(row, health);
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
              <span className={cx("apps-phone-glyph", `dw-tone-${STATUS[runs.status].tone}`)}>
                <Glyph shape={STATUS[runs.status].shape} size={10} />
              </span>
              <span className="apps-phone-text">
                <span className="apps-phone-name">{row.group.name}</span>
                <span className="apps-phone-sub">
                  <span className={phoneWordClass(STATUS[runs.status].tone)}>{healthWords.word(runs)}</span>
                  {recordSaysMore(state) ? (
                    <>
                      <span aria-hidden="true">·</span>
                      <span className={phoneWordClass(STATUS[state.status].tone)}>{words.state(state)}</span>
                    </>
                  ) : null}
                  <span aria-hidden="true">·</span>
                  <span>{plural(count, { one: "# app", other: "# apps" })}</span>
                </span>
              </span>
              <Icon name={expanded ? "chevron-down" : "chevron-right"} size={16} className="apps-phone-chevron" />
            </button>
          );
        }
        const service = row.service as ServiceInfo;
        const state = appState(service, servers);
        const runs = health.of(service);
        const image = imageOf(service);
        const where = service.deployments.map((d) => d.serverName).join(", ");
        return (
          <Link
            key={row.id}
            to="/apps/$serviceId"
            params={{ serviceId: service.id }}
            className={cx("apps-phone-row", row.kind === "member" && "is-member")}
          >
            <span className={cx("apps-phone-glyph", `dw-tone-${STATUS[runs.status].tone}`)}>
              <Glyph shape={STATUS[runs.status].shape} size={10} />
            </span>
            <span className="apps-phone-text">
              <span className="apps-phone-name">
                {row.kind === "app" && row.group.name ? row.group.name : nameOf(service)}
              </span>
              <span className="apps-phone-sub">
                <span className={phoneWordClass(STATUS[runs.status].tone)}>{healthWords.word(runs)}</span>
                {recordSaysMore(state) ? (
                  <>
                    <span aria-hidden="true">·</span>
                    <span className={phoneWordClass(STATUS[state.status].tone)}>{words.state(state)}</span>
                  </>
                ) : null}
                <span aria-hidden="true">·</span>
                <code className="apps-phone-rev">{revisionName(service.revision)}</code>
                <span aria-hidden="true">·</span>
                <span className="apps-phone-image" title={fleet ? where : image}>
                  {fleet ? where : shortImage(image)}
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

/** While the list is on its way: the table with its heads, so nothing moves when it lands. */
function AppsSkeleton({ label, fleet, rows = 3 }: { label: string; fleet: boolean; rows?: number }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const wide = useMediaQuery(IMAGE_COLUMN_QUERY);
  const heads = useColumnHeads(fleet, wide);
  const columns = useMemo<TableColumn<{ id: string }>[]>(
    () =>
      heads.map((head) => ({
        ...head,
        render: () => <span className="apps-skeleton-bar" style={{ width: head.key === "app" ? "44%" : "64%" }} />,
      })),
    [heads],
  );
  if (phone) return <ListSkeleton label={label} rows={rows} />;
  return (
    <div aria-busy="true">
      <span className="dw-sr">{label}</span>
      <Table
        className="apps-table apps-skeleton"
        columns={columns}
        rows={Array.from({ length: rows }, (_, i) => ({ id: String(i + 1) }))}
        rowClassName={() => "apps-skeleton-row"}
      />
    </div>
  );
}

/** While a first list is on its way: the shape of its rows. */
export function ListSkeleton({ label, rows = 3 }: { label: string; rows?: number }) {
  // Each line is its place, which is all a key needs to say of a line that holds one.
  const lines = Array.from({ length: rows }, (_, i) => ({ key: `line-${i + 1}`, width: ["44%", "32%", "38%"][i % 3] }));
  return (
    <Panel flush className="apps-skeleton">
      <div aria-busy="true">
        <span className="dw-sr">{label}</span>
        {lines.map(({ key, width }) => (
          <div key={key} className="apps-skeleton-line" aria-hidden="true">
            <span className="apps-skeleton-bar" style={{ width }} />
            <span className="apps-skeleton-bar is-short" />
          </div>
        ))}
      </div>
    </Panel>
  );
}
