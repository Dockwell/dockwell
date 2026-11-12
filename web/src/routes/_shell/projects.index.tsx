// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { ProjectsPage, type ProjectsSearch } from "../../features/projects/ProjectsPage";

/** Reads which projects the list shows, leaving out what isn't a choice. */
function readSearch(search: Record<string, unknown>): ProjectsSearch {
  return search["show"] === "idle" ? { show: "idle" } : {};
}

/** Every project, with what's in each, and Delete beside those with nothing running (ADR 0074). */
export const Route = createFileRoute("/_shell/projects/")({
  validateSearch: readSearch,
  component: function Projects() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ProjectsPage
        search={search}
        onSearch={(next) => void navigate({ to: "/projects", search: next, replace: true })}
      />
    );
  },
});
