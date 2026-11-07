// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { TerminalPage, type TerminalSearch } from "../../features/terminal/TerminalPage";

/** A container's name or ID, as Docker takes one. */
const containerName = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$/;

/** Reads which container a shell is for, leaving out what can't be one. */
function readSearch(search: Record<string, unknown>): TerminalSearch {
  const container = search["container"];
  return typeof container === "string" && containerName.test(container) ? { container } : {};
}

/** A server's terminal: root's shell, or a shell in one of its containers, through its agent. */
export const Route = createFileRoute("/_shell/servers/$serverId/terminal")({
  validateSearch: readSearch,
  component: function Terminal() {
    const { serverId } = Route.useParams();
    return <TerminalPage serverId={serverId} search={Route.useSearch()} />;
  },
});
