// SPDX-License-Identifier: AGPL-3.0-only

import { TaskKind } from "@dockwell/api";
import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { readListSearch } from "../../features/shell/list";
import { TasksPage, type TasksSearch } from "../../features/tasks/TasksPage";

/** The task a person chose, by ID, which the page shows beside the list. */
function readSearch(search: Record<string, unknown>): TasksSearch {
  const task = search["task"];
  const kind = search["kind"];
  return {
    ...readListSearch(search),
    ...(typeof task === "string" && task !== "" ? { task } : {}),
    ...(kind === TaskKind.APP || kind === TaskKind.SHELL || kind === TaskKind.REQUEST ? { kind } : {}),
  };
}

/** Scheduled tasks: every task, the next day's runs, and the chosen one's runs and output. */
export const Route = createFileRoute("/_shell/scheduled-tasks")({
  validateSearch: readSearch,
  component: function Tasks() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <TasksPage
        search={search}
        onSearch={(next) => void navigate({ to: "/scheduled-tasks", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
