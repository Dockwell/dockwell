// SPDX-License-Identifier: AGPL-3.0-only

import { type Domain, type DomainCheck, DomainCheckState } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

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
