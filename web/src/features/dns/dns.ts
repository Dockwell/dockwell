// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { type DnsProviderInfo, DomainService, type EditSchema } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import type { Opening } from "../changes/ChangeSheet";

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

/** Carries the whole public spec into a provider edit. The server keeps its tokens. */
export function dnsProviderEdit(
  provider: DnsProviderInfo,
  update: { everyProject?: boolean; projects?: string[]; strictZones?: string[]; putBack?: string[] },
): MessageInitShape<typeof EditSchema> {
  return {
    resource: { kind: "dns.provider", name: provider.name },
    baseRevision: provider.revision,
    spec: {
      kind: {
        case: "dnsProvider",
        value: {
          provider: provider.provider || "cloudflare",
          zones: provider.zones,
          projects: provider.projects,
          everyProject: provider.everyProject,
          strictZones: provider.strictZones,
          ...update,
        },
      },
    },
  };
}

/** Adds a project to the provider's existing reach through a plan. */
export function useProviderAccessPlan() {
  const { t } = useLingui();
  return (provider: DnsProviderInfo, project: string, onClose: Opening["onClose"]): Opening => {
    const name = provider.name;
    return {
      kind: "propose",
      summary: t`Use ${name} in this project`,
      edits: [dnsProviderEdit(provider, { projects: [...new Set([...provider.projects, project])] })],
      onClose,
    };
  };
}
