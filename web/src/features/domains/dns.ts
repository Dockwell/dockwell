// SPDX-License-Identifier: AGPL-3.0-only

import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import { type Domain, type DomainCheck, DomainCheckState, DomainService } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useQueries } from "@tanstack/react-query";
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

/** How many domains the page reads DNS for as it opens, inside CheckDomain's 60 a minute. */
const READ_ON_OPEN = 40;

/** What the page read of a domain's DNS itself, and the way to read it again. */
export interface DnsReads {
  /** What it read, or undefined where it read nothing. */
  of: (domain: Domain) => DomainCheck | undefined;
  /** Whether it's reading it now. */
  checking: (domain: Domain) => boolean;
  /** Reads it again, or for the first time, now. Undefined for someone who can't check a name. */
  again?: (domain: Domain) => void;
}

/**
 * DNS read from the page (#791): as it opens, for each domain no server has read yet,
 * and again for any on Check now, from the control plane as a plan's check reads it.
 */
export function useDnsReads(domains: readonly Domain[], on: boolean): DnsReads {
  const transport = useTransport();
  const allowed = useMay()(DomainService.method.checkDomain).allowed;
  const [asked, setAsked] = useState<ReadonlySet<string>>(new Set());
  const unread = domains.filter((d) => !dnsOf(d) && d.servers.length > 0).slice(0, READ_ON_OPEN);
  const wanted = domains.filter((d) => d.servers.length > 0 && (asked.has(d.name) || unread.includes(d)));
  const reads = useQueries({
    queries: wanted.map((domain) => ({
      ...createQueryOptions(
        DomainService.method.checkDomain,
        { serverId: domain.servers[0]?.serverId ?? "", domain: domain.name },
        { transport },
      ),
      enabled: on && allowed,
      retry: false,
      staleTime: 30_000,
    })),
  });
  const at = (domain: Domain) => wanted.findIndex((d) => d.name === domain.name);
  return {
    of: (domain) => reads[at(domain)]?.data?.check,
    checking: (domain) => reads[at(domain)]?.isFetching ?? false,
    again: allowed
      ? (domain) => {
          const i = at(domain);
          if (i >= 0) void reads[i]?.refetch();
          else setAsked((was) => new Set([...was, domain.name]));
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
