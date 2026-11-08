// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { TasksPage } from "../../features/tasks/TasksPage";

/** The task a person chose, by ID, which the page shows beside the list. */
function readSearch(search: Record<string, unknown>): { task?: string } {
  const task = search["task"];
  return typeof task === "string" && task !== "" ? { task } : {};
}

/** Scheduled tasks: every task, the next day's runs, and the chosen one's runs and output. */
export const Route = createFileRoute("/_shell/scheduled-tasks")({ component: TasksPage, validateSearch: readSearch });
