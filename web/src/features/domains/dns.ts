// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { type Domain, type DomainCheck, DomainCheckState, DomainService } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { createContext, useContext, useState } from "react";

import { useMay } from "../auth/may";

/**
 * What DNS says of a domain, as its servers last read it: the one that says least
 * well, as a person reaches the domain through every server it's on.
 */
export function dnsOf(domain: Domain): DomainCheck | undefined {
  const checks = domain.servers.flatMap((server) => (server.status?.dns ? [server.status.dns] : []));
  const rank = (check: DomainCheck) =>
    check.state === DomainCheckState.POINTS_HERE || check.proxied
      ? 0
      : check.state === DomainCheckState.CANT_TELL
        ? 1
        : 2;
  return checks.reduce<DomainCheck | undefined>(
    (worst, check) => (worst && rank(worst) >= rank(check) ? worst : check),
    undefined,
  );
}

/** What DNS says of a domain in a table's cell: a state's shape and word, and the addresses it found. */
export interface DnsWord {
  status: Status;
  word: string;
  found: string;
}

/**
 * What a domain's DNS column says, as Domains-List has one: whether its name leads
 * here, through Cloudflare's proxy, somewhere else, or nowhere yet.
 */
export function useDnsWord(): (check: DomainCheck | undefined) => DnsWord {
  const { t } = useLingui();
  return (check) => {
    const found = check?.found.join(", ") ?? "";
    if (!check) return { status: "unknown", word: t`Not read yet`, found };
    // Cloudflare's proxy ends TLS in front: whether a certificate can be had then is the status column's to say.
    if (check.proxied) return { status: "healthy", word: t`Proxied by Cloudflare`, found };
    switch (check.state) {
      case DomainCheckState.POINTS_HERE:
        return { status: "healthy", word: t`Points here`, found };
      case DomainCheckState.CANT_TELL:
        return { status: "unknown", word: t`Can't tell where it points`, found };
      case DomainCheckState.NO_RECORD:
        return { status: "stopped", word: t`No record yet`, found };
      case DomainCheckState.ELSEWHERE:
        return { status: "degraded", word: t`Points elsewhere`, found };
    }
    return { status: "unknown", word: t`Can't read DNS`, found };
  };
}

/** What the page read of a domain's DNS itself, and the way to read it again. */
export interface DnsReads {
  /** What it read, or undefined where it read nothing. */
  of: (domain: Domain) => DomainCheck | undefined;
  /** Whether it's reading it now. */
  checking: (domain: Domain) => boolean;
  /** Reads it again now, in full. Undefined for someone who can't check a name. */
  again?: (domain: Domain) => void;
}

/**
 * DNS read from the page (#791): as it opens, every domain at once, in one list from
 * the control plane, which reads each connected zone once and keeps it a while, so the
 * page spends no Cloudflare check per row and none of the person's live checks; and a
 * row's in full on Check now (the review's B-M2).
 */
export function useDnsReads(on: boolean, projectOf: (environment: string) => string): DnsReads {
  const transport = useTransport();
  const allowed = useMay()(DomainService.method.checkDomain).allowed;
  const listed = useQuery(
    DomainService.method.listDomains,
    { readDns: true },
    { enabled: on, retry: false, staleTime: 30_000 },
  );
  const [checked, setChecked] = useState<ReadonlyMap<string, DomainCheck>>(new Map());
  const [checking, setChecking] = useState<ReadonlySet<string>>(new Set());
  const read = new Map((listed.data?.domains ?? []).flatMap((d) => (d.dns ? [[d.name, d.dns] as const] : [])));
  return {
    of: (domain) => checked.get(domain.name) ?? read.get(domain.name),
    checking: (domain) => checking.has(domain.name) || (listed.isFetching && !read.has(domain.name)),
    again:
      allowed && on
        ? (domain) => {
            const server = domain.servers[0]?.serverId ?? "";
            if (server === "") return;
            setChecking((was) => new Set([...was, domain.name]));
            // In full, past what's kept, as the person asked for it now.
            void createClient(DomainService, transport)
              .checkDomain(
                { serverId: server, domain: domain.name, projectId: projectOf(domain.environmentId) },
                { headers: { "Cache-Control": "no-cache" } },
              )
              .then((answer) => {
                const check = answer.check;
                if (check) setChecked((was) => new Map([...was, [domain.name, check]]));
              })
              .catch(() => undefined)
              .finally(() =>
                setChecking((was) => {
                  const next = new Set(was);
                  next.delete(domain.name);
                  return next;
                }),
              );
          }
        : undefined,
  };
}

/** The page's own DNS reads, for the rows under it. */
export const DnsReadsContext = createContext<DnsReads | undefined>(undefined);

/** What DNS says of a domain: what the page read itself, or else what its servers last read. */
export function useDnsOf(): (domain: Domain) => DomainCheck | undefined {
  const reads = useContext(DnsReadsContext);
  return (domain) => reads?.of(domain) ?? dnsOf(domain);
}

/** The page's own DNS reads. */
export function useDnsReadsHere(): DnsReads | undefined {
  return useContext(DnsReadsContext);
}
