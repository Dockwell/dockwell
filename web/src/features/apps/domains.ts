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

/** Where a domain stands, as far as someone reaching it goes. */
export interface DomainLook {
  state: "serving" | "pending" | "failed";
  /** What to say of it, from the server, when it isn't serving. */
  message: string;
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
  const pending = domain.servers
    .map((server) => server.status?.certificate)
    .find((certificate) => certificate?.state !== CertificateState.ISSUED);
  if (domain.servers.length === 0 || pending !== undefined || domain.servers.some((s) => !s.status?.certificate)) {
    return { state: "pending", message: pending?.message ?? "" };
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

/** Where an app's domain sends people, once it has one. */
export function useAppAddress(service: ServiceInfo): string | undefined {
  return useAppDomains(service).domains[0]?.url;
}
