// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { DOMAIN_GROUPS, type DomainGroup } from "../../features/apps/domains";
import { DomainsPage, type DomainsSearch } from "../../features/domains/DomainsPage";

/** Reads the list's filters from its URL, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): DomainsSearch {
  const out: DomainsSearch = {};
  for (const key of ["q", "server", "private"] as const) {
    const value = search[key];
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  // Said outright, as what's left out here stays as the link has it, and a state nothing
  // is in would filter out every domain.
  const state = search["state"] as DomainGroup;
  out.state = DOMAIN_GROUPS.includes(state) ? state : undefined;
  return out;
}

/** Without the filters left unset, for its URL. */
function without(search: DomainsSearch): DomainsSearch {
  return Object.fromEntries(Object.entries(search).filter(([, v]) => v !== undefined)) as DomainsSearch;
}

export const Route = createFileRoute("/_shell/domains")({
  validateSearch: readSearch,
  component: function Domains() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <DomainsPage
        search={search}
        onSearch={(next) => void navigate({ to: "/domains", search: without(next), replace: true })}
      />
    );
  },
});
