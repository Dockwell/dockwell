// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, createQueryOptions, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  CertificateState,
  type Domain,
  type DomainRoute,
  DomainService,
  type ListDomainsResponse,
  ResourceService,
  type ServiceInfo,
} from "@dockwell/api";
import { useQueries, useQuery as useTanstackQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useRef } from "react";

import { READ_EVERY_MS, READ_TIMEOUT_MS, type StandingRead, useStandingRead } from "../../lib/standing";
import { useFleetView } from "../servers/useFleet";
import { environmentOf, nameOf, tidyMessage } from "./app";

/** How often a domain whose certificate isn't there yet is read again, which is how the API says to follow it. */
const POLL_MS = 5000;

/** How long a server can say nothing of a domain before that's worth saying, and reading again less often. */
export const SILENT_AFTER_MS = 60_000;

/** How often a domain no server has said anything of is read again, once that's been so for a while. */
const SILENT_POLL_MS = 30_000;

/** The most a route's name, a DNS label, can hold. */
const MAX_LABEL = 63;

/** The most ListResources returns at once. */
const RESOURCE_PAGE = 1000;

/** The guide's part on a server behind a tunnel or a proxy. */
export const TUNNEL_GUIDE =
  "https://github.com/Dockwell/dockwell/blob/main/docs/install.md#behind-cloudflare-tunnel-or-another-proxy";

/**
 * What to call a route to a domain, as a route's name is a DNS label and a
 * domain's has dots. One another domain's route has takes a number, as proposing
 * it under that name would move that route instead.
 */
export function routeName(domain: string, existing: readonly Domain[] = []): string {
  const label = domain.toLowerCase().replace(/[^a-z0-9-]+/g, "-");
  // A label starts with a letter, and an address has none: it still needs a name, so
  // what's refused is the address, in the server's words, and not a name nobody typed.
  const lettered = label.replace(/^[^a-z]+/, "") || `route-${label.replace(/^-+/, "")}`;
  const base = lettered.slice(0, MAX_LABEL).replace(/-+$/, "");
  // Whatever holds the name but this domain's own root, proposing it would move.
  const takenByAnother = (name: string) =>
    existing.some((d) => d.routes.some((route) => route.name === name && (d.name !== domain || route.path !== "")));
  if (!takenByAnother(base)) return base;
  for (let i = 2; ; i++) {
    const suffix = `-${i}`;
    const name = `${base.slice(0, MAX_LABEL - suffix.length).replace(/-+$/, "")}${suffix}`;
    if (!takenByAnother(name)) return name;
  }
}

/**
 * The proxies a person wrote, one for each address or range, or cloudflare. The
 * server says what it won't take, as only it has the rules.
 */
export function proxyList(written: string): string[] {
  return written.split(/[\s,]+/).filter((proxy) => proxy !== "");
}

/** Where a domain stands, as far as someone reaching it goes. */
export interface DomainLook {
  state: "serving" | "pending" | "failed" | "stale";
  /** What to say of it, from the server, when it isn't serving. */
  message: string;
  /** It serves with no certificate here, as TLS ends before this server. */
  noCertificate?: boolean;
  /** The server it's waiting to hear from, when there's nothing else to go on. */
  unreported?: { id: string; name: string };
  /** No server runs it any more, so nothing answers for it and nothing will say why. */
  orphaned?: boolean;
  /** It serves, and message says what's going wrong while it does, such as a renewal failing. */
  warning?: boolean;
  /** Its certificate has run out, whatever was said of it before it did: on which server, and when. */
  expired?: { id: string; name: string; at: number };
  /** How many days its certificate has left, the soonest of its servers', from at least one. */
  days?: number;
  /** Its server isn't connected, so nothing here is known to be so any more. */
  stale?: { id: string; name: string };
  /** What that server last said, for one that isn't connected. */
  said?: DomainLook;
}

/** The ways the list of domains sorts them, in the order it offers them. */
export const DOMAIN_GROUPS = ["failing", "waiting", "serving", "tunnel", "no-report", "not-connected"] as const;

export type DomainGroup = (typeof DOMAIN_GROUPS)[number];

/**
 * Which way a domain sorts: what's going wrong, even while it serves, then what's on its
 * way, and what serves. A domain whose server isn't connected, or has said nothing for a
 * while, comes before them all, as nobody can say any of that of it. Behind a tunnel or
 * a proxy is only what something ends TLS in front of: what answers anyone over plain
 * HTTP has nothing in front, and serves.
 */
export function groupOf(domain: Domain, look: DomainLook, silent: boolean): DomainGroup {
  if (look.stale) return "not-connected";
  if (silent && look.unreported) return "no-report";
  if (look.state === "failed" || look.warning || look.orphaned) return "failing";
  if (look.state === "pending") return "waiting";
  return look.noCertificate && !plainForAnyone(domain) ? "tunnel" : "serving";
}

/** Whether a person should look at a domain in a group: it's failing, or nobody can say. */
export function groupNeedsALook(group: DomainGroup): boolean {
  return group === "failing" || group === "not-connected" || group === "no-report";
}

/** How much a group needs someone, to put first what does, then what's moving, then the rest. */
export function groupConcern(group: DomainGroup): number {
  return groupNeedsALook(group) ? 0 : group === "waiting" ? 1 : 2;
}

/** True when a domain's certificate can't come, and a tunnel or a proxy in front may be why. */
export function mayBeBehind(look: DomainLook): boolean {
  return /set tls to off/.test(look.message);
}

/** True when a domain asks for no certificate, as TLS ends before its server: there's none to say it's getting, whatever the server has yet to report. */
export function asksNoCertificate(domain: Domain): boolean {
  return domain.routes.length > 0 && domain.routes.every((route) => route.tls === "off");
}

/** True when a domain answers anyone over plain HTTP, rather than through something that ends TLS in front of it. */
export function plainForAnyone(domain: Domain): boolean {
  return domain.routes.length > 0 && domain.routes.every((route) => route.plainForAnyone);
}

/**
 * Where someone visiting a domain goes, or a route under it. Behind a tunnel or a proxy
 * they come in over HTTPS, as they do to a certificate here. Domain.url isn't that: it
 * says what this server serves, which behind a tunnel is http://.
 */
export function visitorUrl(domain: Domain, route?: DomainRoute): string {
  return `${plainForAnyone(domain) ? "http" : "https"}://${domain.name}${route?.path ?? ""}`;
}

/** What a route reads as: its domain, and the path under it if it has one. */
export function routeWhere(domain: Domain, route: DomainRoute): string {
  return `${domain.name}${route.path}`;
}

/** Whether a message names a domain itself, not a longer name that holds it, as app.example.com holds example.com. */
function namesDomain(text: string, domain: string): boolean {
  const name = domain.toLowerCase();
  for (let at = text.indexOf(name); at >= 0; at = text.indexOf(name, at + 1)) {
    const before = text[at - 1] ?? "";
    const after = text.slice(at + name.length);
    if (!/[a-z0-9.-]/.test(before) && !/^(?:[a-z0-9-]|\.[a-z0-9])/.test(after)) return true;
  }
  return false;
}

/** Which field of the form a refusal is about, by what the server says, or "" for none. */
export function refusedField(
  message: string,
  form: { domain: string; port: string; onServer: boolean },
): "domain" | "port" | "proxies" | "" {
  const text = message.toLowerCase();
  if (/trusted prox|\bproxy\b.*\b(covers|isn't)\b/.test(text)) return "proxies";
  if (
    form.onServer
      ? new RegExp(`\\bport ${form.port}\\b`).test(text) || text.includes("host_port")
      : /\bthe port its service\b|\bport \d+ in a container\b/.test(text)
  )
    return "port";
  if (form.domain !== "" && namesDomain(text, form.domain)) return "domain";
  // Each of the rest names what it's about, so those above are all that's yours. A
  // limit or the panel's domain says "domain" of something else. These two can't name it.
  if (/^(a route needs a domain|the domain )/.test(text)) return "domain";
  return "";
}

/**
 * A server's refusal of a route as a form says it: what it's about, without which
 * server's agent couldn't plan which route, as this is the only one in front of you.
 * The form has no path to choose another of.
 */
export function formRefusal(message: string, typed = ""): string {
  const said = tidyMessage(
    message
      .replace(/^can't plan on [^:]+: /i, "")
      .replace(/^can't plan route [^:]+: /i, "")
      .replace(/, or choose another path\.?$/, ", or choose another domain."),
  );
  // What someone typed leads as they typed it, even with no dot to tell it's a domain.
  return typed !== "" && said.toLowerCase().startsWith(`${typed} `) ? `${typed}${said.slice(typed.length)}` : said;
}

/** What a server says of a domain, naming the panel's own switch rather than a field only the CLI and the API have. */
export function panelWords(message: string, switchName: string): string {
  return message.replace(/set tls to off(?: on its routes)?/g, `turn on "${switchName}" for this domain`);
}

/**
 * Where a domain stands, as far as someone reaching it goes. With a server that isn't
 * connected, nothing is known of that any more, only what it last said. now is on the
 * control plane's clock, where it's known, as a browser's may be days out.
 */
export function lookOf(domain: Domain, now = Date.now()): DomainLook {
  const said = heardFrom(domain, now);
  const away = domain.servers.find((server) => server.stale);
  return away ? { state: "stale", message: "", stale: { id: away.serverId, name: away.serverName }, said } : said;
}

/** The worst of what a domain's servers say, as it needs them all. */
function heardFrom(domain: Domain, now: number): DomainLook {
  for (const server of domain.servers) {
    if (server.ingress?.message) return { state: "failed", message: server.ingress.message };
  }
  for (const server of domain.servers) {
    const certificate = server.status?.certificate;
    if (certificate?.state === CertificateState.FAILED) return { state: "failed", message: certificate.message };
  }
  if (domain.servers.length === 0) return { state: "pending", message: "", orphaned: true };
  // No certificate is asked for behind a tunnel, so there's none to wait for.
  const certificates = domain.servers.map((server) => server.status?.certificate);
  // A server stops saying issued once it runs out, so one that still does said so before
  // then. Nothing it said can make a certificate good past its end.
  const issuedTo = domain.servers.flatMap((server) => {
    const certificate = server.status?.certificate;
    if (certificate?.state !== CertificateState.ISSUED || !certificate.expireTime) return [];
    return [{ id: server.serverId, name: server.serverName, at: Number(certificate.expireTime.seconds) * 1000 }];
  });
  const first = issuedTo.reduce<(typeof issuedTo)[number] | undefined>(
    (soonest, one) => (soonest && soonest.at <= one.at ? soonest : one),
    undefined,
  );
  if (first && first.at <= now) {
    const said = certificates.find((certificate) => certificate?.message)?.message ?? "";
    return { state: "failed", message: said, expired: first };
  }
  const pending = certificates.find(
    (certificate) =>
      certificate && certificate.state !== CertificateState.ISSUED && certificate.state !== CertificateState.NONE,
  );
  const silent = domain.servers.find((s) => !s.status?.certificate);
  if (pending !== undefined || silent) {
    const message = pending?.message ?? "";
    return {
      state: "pending",
      message,
      ...(silent && !message ? { unreported: { id: silent.serverId, name: silent.serverName } } : {}),
    };
  }
  if (certificates.some((certificate) => certificate?.state === CertificateState.NONE)) {
    return { state: "serving", message: "", noCertificate: true };
  }
  // It has a certificate, and may say what's going wrong with getting the next one.
  const said = certificates.find((certificate) => certificate?.message)?.message ?? "";
  return {
    state: "serving",
    message: said,
    ...(said ? { warning: true } : {}),
    ...(first ? { days: Math.max(1, Math.ceil((first.at - now) / 86_400_000)) } : {}),
  };
}

/**
 * How soon to read domains again: not at all once each serves, and every few seconds
 * until then. Those a server has said nothing of, for a while now, are read less often,
 * as that's a server to go and look at rather than something about to change.
 */
function usePace(): (key: string, domains: readonly Domain[]) => number | false {
  const silentSince = useRef(new Map<string, number>());
  return (key, domains) => {
    const looks = domains.map(lookOf).filter((look) => look.state !== "serving" || look.warning);
    if (looks.length === 0 || !looks.every((look) => look.unreported || look.orphaned || look.stale)) {
      silentSince.current.delete(key);
      return looks.length === 0 ? false : POLL_MS;
    }
    const since = silentSince.current.get(key) ?? Date.now();
    silentSince.current.set(key, since);
    return Date.now() - since >= SILENT_AFTER_MS ? SILENT_POLL_MS : POLL_MS;
  };
}

/**
 * Reads again as a server connects or goes, as what's said of a domain depends on that,
 * and one that serves isn't otherwise read again.
 */
function useReadAgainAsServersMove(read: () => void) {
  const { servers } = useFleetView();
  const connections = servers?.map((server) => `${server.id}:${server.connection}`).join();
  const last = useRef(connections);
  const reading = useRef(read);
  reading.current = read;
  useEffect(() => {
    const was = last.current;
    last.current = connections;
    if (was !== undefined && connections !== undefined && was !== connections) reading.current();
  }, [connections]);
}

/**
 * How soon to read domains someone's looking at again: as usePace has it while one is
 * moving, and otherwise on the pace of any reading, however well they're doing, as what
 * stops being read goes on standing whatever became of it.
 */
function useReadPace(): (domains: readonly Domain[] | undefined) => number {
  const pace = usePace();
  return (domains = []) => Math.min(pace("", domains) || READ_EVERY_MS, READ_EVERY_MS);
}

/** What a read can say of how domains are now: where each stands, nothing for a moment, or nothing until it's back. */
export type DomainRead = "read" | "reading" | "failed";

/** What a read of domains comes to, for what's shown: whether it can be believed now. */
export function readOf(read: StandingRead<ListDomainsResponse>): DomainRead {
  return read.data ? "read" : read.reading ? "reading" : "failed";
}

/** The domains an app is reached at, held to the same rule as any reading (ADR 0027). */
export function useAppDomains(service: ServiceInfo): {
  /** As last read, whatever its age, so its rows stay. read says whether where they stand does. */
  domains: readonly Domain[];
  read: DomainRead;
  isPending: boolean;
  failed: boolean;
  retrying: boolean;
  retry: () => void;
} {
  const transport = useTransport();
  const pace = useReadPace();
  const input = { environment: environmentOf(service), service: nameOf(service) };
  const domains = useStandingRead<ListDomainsResponse>({
    queryKey: createConnectQueryKey({
      schema: DomainService.method.listDomains,
      input,
      transport,
      cardinality: "finite",
    }),
    read: (signal) => createClient(DomainService, transport).listDomains(input, { signal, timeoutMs: READ_TIMEOUT_MS }),
    every: (last) => pace(last?.domains),
  });
  useReadAgainAsServersMove(domains.readAgain);
  const read = readOf(domains);
  return {
    domains: domains.last?.domains ?? [],
    read,
    isPending: domains.last === undefined && read === "reading",
    failed: read === "failed",
    retrying: domains.fetching,
    retry: domains.readAgain,
  };
}

/**
 * Every domain, for a list of them, held to the same rule as any reading (ADR 0027), and
 * read again at once as a server connects or goes.
 */
export function useAllDomains(): StandingRead<ListDomainsResponse> {
  const transport = useTransport();
  const pace = useReadPace();
  const domains = useStandingRead<ListDomainsResponse>({
    queryKey: createConnectQueryKey({
      schema: DomainService.method.listDomains,
      input: {},
      transport,
      cardinality: "finite",
    }),
    read: (signal) => createClient(DomainService, transport).listDomains({}, { signal, timeoutMs: READ_TIMEOUT_MS }),
    every: (last) => pace(last?.domains),
  });
  useReadAgainAsServersMove(domains.readAgain);
  return domains;
}

function selectRouteTimes(resources: readonly { id: string; updateTime?: { seconds: bigint } | undefined }[]) {
  return new Map(
    resources.flatMap((route) => (route.updateTime ? [[route.id, Number(route.updateTime.seconds) * 1000]] : [])) as [
      string,
      number,
    ][],
  );
}

/**
 * When each route's latest revision was applied, by its ID, read a page at a time, under
 * ListResources' own key, so a change that applies reads it again. Undefined until it's
 * read, and if it can't be.
 */
export function useRouteTimes(): ReadonlyMap<string, number> | undefined {
  const transport = useTransport();
  return useTanstackQuery({
    queryKey: createConnectQueryKey({
      schema: ResourceService.method.listResources,
      input: { kind: "route" },
      transport,
      cardinality: "finite",
    }),
    queryFn: async ({ signal }) => {
      const client = createClient(ResourceService, transport);
      const all = [];
      let pageToken = "";
      do {
        const page = await client.listResources({ kind: "route", pageSize: RESOURCE_PAGE, pageToken }, { signal });
        all.push(...page.resources);
        pageToken = page.nextPageToken;
      } while (pageToken !== "");
      return all;
    },
    select: selectRouteTimes,
  }).data;
}

/** What a change brought to life in one environment, or sends there: services and routes, by name. */
export interface EnvironmentReach {
  environment: string;
  services: readonly string[];
  routes: readonly string[];
}

/**
 * The domains a change's arrival leads with: those that send to the services it
 * brought to life, and its own routes, as a change can be nothing but a route. A name
 * means something only in its environment, so each is read on its own, and again until
 * what it reaches settles. routes says which of the change's own routes it has read,
 * as a read from before the change has none of them.
 */
export function useDomainsReaching(reaches: readonly EnvironmentReach[]): {
  /** Each route it reaches, with its domain, as each can go to its own path. */
  rows: readonly { domain: Domain; route: DomainRoute }[];
  routes: ReadonlySet<string>;
  isLoading: boolean;
  failed: boolean;
  retrying: boolean;
  retry: () => void;
} {
  const transport = useTransport();
  const pace = usePace();
  const reaches_ = (reach: EnvironmentReach, route: DomainRoute) =>
    reach.services.includes(route.service) || reach.routes.includes(route.name);
  const reachingOf = (reach: EnvironmentReach, read: readonly Domain[]) =>
    read.filter((domain) => domain.routes.some((route) => reaches_(reach, route)));
  const reads = useQueries({
    queries: reaches.map((reach) => ({
      ...createQueryOptions(DomainService.method.listDomains, { environment: reach.environment }, { transport }),
      // What's held for an arrival goes by this, so it isn't left to what was read a moment ago.
      refetchOnMount: "always" as const,
      refetchInterval: (query: { state: { data?: { domains: Domain[] } | undefined } }) =>
        pace(reach.environment, reachingOf(reach, query.state.data?.domains ?? [])),
    })),
  });
  useReadAgainAsServersMove(() => {
    for (const read of reads) void read.refetch();
  });
  const failures = reads.filter((read) => read.isError);
  return {
    rows: reaches.flatMap((reach, i) =>
      reachingOf(reach, reads[i]?.data?.domains ?? []).flatMap((domain) =>
        domain.routes.filter((route) => reaches_(reach, route)).map((route) => ({ domain, route })),
      ),
    ),
    routes: new Set(
      reaches.flatMap((reach, i) =>
        (reads[i]?.data?.domains ?? []).flatMap((domain) =>
          domain.routes.map((route) => route.name).filter((name) => reach.routes.includes(name)),
        ),
      ),
    ),
    isLoading: reads.some((read) => read.isLoading),
    failed: failures.length > 0,
    retrying: failures.some((read) => read.isFetching),
    retry: () => {
      for (const read of failures) void read.refetch();
    },
  };
}

/**
 * The domains apps answer on, by name, for lists that show them beside each app:
 * a function of an environment and its services' names. Names only, so they're
 * read once, not every few seconds as Domains reads where each stands.
 */
export function useDomainNames(): (environment: string, services: readonly ServiceInfo[]) => string[] | undefined {
  const domains = useQuery(DomainService.method.listDomains, {}, { staleTime: Number.POSITIVE_INFINITY }).data?.domains;
  return useMemo(() => {
    // Until they're read, or while they can't be, nobody can say an app has none.
    if (domains === undefined) return () => undefined;
    const by = new Map<string, string[]>();
    for (const domain of domains) {
      for (const route of domain.routes) {
        const key = `${domain.environmentId}/${route.service}`;
        by.set(key, [...(by.get(key) ?? []), domain.name]);
      }
    }
    return (environment: string, services: readonly ServiceInfo[]): string[] => [
      ...new Set(services.flatMap((s) => by.get(`${environment}/${nameOf(s)}`) ?? [])),
    ];
  }, [domains]);
}
