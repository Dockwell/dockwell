// SPDX-License-Identifier: AGPL-3.0-only

import { DeploymentState, type Server, type ServiceInfo } from "@dockwell/api";
import { Icon, type IconName } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { Link, type LinkProps } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { nameOf } from "../apps/app";
import { useAllDomains } from "../apps/domains";
import { type HealthKind, type HealthView, useHealth } from "../apps/health";
import { useApps } from "../apps/useServices";
import { engineName, useDatabases } from "../databases/useDatabases";
import { kindOf, useWebsites } from "../websites/website";
import { useFleetView } from "./useFleet";
import "./RunsHere.css";

/**
 * What runs on a server, a card for each kind of thing, each a link to its list.
 * Each says how many there are and how they are, in words that change with how many:
 * none invites making one, one names it and opens it, and more count, naming only
 * what needs a look (PRD, scaling the experience, rule 3).
 */
export function RunsHere({ server }: { server: Server }) {
  const { t } = useLingui();
  return (
    <section className="runs" aria-labelledby="runs-title">
      <h2 className="dw-text-heading runs-title" id="runs-title">
        {t`What runs here`}
      </h2>
      <div className="runs-cards">
        <AppsCard server={server} />
        <WebsitesCard server={server} />
        <DatabasesCard server={server} />
        <DomainsCard server={server} />
      </div>
    </section>
  );
}

interface CardProps {
  icon: IconName;
  /** What the card is of, which names it while it reads. */
  kind: string;
  title: ReactNode;
  sub: ReactNode;
  link: LinkProps;
  /** Still reading: the card holds its place without words. */
  busy?: boolean;
}

function Card({ icon, kind, title, sub, link, busy = false }: CardProps) {
  return (
    <Link {...link} className="dw-panel runs-card" aria-busy={busy || undefined}>
      <span className="runs-icon">
        <Icon name={icon} size={18} />
      </span>
      <span className="runs-text">
        <span className="runs-name">
          {busy ? (
            <>
              <span className="dw-sr">{kind}</span>
              <span className="runs-skeleton is-title" />
            </>
          ) : (
            title
          )}
        </span>
        <span className="runs-sub">{busy ? <span className="runs-skeleton is-sub" /> : sub}</span>
      </span>
      <span className="runs-chevron">
        <Icon name="chevron-right" size={16} />
      </span>
    </Link>
  );
}

/** How an app runs on this server, for the words: what its record says first, then its health. */
type AppNote = { kind: "fine"; health: HealthKind } | { kind: "look"; words: string };

function AppsCard({ server }: { server: Server }) {
  const { t } = useLingui();
  // Apps as Apps lists them: a website's own services are on Websites (ADR 0039).
  const services = useApps();
  const health = useHealth();
  const { servers } = useFleetView();
  const fleet = (servers?.length ?? 0) > 1;
  const here = (services.data ?? []).filter((s) => s.deployments.some((d) => d.serverId === server.id));
  // Until health has been read, or failed to be, the card holds its place, so its
  // counts add up to its apps from the first words it says.
  const busy = services.isPending || !health.settled;
  if (services.error && !services.data) {
    return <Card icon="service" kind={t`Apps`} title={t`Apps`} sub={sentence(services.error)} link={{ to: "/apps" }} />;
  }
  if (here.length === 0) {
    return (
      <Card
        icon="service"
        kind={t`Apps`}
        title={t`No apps`}
        sub={t`Deploy one from an image or the catalog`}
        link={{ to: "/apps/new" }}
        busy={busy}
      />
    );
  }
  const notes = here.map((service) => noteOf(service, health, server.id));
  const sub = <AppsLine names={here.map(nameOf)} notes={notes} />;
  const only = here.length === 1 ? here[0] : undefined;
  return (
    <Card
      icon="service"
      kind={t`Apps`}
      title={plural(here.length, { one: "# app", other: "# apps" })}
      sub={sub}
      link={
        only
          ? { to: "/apps/$serviceId", params: { serviceId: only.id } }
          : { to: "/apps", search: fleet ? { server: server.id } : {} }
      }
      busy={busy}
    />
  );
}

/**
 * What an app's deployment on this server or its health there says that someone
 * should look at, or that it's fine. One whose health can't be read is unknown.
 */
function noteOf(service: ServiceInfo, health: HealthView, serverId: string): AppNote {
  switch (service.deployments.find((d) => d.serverId === serverId)?.state) {
    case DeploymentState.PENDING:
      return { kind: "look", words: "deploying" };
    case DeploymentState.FAILED:
      return { kind: "look", words: "failed" };
    default:
  }
  const read = health.of(service);
  if (read.read !== "read") return { kind: "fine", health: "unknown" };
  const kind = read.servers.find((s) => s.serverId === serverId)?.kind ?? read.kind;
  switch (kind) {
    case "unhealthy":
    case "not-running":
    case "paused":
    case "starting":
      return { kind: "look", words: kind };
    default:
      return { kind: "fine", health: kind };
  }
}

/** "5 healthy · worker deploying": how many are fine, then each that isn't, by name. */
function AppsLine({ names, notes }: { names: readonly string[]; notes: readonly AppNote[] }) {
  const { t } = useLingui();
  const fine = (kind: HealthKind) => notes.filter((n) => n.kind === "fine" && n.health === kind).length;
  const healthy = fine("healthy");
  const running = fine("running");
  // What's fine and none of those, as a check that's starting, reads as unknown.
  const unknown = notes.filter((n) => n.kind === "fine").length - healthy - running;
  const looks = notes.flatMap((n, i) => (n.kind === "look" ? [{ name: names[i] ?? "", words: n.words }] : []));
  const parts: string[] = [];
  if (healthy > 0) parts.push(plural(healthy, { one: "# healthy", other: "# healthy" }));
  if (running > 0) parts.push(plural(running, { one: "# running", other: "# running" }));
  if (unknown > 0) parts.push(plural(unknown, { one: "# unknown", other: "# unknown" }));
  if (looks.length > 2) {
    parts.push(plural(looks.length, { one: "# needs a look", other: "# need a look" }));
  } else {
    for (const { name, words } of looks) {
      switch (words) {
        case "deploying":
          parts.push(t`${name} deploying`);
          break;
        case "failed":
          parts.push(t`${name} failed`);
          break;
        case "unhealthy":
          parts.push(t`${name} unhealthy`);
          break;
        case "not-running":
          parts.push(t`${name} not running`);
          break;
        case "paused":
          parts.push(t`${name} paused`);
          break;
        case "starting":
          parts.push(t`${name} starting`);
          break;
      }
    }
  }
  // With one app, its name comes first, as the card opens it.
  const only = names.length === 1 && looks.length === 0 ? names[0] : undefined;
  if (only !== undefined) {
    if (healthy > 0) return <>{t`${only}, healthy`}</>;
    if (running > 0) return <>{t`${only}, running`}</>;
    if (unknown > 0) return <>{t`${only}, unknown`}</>;
    return <>{only}</>;
  }
  return <>{parts.join(" · ")}</>;
}

function DomainsCard({ server }: { server: Server }) {
  const { t } = useLingui();
  const all = useAllDomains();
  const here = (all.data?.domains ?? []).filter((d) => d.servers.some((s) => s.serverId === server.id));
  const busy = all.data === undefined && !all.unread;
  if (all.unread && all.data === undefined) {
    return (
      <Card icon="globe" kind={t`Domains`} title={t`Domains`} sub={t`Can't read them now`} link={{ to: "/domains" }} />
    );
  }
  if (here.length === 0) {
    return (
      <Card
        icon="globe"
        kind={t`Domains`}
        title={t`No domains`}
        sub={t`Add one to reach an app at a name of yours`}
        link={{ to: "/domains" }}
        busy={busy}
      />
    );
  }
  const names = here.map((d) => d.name).sort();
  const shown = names.slice(0, 2).join(", ");
  const more = names.length - 2;
  return (
    <Card
      icon="globe"
      kind={t`Domains`}
      title={plural(here.length, { one: "# domain", other: "# domains" })}
      sub={more > 0 ? plural(more, { one: `${shown} and # more`, other: `${shown} and # more` }) : shown}
      link={{ to: "/domains" }}
      busy={busy}
    />
  );
}

/** "2 static · 1 PHP 8.3": how many websites serve each way, as Websites' Type column words them. */
function WebsitesCard({ server }: { server: Server }) {
  const { t } = useLingui();
  const sites = useWebsites();
  const here = (sites.data ?? []).filter((w) => w.serverId === server.id);
  if (sites.error && !sites.data) {
    return (
      <Card
        icon="browser"
        kind={t`Websites`}
        title={t`Websites`}
        sub={sentence(sites.error)}
        link={{ to: "/websites" }}
      />
    );
  }
  if (here.length === 0) {
    return (
      <Card
        icon="browser"
        kind={t`Websites`}
        title={t`No websites`}
        sub={t`Serve a folder, PHP or an app at a domain`}
        link={{ to: "/websites/new" }}
        busy={sites.isPending}
      />
    );
  }
  // Each way of serving once, in the order the first of each comes, counted.
  const ways = new Map<string, number>();
  for (const site of here) {
    const kind = kindOf(site.spec);
    const php = site.spec?.serves.case === "php" ? site.spec.serves.value.version : "";
    const way = kind === "folder" ? "static" : kind === "php" ? `php ${php}` : kind === "unknown" ? "unknown" : "proxy";
    ways.set(way, (ways.get(way) ?? 0) + 1);
  }
  const words = [...ways].map(([way, n]) => {
    if (way === "static") return plural(n, { one: "# static", other: "# static" });
    if (way === "proxy") return plural(n, { one: "# proxy", other: "# proxies" });
    if (way === "unknown") return plural(n, { one: "# unknown", other: "# unknown" });
    const version = way.slice(4);
    return version
      ? plural(n, { one: `# PHP ${version}`, other: `# PHP ${version}` })
      : plural(n, { one: "# PHP", other: "# PHP" });
  });
  const only = here.length === 1 ? here[0] : undefined;
  return (
    <Card
      icon="browser"
      kind={t`Websites`}
      title={plural(here.length, { one: "# website", other: "# websites" })}
      sub={only ? only.domain || only.name : words.join(" · ")}
      link={only ? { to: "/websites/$websiteId", params: { websiteId: only.id } } : { to: "/websites" }}
    />
  );
}

/** "PostgreSQL 17.2 · Valkey 8.1": each database here by its engine and the version it says it is here. */
function DatabasesCard({ server }: { server: Server }) {
  const { t } = useLingui();
  const all = useDatabases();
  const here = (all.data?.databases ?? []).filter((db) => db.readings.some((r) => r.serverId === server.id));
  if (all.error && !all.data) {
    return (
      <Card
        icon="database"
        kind={t`Databases`}
        title={t`Databases`}
        sub={sentence(all.error)}
        link={{ to: "/databases" }}
      />
    );
  }
  if (here.length === 0) {
    return (
      <Card
        icon="database"
        kind={t`Databases`}
        title={t`No databases`}
        sub={t`Run PostgreSQL, MySQL, Redis and more`}
        link={{ to: "/databases" }}
        busy={all.isPending}
      />
    );
  }
  const names = here.map((db) => {
    const version = db.readings.find((r) => r.serverId === server.id)?.version || db.version;
    return `${engineName(db.engine)} ${version}`.trim();
  });
  // Two fit the card's line; past that, the first and how many more, so it stays one or two lines.
  const shown = names.length > 2 ? (names[0] ?? "") : names.join(" · ");
  const more = names.length > 2 ? names.length - 1 : 0;
  const only = here.length === 1 ? here[0] : undefined;
  return (
    <Card
      icon="database"
      kind={t`Databases`}
      title={plural(here.length, { one: "# database", other: "# databases" })}
      sub={more > 0 ? plural(more, { one: `${shown} and # more`, other: `${shown} and # more` }) : shown}
      link={only ? { to: "/databases/$serviceId", params: { serviceId: only.id } } : { to: "/databases" }}
    />
  );
}
