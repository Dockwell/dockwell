// SPDX-License-Identifier: AGPL-3.0-only

import type { Domain, DomainRoute, ServiceInfo } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
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

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import type { Unread } from "../../lib/online";
import { useTimeWords } from "../../lib/time";
import { AddDomainSheet, type AppChoice, moveBehindOf } from "../apps/AddDomainSheet";
import { ListSkeleton, useLaterTrouble } from "../apps/AppsPage";
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
import { usePlaces, useServices } from "../apps/useServices";
import { Reconnecting } from "../servers/FleetNotes";
import { useAgoNow, useControlPlaneNow, useFleetView } from "../servers/useFleet";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { useWebsiteOfEnvironment } from "../websites/website";
// The add sheet's form and the list's filters are the apps' own, and this page can be where you land.
import "../apps/Apps.css";
import "./Domains.css";
import { phoneWordClass } from "../apps/Health";
import { useServicesWatch } from "../apps/ServicesLive";
import { dnsOf, useDnsWord } from "./dns";

/** The list's place, in its URL: its filters. */
export interface DomainsSearch {
  q?: string;
  state?: DomainGroup;
  server?: string;
}

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
  const [adding, setAdding] = useState(false);
  const [moving, setMoving] = useState<Row>();
  const [typed, setTyped] = useState(search.q ?? "");
  const query = useDeferredValue(typed);

  // The filter goes in the address a moment after you stop typing, so a link
  // to the list keeps it, without a navigation for each key.
  useEffect(() => {
    if (query === (search.q ?? "")) return;
    const timer = setTimeout(() => onSearch?.({ ...search, q: query || undefined }), 300);
    return () => clearTimeout(timer);
  }, [query, search, onSearch]);

  const read = readOf(domains);
  // The names of what's there hold, as an app's do, past how long how it is can be believed.
  const all = domains.last?.domains;
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
  const wanted = query.trim().toLowerCase();
  const rows: Row[] = [];
  for (const { domain, look, silent, group } of ordered) {
    // Where nobody can say how it stands, nobody can say it's in the state you asked for.
    // The moment a read back takes is no reason to move the list.
    if (read !== "failed" && search.state && group !== search.state) continue;
    if (search.server && !domain.servers.some((s) => s.serverId === search.server)) continue;
    let first = true;
    for (const route of domain.routes) {
      const said = [
        routeWhere(domain, route),
        route.service,
        ...domain.servers.map((s) => s.serverName),
        places.label(domain.environmentId),
      ];
      if (!said.join(" ").toLowerCase().includes(wanted)) continue;
      const service = byName.get(appKey(domain.environmentId, route.service));
      const website = websiteOf?.(domain.environmentId)?.id;
      rows.push({ id: route.id, domain, route, look, silent, service, website, first });
      first = false;
    }
  }

  const open = (row: Row) => {
    if (row.website) void navigate({ to: "/websites/$websiteId", params: { websiteId: row.website } });
    else if (row.service) void navigate({ to: "/apps/$serviceId", params: { serviceId: row.service.id } });
  };
  const clear = () => {
    setTyped("");
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
  const showFirst = outOfPlace ? (
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
    body = <ListSkeleton label={t`Loading domains`} />;
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
      <PhoneRows
        rows={rows}
        read={read}
        fleet={fleet}
        environment={sayWhere ? places.label : undefined}
        onMoveBehind={setMoving}
      />
    );
  } else {
    body = (
      <DomainsTable
        rows={rows}
        read={read}
        fleet={fleet}
        environment={sayWhere ? places.label : undefined}
        onMoveBehind={setMoving}
        onOpen={open}
      />
    );
  }

  return (
    <Page
      title={t`Domains`}
      back="/more"
      // The line is held while they load, so the list doesn't move when it lands.
      lead={
        pending || count === 0 ? (
          <span aria-hidden="true">&nbsp;</span>
        ) : (
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
            <TextField
              className="apps-filter"
              size={phone ? "lg" : "sm"}
              icon="search"
              type="search"
              placeholder={t`Filter domains`}
              aria-label={t`Filter domains`}
              value={typed}
              onChange={(event) => setTyped(event.target.value)}
            />
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
              onChange={(event) => onSearch?.({ ...search, state: (event.target.value as DomainGroup) || undefined })}
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
                onChange={(event) => onSearch?.({ ...search, server: event.target.value || undefined })}
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
              {trouble ? <span className="apps-phone-trouble">{trouble}</span> : <span aria-hidden="true">&nbsp;</span>}
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
          <div className="domains-layout">
            <div className="domains-main">{body}</div>
            <Certificates standings={standings} />
          </div>
        )}
      </section>
      {adding ? <AddDomainSheet apps={apps} onClose={() => setAdding(false)} /> : null}
      {moving?.service ? (
        <AddDomainSheet
          service={moving.service}
          start={moveBehindOf(moving.domain, moving.route)}
          onClose={() => setMoving(undefined)}
        />
      ) : null}
    </Page>
  );
}

/** Why nothing here says where domains stand, said once above them rather than on each row. */
function UnreadWords({ why, heard }: { why: Unread; heard: Date | undefined }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const now = useAgoNow(heard);
  const ago = heard ? time.ago(heard, now) : "";
  if (why === "offline") {
    return ago
      ? t`This browser is offline, so the panel can't say where your domains stand, and they read Unknown. It last heard ${ago}. Check your connection.`
      : t`This browser is offline, so the panel can't say where your domains stand, and they read Unknown. Check your connection.`;
  }
  return ago
    ? t`The panel can't reach the control plane, so it can't say where your domains stand, and they read Unknown. It last heard ${ago}, and keeps trying.`
    : t`The panel can't reach the control plane, so it can't say where your domains stand, and they read Unknown. It keeps trying.`;
}

/** What's wrong with a domain, with the fix, as an app's page says it. Nothing while what was said can't be believed. */
function RowNote({ row, read, onMoveBehind }: { row: Row; read: DomainRead; onMoveBehind: (row: Row) => void }) {
  const { domain, look, service, first, silent } = row;
  if (read !== "read" || !first) return null;
  return (
    <p
      className={cx(
        "app-domain-note",
        "domains-note",
        look.state === "failed" && "is-failed",
        look.warning && "is-warning",
      )}
    >
      <DomainNote domain={domain} look={look} silent={silent} />
      {/* The server's words name a switch on the form, so the way to it is here too. */}
      {service && mayBeBehind(look) ? (
        <>
          {" "}
          <button type="button" className="app-panel-link" onClick={() => onMoveBehind(row)}>
            <Trans>It's behind a tunnel or a proxy</Trans>
          </button>
        </>
      ) : null}
    </p>
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
  const target = useRouteTarget()(row.route);
  const says = fleet || environment?.(row.domain.environmentId);
  return (
    <span className="domains-reaches">
      {/* What it reaches, as Domains-List marks it: an app, or a port on the server itself, by the route, whether or not the app has loaded. */}
      <Icon
        name={row.route.service ? "service" : "server"}
        size={16}
        className={cx("domains-reaches-mark", row.route.service ? "is-app" : "is-server")}
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
  const word = useDnsWord()(dnsOf(domain));
  // What it last said may no longer be so, and the status says once that nobody can tell.
  if (read !== "read") return <span aria-hidden="true">–</span>;
  return (
    <span className="dw-cell">
      <StatusBadge status={word.status} label={word.word} labelIsState className="apps-status" title={word.word} />
      {word.found ? (
        <code className="dw-cell-sub domains-dns-found" title={word.found}>
          {word.found}
        </code>
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
  if (inFront > 0) facts.push([t`TLS ends in front`, String(inFront)]);
  if (away > 0) facts.push([t`Not connected`, String(away)]);
  return (
    <Panel
      className="domains-certificates"
      title={t`Certificates`}
      actions={<span className="domains-certificates-meta">{t`Renewed automatically`}</span>}
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

function DomainsTable({
  rows,
  read,
  fleet,
  environment,
  onMoveBehind,
  onOpen,
}: {
  rows: readonly Row[];
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
  onOpen: (row: Row) => void;
}) {
  const { t } = useLingui();
  const columns = useMemo<TableColumn<Row>[]>(
    () => [
      {
        key: "domain",
        header: t`Domain`,
        render: (row) => (
          <div className="dw-cell">
            <span className="domains-name">
              <code className="domains-at">{routeWhere(row.domain, row.route)}</code>
              <Visit row={row} read={read} />
            </span>
            <RowNote row={row} read={read} onMoveBehind={onMoveBehind} />
          </div>
        ),
      },
      {
        key: "reaches",
        header: t`Routes to`,
        // Narrower than it was alone, as the DNS column and the panel beside the list take room the name needs.
        width: "22%",
        render: (row) => <Reaches row={row} fleet={fleet} environment={environment} />,
      },
      { key: "dns", header: t`DNS`, width: 184, render: (row) => <DnsCell domain={row.domain} read={read} /> },
      { key: "status", header: t`Status`, width: 168, render: (row) => <RowBadge row={row} read={read} /> },
    ],
    [t, read, fleet, environment, onMoveBehind],
  );
  return (
    <Table<Row>
      className="apps-table domains-table"
      columns={columns}
      rows={rows}
      caption={t`Domains`}
      rowClickable={(row) => row.service !== undefined || row.website !== undefined}
      onRowClick={(row) => {
        if (row.service || row.website) onOpen(row);
      }}
    />
  );
}

/** The list on a phone: stacked rows of a name and a line of state, as M-Apps draws them. */
function PhoneRows({
  rows,
  read,
  fleet,
  environment,
  onMoveBehind,
}: {
  rows: readonly Row[];
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
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
}: {
  row: Row;
  read: DomainRead;
  fleet: boolean;
  environment: ((id: string) => string) | undefined;
  onMoveBehind: (row: Row) => void;
}) {
  const [status, word] = useDomainState(row.domain, row.look, row.silent, read);
  const target = useRouteTarget()(row.route);
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
      </div>
      <RowNote row={row} read={read} onMoveBehind={onMoveBehind} />
    </div>
  );
}
