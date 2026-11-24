// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { type ChannelsSearch, ChannelsTab } from "../../features/alerts/ChannelsTab";
import { readListSearch } from "../../features/shell/list";

/** Where alerts go, with a test for each. */
export const Route = createFileRoute("/_shell/alerts/channels")({
  validateSearch: (search: Record<string, unknown>): ChannelsSearch => ({
    ...readListSearch(search),
    ...(typeof search["edit"] === "string" && search["edit"] !== "" ? { edit: search["edit"] } : {}),
  }),
  component: function ChannelsTabRoute() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <ChannelsTab
        search={search}
        onSearch={(next) => void navigate({ to: "/alerts/channels", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
