// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { TerminalPage, type TerminalSearch } from "../../features/terminal/TerminalPage";

/** A container's name or ID, as Docker takes one. */
const containerName = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$/;
/** A logical database name inside a container, matching the backend check. */
const databaseName = /^[A-Za-z0-9_.][A-Za-z0-9_.-]{0,127}$/;

/** Reads a container target or an existing terminal ID from the URL. */
function readSearch(search: Record<string, unknown>): TerminalSearch {
  const isConsole = search["console"] === true || search["console"] === "true" || search["console"] === "1";
  const db =
    isConsole && typeof search["database"] === "string" && databaseName.test(search["database"])
      ? search["database"]
      : undefined;
  const consoleFields = isConsole ? { console: true, ...(db ? { database: db } : {}) } : {};
  const exact = search["containerId"];
  const title = search["containerTitle"];
  if (typeof exact === "string" && /^[0-9a-f]{64}$/.test(exact)) {
    return {
      containerId: exact,
      ...(typeof title === "string" && containerName.test(title) ? { containerTitle: title } : {}),
      ...consoleFields,
    };
  }
  const container = search["container"];
  if (typeof container === "string" && containerName.test(container)) return { container, ...consoleFields };
  const terminalId = search["terminalId"];
  return typeof terminalId === "string" && /^trm_[0-9A-Z]{26}$/.test(terminalId) ? { terminalId } : {};
}

/** A server's terminal: root's shell, or a shell in one of its containers, through its agent. */
export const Route = createFileRoute("/_shell/servers/$serverId/terminal")({
  validateSearch: readSearch,
  component: function Terminal() {
    const { serverId } = Route.useParams();
    return <TerminalPage serverId={serverId} search={Route.useSearch()} />;
  },
});
