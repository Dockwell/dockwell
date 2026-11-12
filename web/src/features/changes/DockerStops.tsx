// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ContainerService, ContainerState, DeploymentState, RestartPolicy, type ServiceInfo } from "@dockwell/api";
import { Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useDatabases } from "../databases/useDatabases";
import { useWebsites } from "../websites/website";

/** One thing that stops while Docker restarts, and whether Docker starts it again. */
export interface Stopping {
  key: string;
  name: string;
  kind: "app" | "database" | "website";
  /** The service's ID, or the website's. */
  id: string;
  /** Whether Docker starts it again once it's back, by its restart policy. */
  back: boolean;
  /** For one that stays stopped, the restart policy that keeps it so. */
  policy: RestartPolicy;
}

/** Whether Docker starts a container with this policy again as its daemon comes back, as Docker's own rules have it. */
export function startsAgain(policy: RestartPolicy): boolean {
  // on-failure restarts only a container that exited with an error, never one the daemon stopped.
  return policy !== RestartPolicy.NO && policy !== RestartPolicy.ON_FAILURE;
}

function policyOf(s: ServiceInfo): RestartPolicy {
  // A service's container takes unless-stopped when its spec says nothing.
  return s.spec?.runs.case === "app" ? s.spec.runs.value.restart : RestartPolicy.UNSPECIFIED;
}

/** Whether a service runs on a server: what's deployed there, or on its way. */
function runsOn(s: ServiceInfo, serverId: string): boolean {
  return s.deployments.some(
    (d) => d.serverId === serverId && (d.state === DeploymentState.LIVE || d.state === DeploymentState.PENDING),
  );
}

/**
 * What runs on a server, of what Dockwell manages: each app and database, and each
 * website as one, with whether Docker starts it again by its restart policy. A
 * website comes back once Caddy, which Dockwell starts unless-stopped, and each of
 * its own services do. Those that stay stopped come first.
 */
export function stoppingOn(
  serverId: string,
  services: readonly ServiceInfo[],
  websites: readonly { id: string; name: string; domain: string; environmentId: string; serverId: string }[],
  databases: ReadonlySet<string>,
): Stopping[] {
  const sites = websites.filter((w) => w.serverId === serverId);
  const siteOf = new Map(sites.map((w) => [w.environmentId, w]));
  const siteBack = new Map(sites.map((w) => [w.id, { back: true, policy: RestartPolicy.UNSPECIFIED }]));
  const out: Stopping[] = [];
  for (const s of services) {
    if (!runsOn(s, serverId)) continue;
    const policy = policyOf(s);
    const back = startsAgain(policy);
    const site = siteOf.get(s.resource?.scope ?? "");
    if (site) {
      if (!back) siteBack.set(site.id, { back, policy });
      continue;
    }
    out.push({
      key: `service:${s.id}`,
      name: s.resource?.name ?? s.id,
      kind: databases.has(s.id) ? "database" : "app",
      id: s.id,
      back,
      policy,
    });
  }
  for (const w of sites) {
    const b = siteBack.get(w.id) ?? { back: true, policy: RestartPolicy.UNSPECIFIED };
    out.push({ key: `website:${w.id}`, name: w.domain || w.name, kind: "website", id: w.id, ...b });
  }
  return out.sort((a, b) => Number(a.back) - Number(b.back) || a.name.localeCompare(b.name));
}

/**
 * What stops on a server while Docker restarts for an install (ADR 0064, "What
 * stops"): each app, database and website Dockwell runs there, linked, with whether
 * Docker starts it again; then how many containers it didn't make stop too, whose
 * own restart policies decide.
 */
export function DockerStops({ serverId, server }: { serverId: string; server: string }) {
  const { t } = useLingui();
  const services = useServices();
  const websites = useWebsites();
  const databases = useDatabases();
  const mayList = useMay()(ContainerService.method.listContainers).allowed;
  const containers = useQuery(ContainerService.method.listContainers, { server: serverId }, { enabled: mayList });
  if (!services.data || !websites.data) {
    return <span className="plan-stops-reading">{t`Reading what runs on ${server}…`}</span>;
  }
  const stopping = stoppingOn(
    serverId,
    services.data,
    websites.data,
    new Set((databases.data?.databases ?? []).map((d) => d.id)),
  );
  const others = (containers.data?.containers ?? []).filter(
    (c) => c.state === ContainerState.RUNNING && !c.oneOff,
  ).length;
  const staying = stopping.filter((s) => !s.back).length;
  if (stopping.length === 0 && others === 0) {
    return <span className="plan-stops-reading">{t`Nothing Dockwell runs is on ${server}.`}</span>;
  }
  const kinds: Record<Stopping["kind"], string> = {
    app: t({ message: "App", context: "what stops while Docker restarts" }),
    database: t({ message: "Database", context: "what stops while Docker restarts" }),
    website: t({ message: "Website", context: "what stops while Docker restarts" }),
  };
  const why = (p: RestartPolicy) =>
    p === RestartPolicy.ON_FAILURE
      ? t`Its restart policy starts it again only after it fails`
      : t`Its restart policy never starts it again`;
  const link = (s: Stopping): ReactNode => {
    const name = <code>{s.name}</code>;
    switch (s.kind) {
      case "website":
        return (
          <Link to="/websites/$websiteId" params={{ websiteId: s.id }}>
            {name}
          </Link>
        );
      case "database":
        return (
          <Link to="/databases/$serviceId" params={{ serviceId: s.id }}>
            {name}
          </Link>
        );
    }
    // One that stays stopped is linked to where its restart policy is set.
    return s.back ? (
      <Link to="/apps/$serviceId" params={{ serviceId: s.id }}>
        {name}
      </Link>
    ) : (
      <Link to="/apps/$serviceId/settings" params={{ serviceId: s.id }}>
        {name}
      </Link>
    );
  };
  return (
    <div className="plan-stops">
      <span className="plan-stops-head">
        {staying > 0
          ? plural(staying, {
              one: `What stops on ${server} until Docker is back · # stays stopped after`,
              other: `What stops on ${server} until Docker is back · # stay stopped after`,
            })
          : t`What stops on ${server} until Docker is back · all of it starts again`}
      </span>
      {stopping.length > 0 ? (
        <ul className="plan-stops-list" aria-label={t`What stops on ${server}`}>
          {stopping.map((s) => (
            <li key={s.key}>
              {link(s)} <Tag words>{kinds[s.kind]}</Tag>{" "}
              {s.back ? (
                <span className="plan-stops-back">{t`Starts again`}</span>
              ) : (
                <span className="plan-stops-stays" title={why(s.policy)}>
                  {t`Stays stopped`}
                </span>
              )}
            </li>
          ))}
        </ul>
      ) : null}
      {others > 0 ? (
        <span className="plan-stops-others">
          {plural(others, {
            one: "# container Dockwell didn't make stops too, and its own restart policy decides.",
            other: "# containers Dockwell didn't make stop too, and their own restart policies decide.",
          })}{" "}
          <Link to="/servers/$serverId" params={{ serverId }}>
            <Trans>See them</Trans>
          </Link>
        </span>
      ) : null}
    </div>
  );
}
