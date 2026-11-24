// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type CloudflareCheck,
  CloudflareName,
  type DnsProviderInfo,
  DomainService,
  RouteWay,
  type Server,
  type WayOption,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { sentence } from "../../lib/errors";

import { useMay } from "../auth/may";
import { useSettled } from "./DomainRecord";

/** A zone a connected DNS provider writes in, and the provider, by name. */
export interface Zone {
  zone: string;
  provider: string;
}

/**
 * The zones a person picks a name from (ADR 0104, section 1): those of the providers that
 * serve the project, or, for an admin making a website in a project none serves yet, every
 * provider's, as the same change adds the project to the provider.
 */
export function useZones(project: string, install = false): { zones: Zone[]; providers: DnsProviderInfo[] } {
  const may = useMay();
  const allowed = may(DomainService.method.listDnsProviders).allowed;
  const listed = useQuery(
    DomainService.method.listDnsProviders,
    { projectId: install ? "" : project },
    { enabled: allowed && (install || project !== ""), retry: false },
  );
  const providers = listed.data?.providers ?? [];
  const zones = providers
    .filter((p) => p.provider === "" || p.provider === "cloudflare")
    .flatMap((p) => p.zones.map((zone) => ({ zone, provider: p.name })));
  zones.sort((a, b) => a.zone.localeCompare(b.zone));
  return { zones: zones.filter((z, i) => zones.findIndex((o) => o.zone === z.zone) === i), providers };
}

/** The most specific connected zone holding a whole name. */
export function zoneOfName(zones: readonly Zone[], name: string): string {
  return zones.reduce(
    (best, { zone }) => (zone.length > best.length && (name === zone || name.endsWith(`.${zone}`)) ? zone : best),
    "",
  );
}

/** The whole name, with the zone itself when the subdomain is empty. */
export function nameIn(sub: string, zone: string): string {
  const name = sub.trim().toLowerCase().replace(/\.+$/, "");
  if (!zone || name === zone || name.endsWith(`.${zone}`)) return name;
  return name ? `${name}.${zone}` : zone;
}

export function subdomainOf(name: string, zone: string): string {
  if (!zone) return name;
  return name === zone ? "" : name.endsWith(`.${zone}`) ? name.slice(0, -(zone.length + 1)) : name;
}

/** A server's public address, as a record would point at it, or "". */
export function publicAddressOf(server: Server | undefined): string {
  return server?.network?.ipv4?.address || server?.network?.ipv6?.address || "";
}

/** Whether the caller is an admin of the install: one who may check a DNS provider's token. */
export function useInstallAdmin(): boolean {
  return useMay()(DomainService.method.checkDnsProvider).allowed;
}

/** What a connected zone and the server's tunnels say of a name, and how to read it again. */
export interface CloudflareRead {
  check: CloudflareCheck | undefined;
  companion: string;
  checking: boolean;
  /** Why it couldn't be read, as a state's words, or "" where it was or is being read. */
  failed: string;
  /** Reads it again now, the server's tunnels too, past the while they're kept. */
  again: () => void;
}

/**
 * What a connected zone and the server's tunnels say of a name, read as typing stops.
 * Where it can't be read, it says why, so a sheet never plans a plain route as if the
 * zone weren't Cloudflare's (the review's B-M1). No project, as for a new website, asks
 * for a way, which an admin's check reads in every zone.
 */
export function useCloudflareCheck(server: string, domain: string, project: string, on: boolean): CloudflareRead {
  const { t } = useLingui();
  const asked = useSettled(domain);
  const transport = useTransport();
  const queryClient = useQueryClient();
  const allowed = useMay()(DomainService.method.checkDomain).allowed;
  const input = { serverId: server, domain: asked, projectId: project, ...(project === "" ? { publish: {} } : {}) };
  const canCheck = on && allowed && server !== "" && domain.includes(".") && !/\s/.test(domain);
  const enabled = canCheck && asked !== "";
  const read = useQuery(DomainService.method.checkDomain, input, { enabled, retry: false });
  const [reading, setReading] = useState(false);
  const fresh = enabled && asked === domain;
  const cf = fresh ? read.data?.cloudflare : undefined;
  let failed = "";
  if (fresh && !read.isFetching && !reading) {
    if (read.error) failed = sentence(read.error);
    else if (read.data && (!cf || (cf.state === CloudflareName.UNSPECIFIED && cf.ways.length === 0))) {
      failed = t`Couldn't read Cloudflare`;
    }
  }
  return {
    check: cf && (cf.state !== CloudflareName.UNSPECIFIED || cf.ways.length > 0) ? cf : undefined,
    companion: fresh ? (read.data?.companion ?? "") : "",
    checking: canCheck && (read.isPending || read.isFetching || reading || asked !== domain),
    failed,
    again: () => {
      if (!enabled) return;
      setReading(true);
      // Sent past what's kept, so a cloudflared started a moment ago is seen now.
      void createClient(DomainService, transport)
        .checkDomain(input, { headers: { "Cache-Control": "no-cache" } })
        .then((answer) =>
          queryClient.setQueryData(
            createConnectQueryKey({
              schema: DomainService.method.checkDomain,
              input,
              transport,
              cardinality: "finite",
            }),
            answer,
          ),
        )
        .catch(() => void read.refetch())
        .finally(() => setReading(false));
    },
  };
}

/** The way a check offers, by its kind and, for a tunnel, which. */
export function wayOf(check: CloudflareCheck | undefined, way: RouteWay, tunnel = ""): WayOption | undefined {
  return check?.ways.find((w) => w.way === way && (tunnel === "" || w.tunnel === tunnel || w.tunnel === ""));
}

/** The way a check picks, with its tunnel: the first one it can take of that kind. */
export function pickedOf(check: CloudflareCheck | undefined): { way: RouteWay; tunnel: string } {
  if (!check) return { way: RouteWay.UNSPECIFIED, tunnel: "" };
  const option =
    check.ways.find((w) => w.way === check.way && w.off === "") ?? check.ways.find((w) => w.way === check.way);
  return { way: check.way, tunnel: option?.tunnel ?? "" };
}

/** A way's route fields, beside what it publishes (ADR 0104, section 2's table). */
export function routeOfWay(
  way: RouteWay,
  sslMode: string,
): { tls?: string; trustedProxies?: string[]; proof?: string; plainForAnyone?: boolean } {
  switch (way) {
    case RouteWay.SERVER_TUNNEL:
    case RouteWay.OWN_TUNNEL:
      // The tunnel ends HTTPS, and Caddy trusts what comes from it.
      return { tls: "off", trustedProxies: ["127.0.0.1"], plainForAnyone: false };
    case RouteWay.PROXY:
      // On Flexible, Cloudflare reaches the server over HTTP, which a certificate would loop.
      return sslMode === "flexible"
        ? { tls: "off", trustedProxies: ["cloudflare"], plainForAnyone: false }
        : { tls: "", trustedProxies: ["cloudflare"], proof: "dns", plainForAnyone: false };
    case RouteWay.OWN_HTTPS:
      return { tls: "", trustedProxies: [], plainForAnyone: false };
    default:
      return {};
  }
}

/** A thing at Cloudflare a person agreed Dockwell may replace or take on, as a route's publish names it. */
export interface TakeOverInit {
  recordId?: string;
  tunnel?: string;
  hostname?: string;
  path?: string;
}

/** What a route publishes, as an edit sends it. */
export interface PublishInit {
  way: RouteWay;
  provider: string;
  tunnel?: string;
  takesOver?: TakeOverInit[];
}

/** What a route publishes for a way, or nothing for one Dockwell writes nothing for. */
export function publishOf(
  way: RouteWay,
  tunnel: string,
  provider: string,
  takesOver: readonly TakeOverInit[] = [],
): PublishInit | undefined {
  if (way === RouteWay.UNSPECIFIED || way === RouteWay.FRONT || provider === "") return undefined;
  return {
    way,
    provider,
    ...(way === RouteWay.SERVER_TUNNEL ? { tunnel } : {}),
    ...(takesOver.length > 0 ? { takesOver: [...takesOver] } : {}),
  };
}

/** A name one label down from a zone, for a name two or more down, as Cloudflare's free certificate covers. */
export function oneLabel(sub: string): string {
  return sub.replaceAll(".", "-");
}

/** The next free-looking name after one that's taken: wiki-2, then wiki-3. */
export function nextName(sub: string): string {
  const m = /^(.*)-(\d+)$/.exec(sub);
  return m ? `${m[1]}-${Number(m[2]) + 1}` : `${sub}-2`;
}
