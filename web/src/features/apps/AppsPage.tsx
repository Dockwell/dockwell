// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  DesiredRunState,
  DomainService,
  FileService,
  LogService,
  Protocol,
  type ServiceInfo,
  TerminalService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  SkeletonRows,
  STATUS,
  StatusBadge,
  Table,
  type TableColumn,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useNow, useTimeWords } from "../../lib/time";
import { useMay } from "../auth/may";
import { publishedPortUrl } from "../docker/publishedPort";
import { askable } from "../files/lookup";
import { StartChoices } from "../overview/StartChoices";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  BatchBar,
  type BatchRefusal,
  cardContextMenu,
  combineBatchActions,
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  RowActions,
  type RowSelection,
  SelectBox,
  SortHeader,
  selectionColumn,
  useBatchAction,
  useListPlace,
  useRowMenu,
  useRowSelection,
} from "../shell/ListTools";
import { LIST_PAGE_SIZE, type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { useRuntimes } from "../websites/website";
import {
  type AppGroup,
  appOf,
  appState,
  deployedAt,
  environmentOf,
  groupApps,
  groupDeployedAt,
  groupNeedsALook,
  groupState,
  imageOf,
  isMoving,
  keepOrder,
  matches,
  nameOf,
  needsALook,
  recordSaysMore,
  revisionName,
  shortImage,
  sortGroups,
} from "./app";
import { useDeleteApps } from "./deleteApp";
import { useDomainNames, visitorUrl } from "./domains";
import { ExportSheet } from "./ExportSheet";
import { HealthBadge, phoneWordClass, useHealthWords } from "./Health";
import { type AppHealth, type HealthView, healthNeedsALook, type Unread, useHealth, worstHealth } from "./health";
import { WhatRuns } from "./ImageName";
import { MoveSheet } from "./MoveSheet";
import { runtimeOf, runtimeWords } from "./runtime";
import { PrepareServiceDelete, PrepareServiceUpdate, useServiceRowActions } from "./ServiceRowActions";
import { PickBox, SelectPatches, type UpdatePicks, UpdatePicksBar, useUpdatePicks } from "./UpdateSeveral";
import { counts, offered, ofInstall, StackUpdates, UpdateMark, updateCount, useMayUpdate } from "./updates";
import { useApps, usePlaces } from "./useServices";
import { useAppWords } from "./words";
import { useWorkloadControls } from "./workload";
import "./Apps.css";
import { useDatabases } from "../databases/useDatabases";
import { ReadFailed } from "../shell/ReadFailed";
import { RefLink } from "../shell/RefLink";
import { useServicesWatch } from "./ServicesLive";

/** The list's place, in its URL: its filters, its sort and its page. */
export interface AppsSearch extends ListSearch {
  environment?: string;
  server?: string;
  /** "updates" shows only the apps with an update that counts (ADR 0079). */
  show?: string;
}

/** The columns the list sorts by when asked, as its address names them. */
const SORT_KEYS = ["app", "deployed"] as const;

/** A filter earns its place once there are more apps than fit on a screen at a glance. */
const FILTER_FROM = 6;

/** How long the list waits for how its apps run and where, to order them by it, before it orders them by what it has. */
const ORDER_WAIT_MS = 3_000;

/** The most rows the list holds the place of while it waits. */
const HELD_ROWS = 12;

/** Room for a revision, such as r12, and for what a record most often says beside it, Rolled back or Deploying, on one line. */
const REVISION_WIDTH = 152;

/** How often whether an update counts is looked at again: it moves only as a snooze ends. */
const UPDATES_TICK_MS = 60_000;

/** A SegmentedControl holds up to five; past that, a select. */
const SEGMENTS = 5;

/**
 * From this wide, where each app answers has a column, as Apps-List has it. Its
 * image is always under its name. A fleet's rows hold more, so its column waits
 * until there's room for the name beside it.
 */
const DOMAIN_COLUMN_QUERY = "(min-width: 1100px)";
const FLEET_DOMAIN_COLUMN_QUERY = "(min-width: 1280px)";

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
  const apps = useApps();
  const { reconnecting } = useServicesWatch();
  const { servers, fleet } = useFleetView();
  const places = usePlaces();
  const health = useHealth();
  const navigate = useNavigate();
  const [exporting, setExporting] = useState(false);
  const [collapsed, toggle] = useFolded();
  const place = useListPlace(search, onSearch, SORT_KEYS);
  const query = place.query;

  // A website's own services are its to show, on Websites (ADR 0039), as everywhere apps are counted.
  const all = apps.data;
  const environments = useMemo(() => {
    const counts = new Map<string, number>();
    for (const s of all ?? []) counts.set(environmentOf(s), (counts.get(environmentOf(s)) ?? 0) + 1);
    return places.environments.map((e) => ({ ...e, count: counts.get(e.id) ?? 0 }));
  }, [all, places.environments]);
  // Environments stay out of sight until a project has a choice of them (ADR 0014).
  const choosing = places.showsEnvironment;
  const environment = choosing ? (search.environment ?? "") : "";
  // Whether an update counts moves only with a snooze's end, so the minute is enough.
  const updatesNow = useNow(UPDATES_TICK_MS);
  // A catalog install's update is one, of the install, whichever of its apps carry it.
  const updates = useMemo(() => updateCount(all ?? [], updatesNow), [all, updatesNow]);
  // The filter to updates holds only while it has one to show: once the last is
  // applied, skipped or snoozed, it drops on its own and the whole list is back.
  const updating = search.show === "updates" && updates > 0;
  useEffect(() => {
    if (all !== undefined && search.show === "updates" && updates === 0) onSearch?.({ ...search, show: undefined });
  }, [all, search, updates, onSearch]);
  // Rows picked to update together, of those that can be (ADR 0080).
  const picks = useUpdatePicks(all, updatesNow);
  const shown = useMemo(
    () =>
      (all ?? []).filter(
        (s) =>
          (environment === "" || environmentOf(s) === environment) &&
          (!search.server || s.deployments.some((d) => d.serverId === search.server)) &&
          (!updating || counts(s.update, updatesNow)),
      ),
    [all, environment, search.server, updating, updatesNow],
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
  // Sorted by a head where one was asked for, over the whole list: what needs a look
  // first is the list's order until then, and the reader's sort replaces it.
  const sortState = place.sort;
  const orderedGroups = useMemo(
    () =>
      sortedRows(groups, sortState, (g, key) =>
        key === "deployed" ? groupDeployedAt(g).getTime() : g.name || (g.services[0] ? nameOf(g.services[0]) : ""),
      ),
    [groups, sortState],
  );
  // Where an app answers finds it too, as the filter's words say.
  const domainNames = useDomainNames();
  // Match an entire group by its name, or show the individual apps that match.
  const matching = useMemo(() => {
    const q = query.trim().toLowerCase();
    const answers = (s: ServiceInfo) =>
      q !== "" && (domainNames(environmentOf(s), [s]) ?? []).some((d) => d.toLowerCase().includes(q));
    const out: { group: AppGroup; members: ServiceInfo[] }[] = [];
    for (const group of orderedGroups) {
      // A project's name finds all of it; otherwise what matches is what shows.
      const whole = group.name !== "" && group.name.toLowerCase().includes(q);
      const members = whole ? group.services : group.services.filter((s) => matches(s, query) || answers(s));
      if (members.length > 0) out.push({ group, members });
    }
    return out;
  }, [orderedGroups, query, domainNames]);
  // Page over what shows on screen: a collapsed stack takes one row, so collapsing
  // a large stack never leaves a page half empty, and expanding fills the page.
  type PageSlot =
    | { kind: "collapsed"; group: AppGroup; members: ServiceInfo[] }
    | { kind: "service"; group: AppGroup; service: ServiceInfo };
  const visibleSlots = useMemo<PageSlot[]>(
    () =>
      matching.flatMap<PageSlot>(({ group, members }) =>
        group.stack && collapsed.has(group.key) && !filtering
          ? [{ kind: "collapsed", group, members }]
          : members.map((service) => ({ kind: "service", group, service })),
      ),
    [matching, collapsed, filtering],
  );
  const slotPage = pagedRows(visibleSlots, place.page);
  const totalApps = useMemo(() => matching.reduce((sum, item) => sum + item.members.length, 0), [matching]);
  const beforeApps = useMemo(
    () =>
      visibleSlots
        .slice(0, slotPage.page * LIST_PAGE_SIZE)
        .reduce((sum, slot) => sum + (slot.kind === "collapsed" ? slot.members.length : 1), 0),
    [visibleSlots, slotPage.page],
  );
  const pageApps = useMemo(
    () => slotPage.rows.reduce((sum, slot) => sum + (slot.kind === "collapsed" ? slot.members.length : 1), 0),
    [slotPage.rows],
  );
  const listPage = {
    rows: slotPage.rows,
    page: slotPage.page,
    pages: slotPage.pages,
    from: totalApps === 0 ? 0 : beforeApps + 1,
    to: beforeApps + pageApps,
    total: totalApps,
  };
  const rows: Row[] = [];
  let previous: AppGroup | undefined;
  for (const slot of slotPage.rows) {
    if (slot.kind === "collapsed") {
      rows.push({ id: `stack:${slot.group.key}`, kind: "stack", group: slot.group, last: false });
      previous = slot.group;
      continue;
    }
    const { group, service } = slot;
    if (!group.stack) {
      rows.push({ id: service.id, kind: "app", group, service, last: true });
      continue;
    }
    if (previous !== group) {
      rows.push({ id: `stack:${group.key}`, kind: "stack", group, last: false });
      previous = group;
    }
    rows.push({ id: service.id, kind: "member", group, service, last: false });
  }
  rows.forEach((row, index) => {
    if (row.kind === "member") row.last = rows[index + 1]?.group !== row.group;
  });

  const open = (id: string) => void navigate({ to: "/apps/$serviceId", params: { serviceId: id } });
  const manage = useServiceRowActions(environmentOf);
  // What a row does without opening the app: the everyday verbs, each through the
  // plan or page the app's own screen uses, and only what the reader may do.
  const may = useMay();
  const deleteApps = useDeleteApps();
  const mayUpdate = useMayUpdate();
  const { changeRun, changePause, changeRunMany, restart: restartApp } = useWorkloadControls();
  const [updatingApp, setUpdatingApp] = useState<ServiceInfo>();
  const selection = useRowSelection(
    rows.flatMap(({ service }) => (service ? [service.id] : [])),
    may(ChangeService.method.proposeChange).allowed,
  );
  const isStoppedApp = (service: ServiceInfo) => appOf(service)?.desiredRunState === DesiredRunState.STOPPED;
  const domains = useQuery(DomainService.method.listDomains, {}, { staleTime: Number.POSITIVE_INFINITY }).data?.domains;
  const visitFor = (service: ServiceInfo): { label: string; href: string } | undefined => {
    if (isStoppedApp(service)) return undefined;
    const environment = environmentOf(service);
    const name = nameOf(service);
    for (const domain of domains ?? []) {
      if (domain.environmentId !== environment) continue;
      const route = domain.routes.find((route) => route.service === name && !route.held);
      if (route) return { label: t`Open in browser`, href: visitorUrl(domain, route) };
    }
    const address = servers?.find((s) => s.id === service.deployments[0]?.serverId)?.address;
    for (const port of appOf(service)?.ports ?? []) {
      if (port.protocol === Protocol.UDP) continue;
      const href = publishedPortUrl(port.hostIp, port.hostPort, address);
      if (href) return { label: t`Open in browser`, href };
    }
    return undefined;
  };
  const settledApp = (service: ServiceInfo) => {
    const state = appState(service, servers);
    return !isMoving(state) && state.kind !== "unplaced" && state.kind !== "unreadable";
  };
  const actionsFor = (service: ServiceInfo): MenuAction[] => {
    const where = { environment: environmentOf(service) };
    const stopped = isStoppedApp(service);
    const paused = appOf(service)?.desiredRunState === DesiredRunState.PAUSED;
    const settled = settledApp(service);
    const visit = visitFor(service);
    const offer = counts(service.update, updatesNow) ? offered(service.update) : undefined;
    const shell = health
      .of(service)
      .servers.find((s) => s.container && s.kind !== "not-running" && s.kind !== "paused" && s.kind !== "unknown");
    const runtime = runtimeOf(service);
    const fileServer = service.deployments[0]?.serverId;
    const folder =
      runtime?.folder ?? appOf(service)?.mounts.find((mount) => mount.source.case === "hostPath")?.source.value;
    const filePath = typeof folder === "string" && fileServer && askable(fileServer, folder) ? folder : undefined;
    return [
      { id: "open", icon: "service", label: t`Open`, onSelect: () => open(service.id) },
      ...(visit ? [{ id: "visit", icon: "external" as const, label: visit.label, href: visit.href }] : []),
      ...(settled && service.resource
        ? [
            stopped
              ? {
                  id: "start",
                  icon: "play" as const,
                  label: t`Start`,
                  ...may(ChangeService.method.proposeChange, where).off,
                  onSelect: () => service.resource && changeRun(service.resource, service.revision, true),
                }
              : {
                  id: "stop",
                  icon: "stop" as const,
                  label: t`Stop`,
                  ...may(ChangeService.method.proposeChange, where).off,
                  onSelect: () => service.resource && changeRun(service.resource, service.revision, false),
                },
          ]
        : []),
      ...(settled && !stopped && service.resource
        ? [
            {
              id: paused ? "unpause" : "pause",
              icon: paused ? ("play" as const) : ("pause" as const),
              label: paused ? t`Unpause` : t`Pause`,
              ...may(ChangeService.method.proposeChange, where).off,
              onSelect: () => service.resource && changePause(service.resource, service.revision, !paused),
            },
          ]
        : []),
      ...(settled && !stopped && !paused
        ? [
            {
              id: "restart",
              icon: "refresh" as const,
              label: t`Restart`,
              ...may(ChangeService.method.restartService, where).off,
              onSelect: () => restartApp(service.id),
            },
          ]
        : []),
      ...(may(LogService.method.watchLogs, where).allowed
        ? [
            {
              id: "logs",
              icon: "file" as const,
              label: t`Logs`,
              onSelect: () => void navigate({ to: "/apps/$serviceId/logs", params: { serviceId: service.id } }),
            },
          ]
        : []),
      ...(shell && may(TerminalService.method.openTerminal).allowed
        ? [
            {
              id: "shell",
              icon: "terminal" as const,
              label: t`Terminal`,
              onSelect: () =>
                void navigate({
                  to: "/servers/$serverId/terminal",
                  params: { serverId: shell.serverId },
                  search: { containerId: shell.container, containerTitle: nameOf(service) },
                }),
            },
          ]
        : []),
      ...(filePath && fileServer && may(FileService.method.listFiles).allowed
        ? [
            {
              id: "files",
              icon: "folder" as const,
              label: t`Open files`,
              onSelect: () =>
                void navigate({
                  to: "/servers/$serverId/files",
                  params: { serverId: fileServer },
                  search: { path: filePath },
                }),
            },
          ]
        : []),
      ...(offer
        ? [
            {
              id: "update",
              icon: "arrow-up" as const,
              label: offer.toVersion ? t`Update to ${offer.toVersion}` : t`Update`,
              ...mayUpdate(service).off,
              onSelect: () => setUpdatingApp(service),
            },
          ]
        : []),
      ...manage.actionsFor(service),
    ];
  };
  // Picked rows act together: stopping or starting several is one plan with an
  // edit for each, and the bar says how many a verb leaves out.
  const selectedApps = rows.flatMap(({ service }) => (service && selection.has(service.id) ? [service] : []));
  const batchAction = useBatchAction<ServiceInfo>();
  const editRefusal = (app: ServiceInfo): BatchRefusal | undefined => {
    if (!app.resource) return { reason: t`App details are unavailable` };
    const permission = may(ChangeService.method.proposeChange, { environment: environmentOf(app) });
    return permission.allowed ? undefined : { permission };
  };
  const runBatch = (start: boolean) =>
    batchAction(
      selectedApps,
      (app) => {
        if (!settledApp(app)) return { reason: t`App state is changing or unavailable` };
        if (start !== isStoppedApp(app)) return { reason: start ? t`Already running` : t`Already stopped` };
        return editRefusal(app);
      },
      {
        id: start ? "start" : "stop",
        icon: start ? "play" : "stop",
        label: (count) => (start ? (count ? t`Start ${count}` : t`Start`) : count ? t`Stop ${count}` : t`Stop`),
        onSelect: (targets) =>
          changeRunMany(
            targets.flatMap((app) => (app.resource ? [{ resource: app.resource, baseRevision: app.revision }] : [])),
            start,
            start
              ? plural(targets.length, { one: "Start # app", other: "Start # apps" })
              : plural(targets.length, { one: "Stop # app", other: "Stop # apps" }),
            selection.clear,
          ),
      },
    );
  const batch = combineBatchActions(
    runBatch(true),
    runBatch(false),
    batchAction(selectedApps, editRefusal, {
      id: "delete",
      icon: "trash",
      danger: true,
      label: (count) => (count ? t`Delete ${count}` : t`Delete`),
      onSelect: (targets) => deleteApps(targets, selection.clear),
    }),
  );
  // The filter's words, or what the list is narrowed to: each clears what kept everything out.
  const clearFilters = (what: "q" | "rest") => {
    if (what === "q") place.clearQuery();
    else onSearch?.({ ...search, environment: undefined, server: undefined, show: undefined, page: undefined });
  };

  const pending = apps.isPending || places.isPending || (!ordered && (all?.length ?? 0) > 0);
  const count = all?.length ?? 0;
  // The filter's row is there or not by how many apps there are: remembering
  // last time's count keeps the list from moving when they land.
  useEffect(() => {
    if (all) remember(COUNT_KEY, String(all.length));
  }, [all]);
  const filters = pending ? Number(remembered(COUNT_KEY) ?? 0) >= FILTER_FROM : count >= FILTER_FROM || filtering;
  const elsewhere = count - shown.length;
  const { lead, total, going, trouble, needing } = useAppsLead(
    shown,
    environment ? places.label(environment) : "",
    health,
  );
  const later = useLaterTrouble(needing, trouble, settled);
  // A list sorted by its head is in the reader's order, not the list's, so nothing is out of place in it.
  const showFirst =
    outOfPlace && !sortState ? (
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
      <AppsSkeleton
        label={t`Loading apps`}
        fleet={fleet}
        rows={Math.min(Math.max(all?.length ?? (Number(remembered(COUNT_KEY)) || 3), 1), HELD_ROWS)}
      />
    );
  } else if (apps.error) {
    // Without the websites, a website's own web server would list as an app.
    body = (
      <ReadFailed
        error={apps.error}
        title={t`Can't load the apps`}
        actions={
          <Button onClick={() => void apps.refetch()} loading={apps.isFetching}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        <Trans>What runs on your servers isn't affected.</Trans>
      </ReadFailed>
    );
  } else if ((all?.length ?? 0) === 0) {
    // The ways in are the choices: each opens its own create flow, as the start page has them.
    body = (
      <EmptyState
        icon="service"
        title={<Trans>No apps yet</Trans>}
        choices={<StartChoices ways={["catalog", "git", "image", "compose"]} />}
      >
        <p>
          <Trans>You review a plan before anything starts.</Trans>
        </p>
      </EmptyState>
    );
  } else if (rows.length === 0) {
    body = <NothingHere query={filtering ? place.typed.trim() : ""} search={search} onClear={clearFilters} />;
  } else if (phone) {
    body = (
      <>
        <PhoneList
          rows={rows}
          collapsed={collapsed}
          onToggle={toggle}
          health={health}
          picks={picks}
          actionsFor={actionsFor}
          selection={selection}
        />
        <ListPager page={listPage} onPage={place.setPage} />
      </>
    );
  } else {
    body = (
      <>
        <AppsTable
          rows={rows}
          fleet={fleet}
          caption={t`Apps`}
          collapsed={collapsed}
          onToggle={toggle}
          onOpen={open}
          health={health}
          picks={picks}
          place={place}
          actionsFor={actionsFor}
          selection={selection}
        />
        <ListPager page={listPage} onPage={place.setPage} />
      </>
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
      <section
        className={cx("apps-section", filters && "has-filters")}
        aria-label={t`Apps`}
        ref={selection.list}
        tabIndex={-1}
        onKeyDown={selection.onKeyDown}
      >
        {choosing || fleet || filters || updates > 0 || (phone && count > 0) ? (
          <div className="apps-filters">
            {filters ? (
              <ListSearchField
                className="apps-filter"
                size={phone ? "lg" : "sm"}
                placeholder={t`Filter by name, image or domain`}
                label={t`Filter apps`}
                value={place.typed}
                disabled={pending}
                onChange={place.setTyped}
              />
            ) : null}
            {phone && count > 0 ? (
              <ListSortField place={place} columns={{ app: t`App`, deployed: t`Deployed` }} />
            ) : null}
            {choosing && envOptions.length <= SEGMENTS ? (
              <SegmentedControl
                options={[{ value: "", label: t`All` }, ...envOptions]}
                value={environment}
                label={t`Environment`}
                size="sm"
                onChange={(value) => onSearch?.({ ...search, environment: value || undefined, page: undefined })}
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
                onChange={(event) =>
                  onSearch?.({ ...search, environment: event.target.value || undefined, page: undefined })
                }
              />
            ) : null}
            {updates > 0 ? (
              <Button
                variant="ghost"
                size="sm"
                icon="arrow-up"
                className="apps-updates-filter"
                aria-pressed={updating}
                onClick={() => onSearch?.({ ...search, show: updating ? undefined : "updates", page: undefined })}
              >
                {plural(updates, { one: "# update available", other: "# updates available" })}
              </Button>
            ) : null}
            {updates > 0 && picks.selected.length === 0 ? <SelectPatches picks={picks} /> : null}
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
                onChange={(event) =>
                  onSearch?.({ ...search, server: event.target.value || undefined, page: undefined })
                }
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
        <BatchBar count={selectedApps.length} onClear={selection.clear} {...batch} />
        <UpdatePicksBar picks={picks} className="apps-picks-bar" />
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
      {manage.selected?.action === "move" ? (
        <MoveSheet service={manage.selected.service} onClose={manage.close} />
      ) : null}
      {manage.selected?.action === "delete" ? (
        <PrepareServiceDelete service={manage.selected.service} onClose={manage.close} />
      ) : null}
      {updatingApp ? <PrepareServiceUpdate service={updatingApp} onClose={() => setUpdatingApp(undefined)} /> : null}
    </Page>
  );
}

/**
 * The list is narrowed to nothing: what kept everything out, and a button that
 * clears it. The filter's words first, then an environment or a server.
 */
function NothingHere({
  query,
  search,
  onClear,
}: {
  query: string;
  search: AppsSearch;
  onClear: (what: "q" | "rest") => void;
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const { servers } = useFleetView();
  if (query !== "") {
    return (
      <EmptyState
        icon="search"
        title={<Trans>Nothing matches</Trans>}
        action={
          <Button size="lg" onClick={() => onClear("q")}>
            <Trans>Clear the filter</Trans>
          </Button>
        }
      >
        <p>
          <Trans>
            No app here has <code>{query}</code> in its name, image or domain.
          </Trans>
        </p>
      </EmptyState>
    );
  }
  const environment = search.environment && places.showsEnvironment ? places.label(search.environment) : "";
  const server = search.server ? (servers?.find((s) => s.id === search.server)?.name ?? "") : "";
  let title = t`No apps here`;
  let clear = t`Clear the filters`;
  if (environment && !server) {
    title = t`No apps in ${environment}`;
    clear = t`Show all environments`;
  } else if (server && !environment) {
    title = t`No apps on ${server}`;
    clear = t`Show all servers`;
  }
  return (
    <EmptyState
      icon="search"
      title={title}
      action={
        <Button size="lg" onClick={() => onClear("rest")}>
          {clear}
        </Button>
      }
    />
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
    const heads: { key: string; header: ReactNode; width?: number | string; align?: "right"; muted?: boolean }[] = [
      { key: "app", header: t`App` },
    ];
    heads.push(
      { key: "status", header: t`Status`, width: 128 },
      { key: "revision", header: t`Revision`, width: fleet ? 140 : REVISION_WIDTH },
    );
    if (fleet) heads.push({ key: "servers", header: t`Runs on`, width: 144 });
    // A fixed width on a fleet, where Runs on takes room too: a share of the row left a name 27 px at 1280.
    if (wide) heads.push({ key: "domain", header: t`Domain`, width: fleet ? 120 : "24%", muted: true });
    heads.push({ key: "deployed", header: t`Deployed`, width: fleet ? 100 : 116, align: "right" });
    heads.push({ key: "actions", header: <span className="dw-sr">{t`Actions`}</span>, width: 80, align: "right" });
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
  picks: UpdatePicks;
  place: ListPlace;
  actionsFor: (service: ServiceInfo) => MenuAction[];
  selection: RowSelection;
}

/**
 * What a row picks to update: its app, or its project's apps, of those that can be
 * picked. An app of a catalog install picks nothing of its own: its update is the
 * install's, which the install's row picks.
 */
function picksOf(row: Row, picks: UpdatePicks): string[] {
  if (row.kind === "member" && row.service && ofInstall(row.service)) return [];
  const services = row.kind === "stack" ? row.group.services : row.service ? [row.service] : [];
  return services.filter((s) => picks.can(s.id)).map((s) => s.id);
}

/** Whether a row says an update of its own: not an app of a catalog install, whose install's row says it. */
function marksItsUpdate(row: Row, service: ServiceInfo): boolean {
  return !(row.kind === "member" && ofInstall(service));
}

/** A row's box for picking what it updates, where it has something to pick (ADR 0080). */
function RowPick({ row, picks, order }: { row: Row; picks: UpdatePicks; order: readonly string[] }) {
  const { t } = useLingui();
  const ids = picksOf(row, picks);
  if (ids.length === 0) return null;
  const on = ids.filter((id) => picks.has(id)).length;
  const name = row.kind === "stack" ? row.group.name : row.service ? nameOf(row.service) : "";
  if (row.kind === "stack") {
    return (
      <PickBox
        checked={on === ids.length}
        partly={on > 0}
        label={t`Select the updates in ${name}`}
        onPick={(yes) => picks.setAll(ids, yes)}
      />
    );
  }
  return (
    <PickBox
      checked={on > 0}
      label={t`Select ${name}'s update`}
      onPick={(yes, range) => picks.toggle(order, ids[0] as string, yes, range)}
    />
  );
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

function AppsTable({
  rows,
  fleet,
  caption,
  collapsed,
  onToggle,
  onOpen,
  health,
  picks,
  place,
  actionsFor,
  selection,
}: TableProps) {
  const { t, i18n } = useLingui();
  const { servers } = useFleetView();
  const words = useAppWords();
  const titleOf = useRowTitle();
  const time = useTimeWords();
  const wide = useMediaQuery(fleet ? FLEET_DOMAIN_COLUMN_QUERY : DOMAIN_COLUMN_QUERY);
  const heads = useColumnHeads(fleet, wide);
  const domainNames = useDomainNames();
  const databases = useDatabaseIds();
  const newest = useMemo(() => {
    let out: Date | undefined;
    for (const row of rows) {
      const when = row.service ? deployedAt(row.service) : groupDeployedAt(row.group);
      if (!out || when > out) out = when;
    }
    return out;
  }, [rows]);
  const now = useAgoNow(newest);
  const menu = useRowMenu<Row>();
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
                <span className="apps-name-line">
                  <span className="dw-cell-title apps-clip" title={title}>
                    {title}
                  </span>
                  <NameLabels app={row.group.app} />
                </span>
                <span className="apps-stack-sub">
                  <Icon name="layers" size={12} />
                  {plural(count, { one: "# app", other: "# apps" })}
                  <StackUpdates services={row.group.services} now={now} />
                </span>
              </span>
            </div>
          );
        }
        const service = row.service as ServiceInfo;
        const name = row.kind === "app" && row.group.name ? row.group.name : nameOf(service);
        return (
          <span className="dw-cell apps-name-cell">
            {/* The row takes the keyboard; the link is for a new tab, and for screen readers' lists of links. */}
            <span className="apps-name-line">
              <Link
                to="/apps/$serviceId"
                params={{ serviceId: service.id }}
                className="dw-cell-title apps-clip apps-name"
                title={name}
                tabIndex={-1}
              >
                {name}
              </Link>
              <NameLabels app={row.kind === "app" ? row.group.app : ""} database={databases.has(service.id)} />
            </span>
            {counts(service.update, now) && marksItsUpdate(row, service) ? (
              <span className="dw-cell-sub update-runs">
                <WhatRuns service={service} />
                <UpdateMark service={service} now={now} />
              </span>
            ) : (
              <WhatRuns service={service} className="dw-cell-sub" />
            )}
          </span>
        );
      },
      domain: (row) => {
        const services = row.service ? [row.service] : row.group.services;
        const names = domainNames(row.group.environment, services);
        // Not read yet, or can't be: nobody can say it has none.
        if (names === undefined) return null;
        const [domain, ...more] = names;
        if (!domain) return <span aria-hidden="true">—</span>;
        const others = more.length;
        return (
          <span className="apps-domain" title={[domain, ...more].join(", ")}>
            <code className="apps-clip">{domain}</code>
            {others > 0 ? <span className="apps-domain-more">{t`+${others}`}</span> : null}
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
            {row.service ? (
              revisionName(row.service.revision)
            ) : (
              <span aria-hidden="true" className="apps-record-none">
                —
              </span>
            )}
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
      actions: (row) => {
        if (!row.service) return null;
        const service = row.service;
        const name = nameOf(service);
        return <RowActions label={t`Actions for ${name}`} actions={actionsFor(service)} quick={["start", "stop"]} />;
      },
    };
    return heads.map((head) => ({
      ...head,
      ...(head.key === "app" || head.key === "deployed"
        ? {
            header: <SortHeader label={head.header as string} sortKey={head.key} place={place} />,
            sort: place.sortOf(head.key),
          }
        : {}),
      ...(head.key === "revision" ? { mono: true } : {}),
      ...(head.key === "servers" || head.key === "deployed" ? { muted: true } : {}),
      render: render[head.key] as (row: Row) => ReactNode,
    }));
  }, [
    t,
    i18n,
    words,
    servers,
    time,
    now,
    collapsed,
    onToggle,
    actionsFor,
    heads,
    health,
    titleOf,
    domainNames,
    databases,
    place,
  ]);
  // A column to pick rows by, while any row has an update to pick (ADR 0080).
  const order = rows.filter((r) => r.kind !== "stack").flatMap((r) => picksOf(r, picks));
  const shownPicks = [...new Set(rows.flatMap((r) => picksOf(r, picks)))];
  const picked = shownPicks.filter((id) => picks.has(id)).length;
  const withPicks: TableColumn<Row>[] =
    shownPicks.length === 0
      ? columns
      : [
          {
            key: "pick",
            width: 40,
            header: (
              <PickBox
                checked={picked === shownPicks.length}
                partly={picked > 0}
                label={t`Select every update shown`}
                onPick={(on) => picks.setAll(shownPicks, on)}
              />
            ),
            render: (row) => <RowPick row={row} picks={picks} order={order} />,
          },
          ...columns,
        ];

  return (
    <>
      <Table<Row>
        className={cx("apps-table", selection.any && "has-selection")}
        columns={[
          ...(selection.enabled
            ? [
                selectionColumn({
                  selection,
                  rows,
                  idOf: (row) => row.service?.id,
                  labelOf: (row) => t`Select ${row.service ? nameOf(row.service) : row.group.name}`,
                  allLabel: t`Select all rows on this page`,
                }),
              ]
            : []),
          ...withPicks,
        ]}
        rows={rows}
        caption={caption}
        rowClassName={(row) =>
          cx(
            row.kind === "stack" && "is-stack",
            row.kind === "member" && "is-member",
            row.last && "is-last",
            selection.has(row.id) && "is-checked",
          )
        }
        onRowClick={(row) => {
          if (row.kind === "stack") onToggle(row.group.key);
          else if (row.service) onOpen(row.service.id);
        }}
        rowHasContextMenu={(row) => row.service !== undefined}
        onRowContextMenu={(row, at, el) => {
          if (row.service)
            menu.open(row, at, el, { label: t`Actions for ${nameOf(row.service)}`, actions: actionsFor(row.service) });
        }}
      />
      {menu.element}
    </>
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
      {service.deployments.map((d, i) => (
        <span key={d.serverId}>
          {i > 0 ? ", " : null}
          <RefLink kind="server" id={d.serverId} look="plain">
            {d.serverName}
          </RefLink>
        </span>
      ))}
    </code>
  );
}

function StackServers({ group }: { group: AppGroup }) {
  const servers = new Map(group.services.flatMap((s) => s.deployments.map((d) => [d.serverId, d.serverName] as const)));
  const names = [...new Set(servers.values())];
  const [only] = [...servers];
  if (servers.size === 1 && only)
    return (
      <code className="apps-servers apps-clip">
        <RefLink kind="server" id={only[0]} look="plain">
          {only[1]}
        </RefLink>
      </code>
    );
  const count = names.length;
  return <span title={names.join(", ")}>{plural(count, { one: "# server", other: "# servers" })}</span>;
}

/** The list on a phone: stacked rows of a name and a line of state, as M-Apps draws them. */
function PhoneList({
  rows,
  collapsed,
  onToggle,
  health,
  picks,
  actionsFor,
  selection,
}: {
  rows: readonly Row[];
  collapsed: ReadonlySet<string>;
  onToggle: (key: string) => void;
  health: HealthView;
  picks: UpdatePicks;
  actionsFor: (service: ServiceInfo) => MenuAction[];
  selection: RowSelection;
}) {
  const { t } = useLingui();
  const { servers, fleet } = useFleetView();
  const words = useAppWords();
  const healthWords = useHealthWords();
  const runtimes = useRuntimes().data;
  const databases = useDatabaseIds();
  const now = useNow(UPDATES_TICK_MS);
  const context = useRowMenu<Row>();
  const phoneOrder = rows.filter((r) => r.kind !== "stack").flatMap((r) => picksOf(r, picks));
  return (
    <Panel flush className={cx("apps-phone", selection.any && "has-selection")}>
      {rows.map((row) => {
        if (row.kind === "stack") {
          const state = groupState(row.group, servers);
          const runs = rowHealth(row, health);
          const count = row.group.services.length;
          const expanded = !collapsed.has(row.group.key);
          const stack = (
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
                <span className="apps-phone-name">
                  {row.group.name} <NameLabels app={row.group.app} />
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
                  <span>{plural(count, { one: "# app", other: "# apps" })}</span>
                  <StackUpdates services={row.group.services} now={now} />
                </span>
              </span>
              <Icon name={expanded ? "chevron-down" : "chevron-right"} size={16} className="apps-phone-chevron" />
            </button>
          );
          // A catalog install's update is the install's, so its box is its row's, beside it.
          if (!row.group.services.some(ofInstall) || picksOf(row, picks).length === 0) return stack;
          return (
            <div key={row.id} className="apps-phone-pickrow">
              <RowPick row={row} picks={picks} order={phoneOrder} />
              {stack}
            </div>
          );
        }
        const service = row.service as ServiceInfo;
        const state = appState(service, servers);
        const runs = health.of(service);
        const runtime = runtimeOf(service);
        const image = runtime ? `${runtimeWords(runtime, runtimes)} · ${runtime.folder}` : imageOf(service);
        const where = service.deployments.map((d) => d.serverName).join(", ");
        const link = (
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
                {row.kind === "app" && row.group.name ? row.group.name : nameOf(service)}{" "}
                <NameLabels app={row.kind === "app" ? row.group.app : ""} database={databases.has(service.id)} />
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
                  {fleet ? where : runtime ? image : shortImage(image)}
                </span>
                {marksItsUpdate(row, service) ? <UpdateMark service={service} now={now} /> : null}
              </span>
            </span>
            <Icon name="chevron-right" size={16} className="apps-phone-chevron" />
          </Link>
        );
        const name = nameOf(service);
        const label = t`Actions for ${name}`;
        return (
          // biome-ignore lint/a11y/noStaticElementInteractions: The row link and menu button provide keyboard access.
          <div
            key={row.id}
            className={cx("apps-phone-menurow list-select-card", selection.has(row.id) && "is-checked")}
            onContextMenu={(event) =>
              cardContextMenu(event, (at, el) => context.open(row, at, el, { label, actions: actionsFor(service) }))
            }
          >
            {selection.enabled ? (
              <SelectBox
                label={t`Select ${name}`}
                checked={selection.has(row.id)}
                onPick={(on, range) => selection.toggle(row.id, on, range)}
              />
            ) : null}
            {picks.can(service.id) && picksOf(row, picks).length > 0 ? (
              <RowPick row={row} picks={picks} order={phoneOrder} />
            ) : null}
            {link}
            <ActionMenu label={label} actions={actionsFor(service)} />
          </div>
        );
      })}
      {context.element}
    </Panel>
  );
}

/** While the list is on its way: the table with its heads, so nothing moves when it lands. */
function AppsSkeleton({ label, fleet, rows = 3 }: { label: string; fleet: boolean; rows?: number }) {
  const phone = useMediaQuery(PHONE_QUERY);
  const wide = useMediaQuery(fleet ? FLEET_DOMAIN_COLUMN_QUERY : DOMAIN_COLUMN_QUERY);
  const heads = useColumnHeads(fleet, wide);
  const columns = useMemo<TableColumn<{ id: string }>[]>(
    () =>
      heads.map((head) => ({
        ...head,
        render: () => null,
        skeleton: head.key === "app" ? "titled" : head.key === "revision" ? "mono" : undefined,
      })),
    [heads],
  );
  if (phone) {
    return (
      <Loading label={label}>
        <Panel flush>
          <SkeletonRows rows={rows} end="pill" />
        </Panel>
      </Loading>
    );
  }
  return <Table className="apps-table" columns={columns} rows={[]} loading loadingRows={rows} loadingLabel={label} />;
}

/** The services Databases lists, by ID, which Apps calls databases too. */
function useDatabaseIds(): ReadonlySet<string> {
  const data = useDatabases().data;
  return useMemo(() => new Set((data?.databases ?? []).map((d) => d.id)), [data]);
}

/**
 * What an app is beside the name the person gave it: the catalog app it was
 * installed as, and a database, called a database as Databases and Projects do (#796).
 */
function NameLabels({ app, database = false }: { app: string; database?: boolean }) {
  const { t } = useLingui();
  if (!app && !database) return null;
  return (
    <span className="apps-name-labels">
      {app ? <span className="apps-name-label">{app}</span> : null}
      {database ? <span className="apps-name-label">{t`Database`}</span> : null}
    </span>
  );
}
