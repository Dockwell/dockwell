// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type Domain, type DomainRoute, type ServiceInfo } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Loading,
  Panel,
  PHONE_QUERY,
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
import { type ReactNode, useCallback, useId, useLayoutEffect, useMemo, useRef, useState } from "react";

import { isRoleRefusal, sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { useHeldRows } from "../../lib/loading";
import type { Unread } from "../../lib/online";
import { useTimeWords } from "../../lib/time";
import { AddDomainSheet, type AppChoice, moveBehindOf } from "../apps/AddDomainSheet";
import { useLaterTrouble } from "../apps/AppsPage";
import { environmentOf, keepOrder, nameOf } from "../apps/app";
import { DomainNote, useDomainState, useRouteTarget, useSilence } from "../apps/DomainNote";
import {
  DOMAIN_GROUPS,
  type DomainGroup,
  type DomainLook,
  type DomainRead,
  groupConcern,
  groupNeedsALook,
  groupOf,
  lookOf,
  mayBeBehind,
  readOf,
  routeWhere,
  useAllDomains,
  visitorUrl,
} from "../apps/domains";
import { type OpenWay, openWaysOf, PrivateButton, PrivateFact, PrivateSheet } from "../apps/PrivateSheet";
import { usePlaces, useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { EmptyState } from "../shell/EmptyState";
import {
  ListPager,
  type ListPlace,
  ListSearchField,
  ListSortField,
  SortHeader,
  useListPlace,
  useRowMenu,
} from "../shell/ListTools";
import { type ListSearch, pagedRows, sortedRows } from "../shell/list";
import { Page } from "../shell/Page";
import { ReadFailed } from "../shell/ReadFailed";
import { useWebsiteOfEnvironment } from "../websites/website";
// The add sheet's form and the list's filters are the apps' own, and this page can be where you land.
import "../apps/Apps.css";
import "./Domains.css";
import { phoneWordClass } from "../apps/Health";
import { useInstallAdmin } from "../apps/publish";
import { useServicesWatch } from "../apps/ServicesLive";
import { useDnsProviders } from "../dns/dns";
import { CloudflarePanels } from "./CloudflarePanel";
import { DnsReadsContext, useDnsOf, useDnsReads, useDnsReadsHere, useDnsWord } from "./dns";
import { ManageNameContext, PublishedCell, PublishedNote, ZonesContext } from "./Published";

/** The list's place, in its URL: its filters, its sort and its page. */
export interface DomainsSearch extends ListSearch {
  state?: DomainGroup;
  server?: string;
  /** The route whose sheet of who gets in is open, by ID (ADR 0087). */
  private?: string;
  /** Add a domain opens with the page, as the top bar's New offers it (#617). */
  add?: boolean;
}

/** The columns the list sorts by when asked, as its address names them. */
const SORT_KEYS = ["domain"] as const;

/** How often how long a server has said nothing, and what a certificate has left, are looked at again. */
const NOW_EVERY_MS = 5_000;

/** A domain, and where it stands: one answer for its rows, the filters, the counts and the order. */
interface Standing {
  key: string;
  domain: Domain;
  look: DomainLook;
  silent: boolean;
  group: DomainGroup;
}

/** One row of the list: a route, as each can go to its own app, as an app's page shows them. */
interface Row {
  id: string;
  domain: Domain;
  route: DomainRoute;
  look: DomainLook;
  silent: boolean;
  /** The app it reaches, by its environment, as a name means something only in one. */
  service: ServiceInfo | undefined;
  /** The website it is, by its ID, when what it reaches is a website's own (ADR 0039). */
  website: string | undefined;
  /** The first of its domain here, which says once what's wrong with all of them. */
  first: boolean;
  /** The ways into its app around it, when it keeps strangers out. */
  open: readonly OpenWay[];
  /** Its domain's other half, www or the bare domain, folded in as it only redirects here (ADR 0070). */
  folded: readonly Standing[];
  /** The domains that only redirect here, its other half too, for the Redirects column (#820). */
  sources: readonly string[];
}

/** A domain's other half: www's name for a bare domain, or the bare domain for www. */
function companionName(name: string): string {
  return name.startsWith("www.") ? name.slice(4) : `www.${name}`;
}

/** The domain a domain only redirects to, whole, when that's its other half, or empty. */
function foldsInto(domain: Domain): string {
  if (domain.routes.length === 0) return "";
  const target = companionName(domain.name);
  const into = domain.routes.every((route) => {
    if (route.path !== "" || !route.redirect) return false;
    try {
      return new URL(route.redirect.to).hostname === target;
    } catch {
      return false;
    }
  });
  return into ? target : "";
}

/** The domain a domain only redirects to, whatever it is, or empty: a website's list makes such ones. */
function redirectsTo(domain: Domain): string {
  const hosts = new Set<string>();
  for (const route of domain.routes) {
    if (!route.redirect) return "";
    try {
      hosts.add(new URL(route.redirect.to).hostname);
    } catch {
      return "";
    }
  }
  return hosts.size === 1 ? ([...hosts][0] ?? "") : "";
}

/** An app's key among the routes that reach it. */
const appKey = (environment: string, name: string) => `${environment}/${name}`;

/**
 * Every domain across your apps and servers (Domains-List, Domains-Fleet): what each
 * reaches, and where it stands, in the words an app's page has for it. A domain's state
 * is what its servers say, read all the time and held to ADR 0027: past how long a
 * reading stands the rows read Unknown, with why said once above them, and aren't
 * counted among what needs a look. What needs a look comes first, and after that each
 * keeps its place, so nothing moves under the pointer, as the apps list does. What's in
 * the mocks that nothing here can read yet, traffic and DNS records, isn't drawn.
 */
export function DomainsPage({
  search = {},
  onSearch,
}: {
  search?: DomainsSearch;
  onSearch?: (next: DomainsSearch) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const domains = useAllDomains();
  const services = useServices();
  const websiteOf = useWebsiteOfEnvironment();
  const { reconnecting } = useServicesWatch();
  const { servers, fleet } = useFleetView();
  const places = usePlaces();
  const navigate = useNavigate();
  const may = useMay();
  const mayAdd = may(ChangeService.method.proposeChange);
  const sheet = useChangeSheet();
  const [adding, setAddingHere] = useState(search.add === true);
  // Closing the sheet takes its word out of the address, so a reload doesn't open it again.
  const setAdding = (open: boolean) => {
    setAddingHere(open);
    if (!open && search.add) onSearch?.({ ...search, add: undefined });
  };
  const [moving, setMoving] = useState<Row>();
  // A name made in Cloudflare that an admin takes on, or moves, from its row.
  const [managing, setManaging] = useState<{ service: ServiceInfo; name: string }>();
  const admin = useInstallAdmin();
  // Who gets in to a route, in the list's address so a link opens it, or here where the list has none.
  const [privateHere, setPrivateHere] = useState<string>();
  const privateId = search.private ?? privateHere;
  const choosePrivate = (id: string | undefined) =>
    onSearch ? onSearch({ ...search, private: id }) : setPrivateHere(id);
  const place = useListPlace(search, onSearch, SORT_KEYS);

  const read = readOf(domains);
  // DNS no server has read yet is read as the page opens, and any row's on Check now (#791).
  const dnsReads = useDnsReads(read === "read", (env) => places.place(env)?.scope ?? "");
  // The names of what's there hold, as an app's do, past how long how it is can be believed.
  const all = domains.last?.domains;
  useHeldRows("domains", all?.length);
  // The connected providers: their zones say which zone's rule publishes a name, and
  // each has its panel beside the list (#820).
  const providers = useDnsProviders((all?.length ?? 0) > 0).data?.providers ?? [];
  const zones = useMemo(() => providers.flatMap((p) => p.zones), [providers]);
  // A column for where each name stands at Cloudflare, once any is published there.
  const atCloudflare = (all ?? []).some((d) => d.published.length > 0);
  const sayWhere = places.showsEnvironment || places.severalProjects;
  const apps = useMemo(() => {
    // A website's own services get their domain from Websites.
    const out = (services.data ?? []).filter((s) => !websiteOf?.(environmentOf(s)));
    const several = new Set(out.map(environmentOf)).size > 1;
    return out
      .map((service): AppChoice => {
        const name = nameOf(service);
        return { service, label: several ? `${name} (${places.label(environmentOf(service))})` : name };
      })
      .sort((a, b) => a.label.localeCompare(b.label));
  }, [services.data, places.label, websiteOf]);
  const now = useControlPlaneNow(NOW_EVERY_MS);
  const silentOf = useSilence(now);
  const standings = (all ?? []).map((domain): Standing => {
    const look = lookOf(domain, now);
    const silent = silentOf(domain, look);
    return { key: domain.name, domain, look, silent, group: groupOf(domain, look, silent) };
  });
  // What needs a look comes first, in the API's order among themselves. Once ordered by a
  // read that can be believed, each keeps its place, so nothing moves as something goes
  // wrong, which the lead offers to bring up. Until then, what was last read is all
  // there is to go by. Each time someone asks to see what needs a look first, it starts over.
  const [epoch, setEpoch] = useState(0);
  const order = useRef<{ epoch: number; keys: string[] }>({ epoch: -1, keys: [] });
  const sorted = [...standings].sort((a, b) => groupConcern(a.group) - groupConcern(b.group));
  let ordered = sorted;
  if (order.current.epoch === epoch || read === "read") {
    const kept = keepOrder(order.current.epoch === epoch ? order.current.keys : [], sorted);
    order.current = { epoch, keys: kept.order };
    ordered = kept.groups;
  }
  // Something that needs a look below something that doesn't, as it went wrong since the list was ordered.
  const outOfPlace =
    read === "read" &&
    ordered.some((one, i) => groupNeedsALook(one.group) && ordered.slice(0, i).some((o) => !groupNeedsALook(o.group)));
  const byName = new Map((services.data ?? []).map((s) => [appKey(environmentOf(s), nameOf(s)), s]));
  const wanted = place.query;
  const rows: Row[] = [];
  // Where nobody can say how it stands, nobody can say it's in the state you asked for.
  // The moment a read back takes is no reason to move the list.
  const shown = ordered.filter(
    ({ domain, group }) =>
      !(read !== "failed" && search.state && group !== search.state) &&
      !(search.server && !domain.servers.some((s) => s.serverId === search.server)),
  );
  // www that only redirects to its bare domain, or the other way round, folds into that
  // domain's row, as the mock has it, where both are listed in the same environment:
  // one of another keeps its own row, which says where it is and what it offers. So
  // does one that needs a look, such as a certificate that failed or a server that
  // can't serve it, as its row says why with the way to fix it.
  const folds = new Map<string, Standing[]>();
  for (const one of shown) {
    if (groupNeedsALook(one.group)) continue;
    const into = foldsInto(one.domain);
    const host = shown.find((o) => o.domain.name === into && o.domain.environmentId === one.domain.environmentId);
    if (into && host && !foldsInto(host.domain)) folds.set(into, [...(folds.get(into) ?? []), one]);
  }
  const foldedAway = new Set([...folds.values()].flat().map((one) => one.domain.name));
  // What redirects to each domain, from every domain, as the Redirects column says it.
  const sourcesOf = new Map<string, string[]>();
  for (const domain of all ?? []) {
    const to = redirectsTo(domain);
    if (to && to !== domain.name) sourcesOf.set(to, [...(sourcesOf.get(to) ?? []), domain.name]);
  }
  for (const { domain, look, silent } of shown) {
    if (foldedAway.has(domain.name)) continue;
    const folded = folds.get(domain.name) ?? [];
    let first = true;
    for (const route of domain.routes) {
      const said = [
        routeWhere(domain, route),
        route.service,
        ...domain.servers.map((s) => s.serverName),
        places.label(domain.environmentId),
        ...folded.map((one) => one.domain.name),
      ];
      if (!said.join(" ").toLowerCase().includes(wanted)) continue;
      const service = byName.get(appKey(domain.environmentId, route.service));
      const website = websiteOf?.(domain.environmentId)?.id;
      rows.push({
        id: route.id,
        domain,
        route,
        look,
        silent,
        service,
        website,
        first,
        open: openWaysOf(domain, route, service, all ?? []),
        folded: first && route.path === "" ? folded : [],
        sources: first && route.path === "" ? (sourcesOf.get(domain.name) ?? []) : [],
      });
      first = false;
    }
  }

  // Sorted by name where its head was asked to, over every row the filters keep;
  // what needs a look first is the list's order until then. A page of them shows.
  const sortState = place.sort;
  const listed = sortedRows(rows, sortState, (r) => routeWhere(r.domain, r.route));
  const listPage = pagedRows(listed, place.page);

  const privateAt = privateId
    ? (all ?? [])
        .flatMap((domain) => domain.routes.map((route) => ({ domain, route })))
        .find((at) => at.route.id === privateId)
    : undefined;
  const onPrivate = (row: Row) => choosePrivate(row.route.id);

  const open = (row: Row) => {
    if (row.website) void navigate({ to: "/websites/$websiteId", params: { websiteId: row.website } });
    else if (row.service) void navigate({ to: "/apps/$serviceId", params: { serviceId: row.service.id } });
  };
  const actionsFor = (row: Row): MenuAction[] => {
    const address = routeWhere(row.domain, row.route);
    const service = row.service;
    const scope = row.domain.environmentId;
    return [
      ...(service || row.website
        ? [{ id: "open", icon: "link" as const, label: t`Open`, onSelect: () => open(row) }]
        : []),
      ...(read === "read" && row.look.state === "serving"
        ? [
            {
              id: "visit",
              icon: "external" as const,
              label: t`Open ${address}`,
              href: visitorUrl(row.domain, row.route),
            },
          ]
        : []),
      ...(service && !row.website && scope
        ? [
            {
              id: "remove",
              icon: "trash" as const,
              label: t`Remove`,
              danger: true,
              ...may(ChangeService.method.proposeChange, { environment: scope }).off,
              onSelect: () =>
                sheet.open({
                  kind: "propose",
                  summary: t`Remove ${address} from ${nameOf(service)}`,
                  edits: [{ resource: { kind: "route", scope, name: row.route.name }, delete: true }],
                }),
            },
          ]
        : []),
    ];
  };
  const clear = () => {
    place.clearQuery();
    onSearch?.({});
  };

  const pending = all === undefined && read === "reading";
  const count = all?.length ?? 0;
  const needing = read === "read" ? standings.filter((one) => groupNeedsALook(one.group)).length : 0;
  const total = plural(count, { one: "# domain", other: "# domains" });
  const trouble = needing > 0 ? plural(needing, { one: "# needs a look", other: "# need a look" }) : "";
  const later = useLaterTrouble(needing, trouble, read === "read");
  const states: Record<DomainGroup, string> = {
    failing: t`Failing`,
    waiting: t`Waiting`,
    serving: t`Serving`,
    tunnel: t`Behind a tunnel or a proxy`,
    "no-report": t`No report`,
    "not-connected": t`Not connected`,
  };
  // A list sorted by name is in the reader's order, not the list's, so nothing is out of place in it.
  const showFirst =
    outOfPlace && !sortState ? (
      <Button variant="ghost" size="sm" className="apps-show-first" onClick={() => setEpoch((n) => n + 1)}>
        <Trans>Show them first</Trans>
      </Button>
    ) : null;

  const add = (size?: "lg") => (
    <Button
      variant="primary"
      icon="plus"
      className="apps-new"
      onClick={() => setAdding(true)}
      {...(size ? { size } : {})}
    >
      <Trans>Add a domain</Trans>
    </Button>
  );
  const canAdd = apps.length > 0 && count > 0;
  // Couldn't read isn't none.
  const noApps = services.data?.length === 0;

  let body: ReactNode;
  if (pending) {
    body = <DomainsSkeleton phone={phone} />;
  } else if (all === undefined && isRoleRefusal(domains.error)) {
    body = <ReadFailed error={domains.error} title={t`Can't load the domains`} />;
  } else if (all === undefined) {
    body = (
      <Callout
        tone="critical"
        title={t`Can't load the domains`}
        actions={
          <Button onClick={domains.readAgain}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {domains.error ? sentence(domains.error) : domains.unread ? <UnreadWords {...domains.unread} /> : null}{" "}
        <Trans>What your domains serve isn't affected.</Trans>
      </Callout>
    );
  } else if (count === 0) {
    body = (
      <EmptyState
        icon="link"
        title={<Trans>No domains yet</Trans>}
        action={
          apps.length > 0 ? (
            add("lg")
          ) : noApps ? (
            <RouterButtonLink to="/apps/new" variant="primary" icon="plus" size="lg">
              <Trans>New app</Trans>
            </RouterButtonLink>
          ) : undefined
        }
      >
        <p>
          {noApps ? (
            <Trans>A domain sends people to an app, so run one first.</Trans>
          ) : (
            <Trans>Send a domain to one of your apps. You review a plan before anything changes.</Trans>
          )}
        </p>
      </EmptyState>
    );
  } else if (rows.length === 0) {
    body = (
      <EmptyState
        icon="search"
        title={<Trans>Nothing matches</Trans>}
        action={
          <Button size="lg" onClick={clear}>
            <Trans>Clear the filters</Trans>
          </Button>
        }
      >
        <p>
          <Trans>No domain here matches what you've chosen. Try part of a name, or another state.</Trans>
        </p>
      </EmptyState>
    );
  } else if (phone) {
    body = (
      <>
        <PhoneRows
          rows={listPage.rows}
          read={read}
          fleet={fleet}
          environment={sayWhere ? places.label : undefined}
          onMoveBehind={setMoving}
          onPrivate={onPrivate}
          actionsFor={actionsFor}
        />
        <ListPager page={listPage} onPage={place.setPage} />
      </>
    );
  } else {
    body = (
      <>
        <DomainsTable
          rows={listPage.rows}
          read={read}
          fleet={fleet}
          environment={sayWhere ? places.label : undefined}
          onMoveBehind={setMoving}
          onPrivate={onPrivate}
          onOpen={open}
          place={place}
          actionsFor={actionsFor}
        />
        <ListPager page={listPage} onPage={place.setPage} />
      </>
    );
  }

  return (
    <DnsReadsContext.Provider value={dnsReads}>
      <ZonesContext.Provider value={zones}>
        <ManageNameContext.Provider
          value={
            admin
              ? (domain: Domain) => {
                  const route = domain.routes.find((r) => r.path === "") ?? domain.routes[0];
                  const service = route && byName.get(appKey(domain.environmentId, route.service));
                  if (service) setManaging({ service, name: domain.name });
                }
              : undefined
          }
        >
          <Page
            title={t`Domains`}
            back="/more"
            // The line is held while they load, so the list doesn't move when it lands; with none, nothing will,
            // and the empty state sits under the title as Websites' does.
            lead={
              pending ? (
                <span aria-hidden="true">&nbsp;</span>
              ) : count === 0 ? undefined : (
                <>
                  {[total, trouble].filter(Boolean).join(" · ")}
                  {showFirst ? <> {showFirst}</> : null}
                </>
              )
            }
            status={reconnecting ? <Reconnecting /> : undefined}
            className="apps-page domains-page"
            actions={canAdd ? add() : undefined}
          >
            <section className={cx("apps-section", count > 0 && "has-filters")} aria-label={t`Domains`}>
              {count > 0 ? (
                <div className="apps-filters">
                  <ListSearchField
                    className="apps-filter"
                    size={phone ? "lg" : "sm"}
                    placeholder={t`Filter domains`}
                    label={t`Filter domains`}
                    value={place.typed}
                    onChange={place.setTyped}
                  />
                  {phone ? <ListSortField place={place} columns={{ domain: t`Domain` }} /> : null}
                  <SelectField
                    aria-label={t`State`}
                    size="sm"
                    className="domains-state-filter"
                    value={search.state ?? ""}
                    // Nothing can be sorted by how it stands while nobody can say.
                    disabled={read === "failed"}
                    options={[
                      { value: "", label: t`All states` },
                      ...DOMAIN_GROUPS.map((group) => ({ value: group, label: states[group] })),
                    ]}
                    onChange={(event) =>
                      onSearch?.({
                        ...search,
                        state: (event.target.value as DomainGroup) || undefined,
                        page: undefined,
                      })
                    }
                  />
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
              {phone && canAdd ? (
                <>
                  <div className="apps-phone-head">
                    <h2 className="dw-text-heading apps-phone-count">{total}</h2>
                    {add()}
                  </div>
                  {/* The line is held, so the list stays put as things go wrong, and as the way to bring them up arrives. */}
                  <p className="domains-phone-trouble">
                    {trouble ? (
                      <span className="apps-phone-trouble">{trouble}</span>
                    ) : (
                      <span aria-hidden="true">&nbsp;</span>
                    )}
                    {showFirst}
                  </p>
                </>
              ) : null}
              {/* What goes wrong later moves nothing on the screen, so someone not looking at it has to be told. */}
              <p className="dw-sr" aria-live="polite">
                {later && outOfPlace ? t`${later}. Show them first puts them at the top of the list.` : later}
              </p>
              {domains.unread && all !== undefined ? (
                <Callout tone="info" shape="dashed-ring" className="apps-unread">
                  <UnreadWords {...domains.unread} />
                </Callout>
              ) : null}
              {services.error ? (
                <Callout
                  tone="warning"
                  title={t`Can't read your apps`}
                  actions={
                    <Button onClick={() => void services.refetch()} loading={services.isFetching}>
                      <Trans>Try again</Trans>
                    </Button>
                  }
                >
                  <Trans>Until then, a domain doesn't lead to its app, and you can't add one here.</Trans>
                </Callout>
              ) : null}
              {/* Every domain's, whatever the filters show, so it stays put while someone types. */}
              {phone || count === 0 || read !== "read" ? (
                body
              ) : (
                // With the At Cloudflare column the list takes the width, and the panels go under it, side by side.
                <div className={cx("domains-layout", atCloudflare && "is-wide")}>
                  <div className="domains-main">{body}</div>
                  <div className="domains-side">
                    <CloudflarePanels providers={providers} />
                    <Certificates standings={standings} />
                  </div>
                </div>
              )}
            </section>
            {/* ?add opens it only for someone who may plan a change (ADR 0059). */}
            {adding && mayAdd.allowed ? <AddDomainSheet apps={apps} onClose={() => setAdding(false)} /> : null}
            {privateAt ? (
              <PrivateSheet
                domain={privateAt.domain}
                route={privateAt.route}
                onClose={() => choosePrivate(undefined)}
                service={byName.get(appKey(privateAt.domain.environmentId, privateAt.route.service))}
                all={all ?? []}
                onOther={choosePrivate}
              />
            ) : null}
            {moving?.service ? (
              <AddDomainSheet
                service={moving.service}
                start={moveBehindOf(moving.domain, moving.route)}
                onClose={() => setMoving(undefined)}
              />
            ) : null}
            {managing ? (
              <AddDomainSheet service={managing.service} named={managing.name} onClose={() => setManaging(undefined)} />
            ) : null}
          </Page>
        </ManageNameContext.Provider>
      </ZonesContext.Provider>
    </DnsReadsContext.Provider>
  );
}

/** Where a redirect sends, as a row says it: its address without the scheme, such as example.com/blog. */
function redirectWhere(to: string): string {
  return to.replace(/^https?:\/\//i, "").replace(/\/$/, "");
}

/** What a route reaches, as a row says it: where it redirects, or the app or the port on the server it sends to (ADR 0070). */
function useTargetWords(): (route: DomainRoute) => string {
  const { t } = useLingui();
  const target = useRouteTarget();
  return (route) => {
    if (!route.redirect) return target(route);
    const where = redirectWhere(route.redirect.to);
    return t`Redirects to ${where}`;
  };
}

/** Why nothing here says where domains stand, said once above them rather than on each row. */
function UnreadWords({ why, heard }: { why: Unread; heard: Date | undefined }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const now = useAgoNow(heard);
  const ago = heard ? time.ago(heard, now) : "";
  if (why === "offline") {
    return ago
      ? t`This browser is offline, so your domains read Unknown. Last heard ${ago}.`
      : t`This browser is offline, so your domains read Unknown.`;
  }
  return ago
    ? t`Can't reach the control plane, so your domains read Unknown. Last heard ${ago}; it keeps trying.`
    : t`Can't reach the control plane, so your domains read Unknown. It keeps trying.`;
}

/**
 * What's wrong with a domain, as an app's page says it, in one line under the row, with
 * More for the rest when it runs longer, and the way to fix it beside it. Nothing while
 * what was said can't be believed.
 */
function RowNote({ row, read, onMoveBehind }: { row: Row; read: DomainRead; onMoveBehind: (row: Row) => void }) {
  const { t } = useLingui();
  const { domain, look, service, first, silent } = row;
  const text = useRef<HTMLSpanElement>(null);
  const textId = useId();
  const [long, setLong] = useState(false);
  // A note that ends in its own link, such as Check its page, isn't cut short, as its
  // way to put it right would be out of sight and still in the tab order.
  const [linked, setLinked] = useState(false);
  const [open, setOpen] = useState(false);
  const shown = read === "read" && first;
  const measure = useCallback(() => {
    const el = text.current;
    if (!el) return;
    setLinked(el.querySelector("a, button") !== null);
    setLong(el.scrollHeight > el.clientHeight + 1);
  }, []);
  // Measured as each render lays it out, as the words change with the list, and again
  // as the row's width changes, which no render follows.
  useLayoutEffect(() => {
    if (shown) measure();
  });
  useLayoutEffect(() => {
    const el = text.current;
    if (!shown || !el || typeof ResizeObserver === "undefined") return;
    const seen = new ResizeObserver(measure);
    seen.observe(el);
    return () => seen.disconnect();
  }, [shown, measure]);
  if (!shown) return null;
  const name = domain.name;
  return (
    <div
      className={cx(
        "app-domain-note",
        "domains-note",
        look.state === "failed" && "is-failed",
        look.warning && "is-warning",
      )}
    >
      <span ref={text} id={textId} className={cx("domains-note-text", !open && !linked && "is-one-line")}>
        <DomainNote domain={domain} look={look} silent={silent} />
      </span>
      {(long && !linked) || open ? (
        <button
          type="button"
          className="app-panel-link"
          aria-expanded={open}
          aria-controls={textId}
          aria-label={open ? t`Less about ${name}` : t`More about ${name}`}
          onClick={() => setOpen(!open)}
        >
          {open ? <Trans context="show less of the rest">Less</Trans> : <Trans context="show the rest">More</Trans>}
        </button>
      ) : null}
      {/* The server's words name a switch on the form, so the way to it is here too. */}
      {service && mayBeBehind(look) ? (
        <button type="button" className="app-panel-link" onClick={() => onMoveBehind(row)}>
          <Trans>It's behind a tunnel or a proxy</Trans>
        </button>
      ) : null}
    </div>
  );
}

/** A way to visit a domain, once it serves, where a visitor would go. */
function Visit({ row, read }: { row: Row; read: DomainRead }) {
  const { t } = useLingui();
  if (read !== "read" || row.look.state !== "serving") return null;
  const where = routeWhere(row.domain, row.route);
  return (
    <a
      className="app-reach-open domains-visit"
      href={visitorUrl(row.domain, row.route)}
      target="_blank"
      rel="noreferrer"
      aria-label={t`Open ${where}`}
    >
      <Icon name="external" size={16} />
    </a>
  );
}

/** The way to who gets in to a row's route, off with why where your role can't plan there. */
function Private({ row, onPrivate }: { row: Row; onPrivate: (row: Row) => void }) {
  const may = useMay()(ChangeService.method.proposeChange, { environment: row.domain.environmentId });
  // A redirect has nothing behind it to protect, so it lets everyone through (ADR 0070).
  if (row.route?.redirect) return null;
  return <PrivateButton domain={row.domain} route={row.route} onOpen={() => onPrivate(row)} off={may.off} />;
}

/** Where a row's domain is, once there's a choice: its servers, and its environment. */
function Where({
  row,
  fleet,
  environment,
}: {
  row: Row;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
}) {
  const held = fleet ? row.domain.servers.map((s) => s.serverName).join(", ") : "";
  const place = environment?.(row.domain.environmentId) ?? "";
  return (
    <>
      {held ? <code>{held}</code> : null}
      {held && place ? " · " : null}
      {place}
    </>
  );
}

/** What a route reaches, and where: the app, and its servers and environment where there's a choice of them. */
function Reaches({
  row,
  fleet,
  environment,
}: {
  row: Row;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
}) {
  const target = useTargetWords()(row.route);
  const says = fleet || environment?.(row.domain.environmentId);
  const redirects = row.route.redirect !== undefined;
  return (
    <span className="domains-reaches">
      {/* What it reaches, as Domains-List marks it: an app, or a port on the server itself, by the route, whether or not the app has loaded. */}
      <Icon
        name={redirects ? "arrow-right" : row.route.service ? "service" : "server"}
        size={16}
        className={cx("domains-reaches-mark", redirects ? "is-redirect" : row.route.service ? "is-app" : "is-server")}
      />
      <span className="dw-cell">
        {row.website ? (
          // The row takes the keyboard; the link is for a new tab, and for screen readers' lists of links.
          <Link
            to="/websites/$websiteId"
            params={{ websiteId: row.website }}
            className="dw-cell-title apps-name domains-target"
            tabIndex={-1}
          >
            {target}
          </Link>
        ) : row.service ? (
          <Link
            to="/apps/$serviceId"
            params={{ serviceId: row.service.id }}
            className="dw-cell-title apps-name domains-target"
            tabIndex={-1}
          >
            {target}
          </Link>
        ) : (
          <span className="dw-cell-title domains-target">{target}</span>
        )}
        {says ? (
          <span className="dw-cell-sub domains-where">
            <Where row={row} fleet={fleet} environment={environment} />
          </span>
        ) : null}
      </span>
    </span>
  );
}

function RowBadge({ row, read }: { row: Row; read: DomainRead }) {
  const [status, word] = useDomainState(row.domain, row.look, row.silent, read);
  return <StatusBadge status={status} label={word} labelIsState className="apps-status" title={word} />;
}

/** What DNS says of a domain, as Domains-List's column has it: whether it leads here, and the addresses it found. */
function DnsCell({ domain, read }: { domain: Domain; read: DomainRead }) {
  const { t } = useLingui();
  const reads = useDnsReadsHere();
  const checking = reads?.checking(domain) ?? false;
  const word = useDnsWord()(useDnsOf()(domain));
  // What it last said may no longer be so, and the status says once that nobody can tell.
  if (read !== "read") return <span aria-hidden="true">–</span>;
  return (
    <span className="dw-cell">
      {/* Its longest word fits the column by a pixel or two, so it wraps where a font or a language runs wider. */}
      <StatusBadge
        status={checking ? "deploying" : word.status}
        label={checking ? t`Reading DNS` : word.word}
        labelIsState
        className="domains-dns-word"
      />
      {word.found ? (
        <code className="dw-cell-sub domains-dns-found" title={word.found}>
          {word.found}
        </code>
      ) : null}
      {reads?.again && !checking ? (
        <button
          type="button"
          className="dw-cell-sub domains-check-now"
          aria-label={t`Check DNS for ${domain.name} now`}
          onClick={(event) => {
            // The row opens what it routes to; this reads DNS alone.
            event.stopPropagation();
            reads.again?.(domain);
          }}
        >
          <Trans>Check now</Trans>
        </button>
      ) : null}
    </span>
  );
}

/**
 * Where the certificates stand, beside the list, as Domains-List has it: how many
 * serve, how many are on their way or going wrong, and which runs out first. Each
 * domain counts once, so the numbers add up to the page's count.
 */
function Certificates({ standings }: { standings: readonly Standing[] }) {
  const { t } = useLingui();
  const looks = standings.map((one) => one.look);
  const count = (pick: (look: DomainLook) => boolean) => looks.filter(pick).length;
  const valid = count((l) => l.state === "serving" && !l.noCertificate && !l.warning);
  const renewing = count((l) => l.state === "serving" && Boolean(l.warning));
  // A domain no server runs any more waits for nothing: it's among what fails, as the list sorts it.
  const issuing = count((l) => l.state === "pending" && !l.orphaned);
  const failed = count((l) => l.state === "failed" || (l.state === "pending" && Boolean(l.orphaned)));
  const inFront = count((l) => l.state === "serving" && Boolean(l.noCertificate));
  const away = count((l) => l.state === "stale");
  const soonest = looks.reduce<number>((first, look, i) => {
    if (look.state !== "serving" || look.days === undefined) return first;
    return first >= 0 && (looks[first]?.days ?? 0) <= look.days ? first : i;
  }, -1);
  if (looks.length === 0) return null;
  const facts: [string, ReactNode][] = [];
  if (soonest >= 0) {
    const name = standings[soonest]?.domain.name ?? "";
    const days = looks[soonest]?.days ?? 0;
    facts.push([
      t`Runs out first`,
      <span key="soonest">
        <code>{name}</code> {plural(days, { one: "in # day", other: "in # days" })}
      </span>,
    ]);
  }
  facts.push([t`Valid`, String(valid)]);
  if (renewing > 0) facts.push([t`Renewal failing`, String(renewing)]);
  if (issuing > 0) facts.push([t`Issuing`, String(issuing)]);
  if (failed > 0) facts.push([t`Failed`, String(failed)]);
  if (inFront > 0) facts.push([t`HTTPS ends in front`, String(inFront)]);
  if (away > 0) facts.push([t`Not connected`, String(away)]);
  return (
    <Panel
      className="domains-certificates"
      title={t`Certificates`}
      actions={
        // Only a certificate here is renewed: one that ends in front has none to renew (#791).
        valid + renewing + issuing > 0 ? (
          <span className="domains-certificates-meta">{t`Renewed automatically`}</span>
        ) : undefined
      }
    >
      <dl className="domains-facts">
        {facts.map(([term, value], i) => (
          // The name that runs out first can be long, so it has the panel's whole width under its term.
          <div key={term} className={soonest >= 0 && i === 0 ? "is-wide" : undefined}>
            <dt>{term}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </Panel>
  );
}

/** The list on its way: the table's own columns under its head, or a phone's rows. */
function DomainsSkeleton({ phone }: { phone: boolean }) {
  const { t } = useLingui();
  const rows = useHeldRows("domains", undefined);
  const columns = useMemo<TableColumn<{ id: string }>[]>(
    () => [
      { key: "domain", header: t`Domain`, render: () => null, skeleton: "mono" },
      { key: "reaches", header: t`Routes to`, width: "22%", render: () => null },
      { key: "dns", header: t`DNS`, width: 184, render: () => null, skeleton: "pill" },
      { key: "status", header: t`Status`, width: 168, render: () => null },
    ],
    [t],
  );
  if (phone) {
    return (
      <Loading label={t`Loading domains`}>
        <Panel flush>
          <SkeletonRows rows={rows} end="pill" />
        </Panel>
      </Loading>
    );
  }
  return (
    <Table
      className="apps-table domains-table"
      columns={columns}
      rows={[]}
      caption={t`Domains`}
      loading
      loadingRows={rows}
      loadingLabel={t`Loading domains`}
    />
  );
}

function DomainsTable({
  rows,
  read,
  fleet,
  environment,
  onMoveBehind,
  onPrivate,
  onOpen,
  place,
  actionsFor,
}: {
  rows: readonly Row[];
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
  onPrivate: (row: Row) => void;
  onOpen: (row: Row) => void;
  place: ListPlace;
  actionsFor: (row: Row) => MenuAction[];
}) {
  const { t } = useLingui();
  const menu = useRowMenu<Row>();
  const redirects = rows.some((row) => row.sources.length > 0);
  const atCloudflare = rows.some((row) => row.domain.published.length > 0);
  const columns: TableColumn<Row>[] = [
    {
      key: "domain",
      header: <SortHeader label={t`Domain`} sortKey="domain" place={place} />,
      sort: place.sortOf("domain"),
      render: (row) => (
        <div className="dw-cell">
          <span className="domains-name">
            <code className="domains-at">{routeWhere(row.domain, row.route)}</code>
            <Visit row={row} read={read} />
            <Private row={row} onPrivate={onPrivate} />
          </span>
          <PrivateFact domain={row.domain} route={row.route} open={row.open} />
          <Unserved row={row} />
          <RowNote row={row} read={read} onMoveBehind={onMoveBehind} />
        </div>
      ),
    },
    {
      key: "reaches",
      header: t`Routes to`,
      // Narrower than it was alone, as the DNS column and the panel beside the list take room the name needs,
      // and narrower again beside Cloudflare's columns (#820).
      width: atCloudflare ? "17%" : "22%",
      render: (row) => <Reaches row={row} fleet={fleet} environment={environment} />,
    },
    {
      key: "dns",
      header: t`DNS`,
      width: atCloudflare ? 140 : 184,
      render: (row) => <DnsCell domain={row.domain} read={read} />,
    },
    {
      key: "status",
      header: t`Status`,
      width: 168,
      render: (row) => <RowBadge row={row} read={read} />,
    },
    // Each only where a row has something for it, so a list with none draws no empty column (#820).
    ...(redirects
      ? [{ key: "redirects", header: t`Redirects`, width: 128, render: (row: Row) => <Sources row={row} /> }]
      : []),
    ...(atCloudflare
      ? [
          {
            key: "cloudflare",
            header: t`At Cloudflare`,
            width: 248,
            render: (row: Row) => (row.first ? <PublishedCell domain={row.domain} /> : null),
          },
        ]
      : []),
    {
      key: "actions",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 48,
      align: "right",
      render: (row: Row) => (
        <ActionMenu label={t`Actions for ${routeWhere(row.domain, row.route)}`} actions={actionsFor(row)} />
      ),
    },
  ];
  return (
    <>
      <Table<Row>
        className="apps-table domains-table"
        columns={columns}
        rows={rows}
        caption={t`Domains`}
        rowClickable={(row) => row.service !== undefined || row.website !== undefined}
        onRowClick={(row) => {
          if (row.service || row.website) onOpen(row);
        }}
        rowHasContextMenu={(row) => actionsFor(row).length > 0}
        onRowContextMenu={(row, at, el) =>
          menu.open(row, at, el, {
            label: t`Actions for ${routeWhere(row.domain, row.route)}`,
            actions: actionsFor(row),
          })
        }
      />
      {menu.element}
    </>
  );
}

/** The list on a phone: stacked rows of a name and a line of state, as M-Apps draws them. */
function PhoneRows({
  rows,
  read,
  fleet,
  environment,
  onMoveBehind,
  onPrivate,
  actionsFor,
}: {
  rows: readonly Row[];
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
  onPrivate: (row: Row) => void;
  actionsFor: (row: Row) => MenuAction[];
}) {
  return (
    <Panel flush className="apps-phone domains-phone">
      {rows.map((row) => (
        <PhoneRow
          key={row.id}
          row={row}
          read={read}
          fleet={fleet}
          environment={environment}
          onMoveBehind={onMoveBehind}
          onPrivate={onPrivate}
          actionsFor={actionsFor}
        />
      ))}
    </Panel>
  );
}

function PhoneRow({
  row,
  read,
  fleet,
  environment,
  onMoveBehind,
  onPrivate,
  actionsFor,
}: {
  row: Row;
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
  onPrivate: (row: Row) => void;
  actionsFor: (row: Row) => MenuAction[];
}) {
  const { t } = useLingui();
  const [status, word] = useDomainState(row.domain, row.look, row.silent, read);
  const target = useTargetWords()(row.route);
  // Where its DNS leads, as the wide list's column says it, once a server has read it and the list can be believed.
  const check = useDnsOf()(row.domain);
  const dns = useDnsWord()(check);
  const { tone, shape } = STATUS[status];
  const says = fleet || environment?.(row.domain.environmentId);
  const body = (
    <>
      <span className={cx("apps-phone-glyph", `dw-tone-${tone}`)}>
        <Glyph shape={shape} size={10} />
      </span>
      <span className="apps-phone-text">
        <code className="apps-phone-name domains-at">{routeWhere(row.domain, row.route)}</code>
        <span className={`${phoneWordClass(tone)} domains-phone-word`}>{word}</span>
        {/* Lines of their own, as what it reaches is half of what a row is for, and a clipped name says neither. */}
        <span className="domains-phone-target">{target}</span>
        {check && read === "read" && row.first ? (
          <span className="domains-phone-dns">
            {dns.word}
            {dns.found ? (
              <>
                {" · "}
                <code>{dns.found}</code>
              </>
            ) : null}
          </span>
        ) : null}
        {says ? (
          <span className="domains-where">
            <Where row={row} fleet={fleet} environment={environment} />
          </span>
        ) : null}
      </span>
      {row.service || row.website ? <Icon name="chevron-right" size={16} className="apps-phone-chevron" /> : <span />}
    </>
  );
  // The note can hold a link of its own, so it's beside the row's rather than in it.
  return (
    <div>
      <div className="domains-phone-row">
        {row.website ? (
          <Link to="/websites/$websiteId" params={{ websiteId: row.website }} className="apps-phone-row">
            {body}
          </Link>
        ) : row.service ? (
          <Link to="/apps/$serviceId" params={{ serviceId: row.service.id }} className="apps-phone-row">
            {body}
          </Link>
        ) : (
          <div className="apps-phone-row is-inert">{body}</div>
        )}
        <Visit row={row} read={read} />
        <Private row={row} onPrivate={onPrivate} />
        <ActionMenu label={t`Actions for ${routeWhere(row.domain, row.route)}`} actions={actionsFor(row)} />
      </div>
      <PrivateFact domain={row.domain} route={row.route} open={row.open} />
      <Folded row={row} />
      <Unserved row={row} />
      <RowNote row={row} read={read} onMoveBehind={onMoveBehind} />
      {row.first ? <PublishedNote domain={row.domain} /> : null}
    </div>
  );
}

/**
 * What only redirects to a domain, in its Redirects cell: "from www", its other half
 * by the part before it, the rest whole (#820).
 */
function Sources({ row }: { row: Row }) {
  if (row.sources.length === 0) return null;
  const here = row.domain.name;
  return (
    <span className="domains-sources">
      <Trans>from</Trans>{" "}
      {row.sources.map((name, i) => (
        <span key={name}>
          {i > 0 ? ", " : null}
          <code>{name.endsWith(`.${here}`) ? name.slice(0, -here.length - 1) : name}</code>
        </span>
      ))}
    </span>
  );
}

/**
 * The other half folded into a domain's row: "Redirects: www → example.com", and what's
 * wrong with it, as its own row, where that would show, is folded away (ADR 0070).
 */
function Folded({ row }: { row: Row }) {
  if (row.folded.length === 0) return null;
  return (
    <>
      {row.folded.map((one) => {
        const from = one.domain.name.endsWith(`.${row.domain.name}`)
          ? one.domain.name.slice(0, -row.domain.name.length - 1)
          : one.domain.name;
        const into = row.domain.name;
        return (
          <span key={one.domain.name} className="domains-folded">
            <Icon name="arrow-right" size={12} />
            <span>
              <Trans>
                Redirects: <code>{from}</code> → {into}
              </Trans>
            </span>
          </span>
        );
      })}
    </>
  );
}

/**
 * A redirect on a server whose agent is too old to serve one, as after a downgrade:
 * where it isn't served, and the way to the server to update its agent (ADR 0070).
 */
function Unserved({ row }: { row: Row }) {
  if (!row.route.redirect || !row.first) return null;
  return (
    <>
      {row.domain.servers
        .filter((s) => s.redirectUnserved)
        .map((s) => {
          const server = s.serverName;
          return (
            <span key={s.serverId} className="domains-folded domains-folded-trouble">
              <span>
                <Trans>Not served on {server}</Trans>
              </span>
              {" · "}
              <Link to="/servers/$serverId" params={{ serverId: s.serverId }}>
                <Trans>Update its agent</Trans>
              </Link>
            </span>
          );
        })}
    </>
  );
}
