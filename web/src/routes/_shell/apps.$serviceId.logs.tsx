// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { type LogRange, LogsPage, type LogsSearch } from "../../features/logs/LogsPage";

const RANGES: readonly LogRange[] = ["15m", "1h", "24h"];

/** Reads a view of logs from its URL, leaving out what isn't one. */
function readSearch(search: Record<string, unknown>): LogsSearch {
  const out: LogsSearch = {};
  const text = (key: string) => {
    const value = search[key];
    return typeof value === "string" && value !== "" ? value : undefined;
  };
  const range = text("range");
  if (range && (RANGES as readonly string[]).includes(range)) out.range = range as LogRange;
  for (const key of ["levels", "replica", "q", "change"] as const) {
    const value = text(key);
    if (value) out[key] = value;
  }
  if (search["deep"] === true) out.deep = true;
  const revision = Number(search["revision"]);
  if (Number.isSafeInteger(revision) && revision > 0) out.revision = revision;
  return out;
}

/** An app's logs: following live, a time range, or one deploy's. */
export const Route = createFileRoute("/_shell/apps/$serviceId/logs")({
  validateSearch: readSearch,
  component: function Logs() {
    const { serviceId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <LogsPage
        serviceId={serviceId}
        search={search}
        onSearch={(next) =>
          void navigate({ to: "/apps/$serviceId/logs", params: { serviceId }, search: next, replace: true })
        }
      />
    );
  },
});
