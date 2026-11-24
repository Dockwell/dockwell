// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { BackupsPage, type BackupsSearch } from "../../features/backups/BackupsPage";
import { readListSearch } from "../../features/shell/list";

/** Reads where the list is and the state it shows, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): BackupsSearch {
  const state = search["state"];
  return {
    ...readListSearch(search),
    ...(state === "done" || state === "failed" || state === "running" ? { state } : {}),
  };
}

/** Every backup, with the way to take one and to restore one. */
export const Route = createFileRoute("/_shell/backups/")({
  validateSearch: readSearch,
  component: function Backups() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <BackupsPage
        search={search}
        onSearch={(next) => void navigate({ to: "/backups", search: next, replace: true })}
      />
    );
  },
});
