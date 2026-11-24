// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { RulesTab } from "../../features/alerts/ListTabs";
import { readListSearch } from "../../features/shell/list";

/** What Dockwell watches for, and where each rule sends. */
export const Route = createFileRoute("/_shell/alerts/rules")({
  validateSearch: readListSearch,
  component: function RulesTabRoute() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <RulesTab
        search={search}
        onSearch={(next) => void navigate({ to: "/alerts/rules", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
