// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { type DnsProviderInfo, DomainService } from "@dockwell/api";

/** Where a Cloudflare API token is made. */
export const CLOUDFLARE_TOKENS = "https://dash.cloudflare.com/profile/api-tokens";

/** The DNS providers an admin connected, by name and zones (ADR 0088). */
export function useDnsProviders(enabled = true) {
  return useQuery(DomainService.method.listDnsProviders, {}, { enabled, staleTime: 30_000 });
}

/** A provider's name, not yet taken: the provider's own, then numbered. */
export function freeProviderName(base: string, taken: readonly DnsProviderInfo[]): string {
  const names = new Set(taken.map((p) => p.name));
  if (!names.has(base)) return base;
  for (let i = 2; ; i++) if (!names.has(`${base}-${i}`)) return `${base}-${i}`;
}
