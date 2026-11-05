// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { CertificateState, type Domain, DomainService, type ServiceInfo } from "@dockwell/api";

import { environmentOf, nameOf } from "./app";

/** How often a domain whose certificate isn't there yet is read again, which is how the API says to follow it. */
const POLL_MS = 5000;

/** The most a route's name, a DNS label, can hold. */
const MAX_LABEL = 63;

/**
 * What to call a route to a domain, as a route's name is a DNS label and a
 * domain's has dots. One another domain's route has takes a number, as proposing
 * it under that name would move that route instead.
 */
export function routeName(domain: string, existing: readonly Domain[] = []): string {
  const base = domain
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .slice(0, MAX_LABEL)
    .replace(/-+$/, "");
  const takenByAnother = (name: string) =>
    existing.some((d) => d.name !== domain && d.routes.some((route) => route.name === name));
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
  state: "serving" | "pending" | "failed";
  /** What to say of it, from the server, when it isn't serving. */
  message: string;
  /** It serves with no certificate here, as TLS ends before this server. */
  noCertificate?: boolean;
}

/** True when a domain asks for no certificate, as TLS ends before its server: there's none to say it's getting, whatever the server has yet to report. */
export function asksNoCertificate(domain: Domain): boolean {
  return domain.routes.length > 0 && domain.routes.every((route) => route.tls === "off");
}

/** The worst of what a domain's servers say, as it needs them all. */
export function lookOf(domain: Domain): DomainLook {
  for (const server of domain.servers) {
    if (server.ingress?.message) return { state: "failed", message: server.ingress.message };
  }
  for (const server of domain.servers) {
    const certificate = server.status?.certificate;
    if (certificate?.state === CertificateState.FAILED) return { state: "failed", message: certificate.message };
  }
  // No certificate is asked for behind a tunnel, so there's none to wait for.
  const certificates = domain.servers.map((server) => server.status?.certificate);
  const pending = certificates.find(
    (certificate) => certificate?.state !== CertificateState.ISSUED && certificate?.state !== CertificateState.NONE,
  );
  if (domain.servers.length === 0 || pending !== undefined || domain.servers.some((s) => !s.status?.certificate)) {
    return { state: "pending", message: pending?.message ?? "" };
  }
  if (certificates.some((certificate) => certificate?.state === CertificateState.NONE)) {
    return { state: "serving", message: "", noCertificate: true };
  }
  return { state: "serving", message: "" };
}

/** The domains an app is reached at, read again until each settles: a failed certificate is retried by itself. */
export function useAppDomains(service: ServiceInfo): { domains: readonly Domain[]; isPending: boolean } {
  const query = useQuery(
    DomainService.method.listDomains,
    { environment: environmentOf(service), service: nameOf(service) },
    {
      refetchInterval: (state) =>
        (state.state.data?.domains ?? []).some((domain) => lookOf(domain).state !== "serving") ? POLL_MS : false,
    },
  );
  return { domains: query.data?.domains ?? [], isPending: query.isPending };
}

/**
 * The domains a change's arrival leads with: those that send to the services it
 * brought to life, and its own routes, as a change can be nothing but a route. A
 * change reads as one environment, so this reads that one's, however many services
 * it has, and again until each settles. isLoading is true until it has read them,
 * and until what it read has the change's own routes: a read from before the change
 * can't, and it stands until the one after comes back.
 */
export function useDomainsReaching(
  environment: string,
  services: readonly string[],
  routes: readonly string[],
): { domains: readonly Domain[]; isLoading: boolean } {
  const reaching = (domain: Domain) =>
    domain.routes.some((route) => services.includes(route.service) || routes.includes(route.name));
  const query = useQuery(
    DomainService.method.listDomains,
    { environment },
    {
      enabled: environment !== "" && services.length + routes.length > 0,
      refetchInterval: (state) =>
        (state.state.data?.domains ?? []).some((domain) => reaching(domain) && lookOf(domain).state !== "serving")
          ? POLL_MS
          : false,
    },
  );
  const read = query.data?.domains ?? [];
  const behind = routes.some((name) => !read.some((domain) => domain.routes.some((route) => route.name === name)));
  return { domains: read.filter(reaching), isLoading: query.isLoading || behind };
}
