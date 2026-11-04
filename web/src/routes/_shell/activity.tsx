// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import {
  type ActivityKind,
  ActivityPage,
  type ActivitySearch,
  searchWithout,
} from "../../features/activity/ActivityPage";

const KINDS: readonly ActivityKind[] = ["changes", "servers", "access"];

/** Reads Activity's filters from its URL, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): ActivitySearch {
  const out: ActivitySearch = {};
  const text = (key: string) => {
    const value = search[key];
    return typeof value === "string" && value !== "" ? value : undefined;
  };
  const kind = text("kind");
  if (kind && (KINDS as readonly string[]).includes(kind)) out.kind = kind as ActivityKind;
  for (const key of ["actor", "server", "resource", "event"] as const) {
    const value = text(key);
    if (value) out[key] = value;
  }
  return out;
}

export const Route = createFileRoute("/_shell/activity")({
  validateSearch: readSearch,
  component: function Activity() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ActivityPage
        search={search}
        onSearch={(next) => void navigate({ to: "/activity", search: searchWithout(next), replace: true })}
      />
    );
  },
});
