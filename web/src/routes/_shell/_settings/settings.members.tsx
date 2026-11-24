// SPDX-License-Identifier: AGPL-3.0-only

import { Role } from "@dockwell/api";
import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { MembersPage, type MembersSearch } from "../../../features/settings/MembersPage";
import { readListSearch } from "../../../features/shell/list";

/** Who signs in to the install, their role and how they sign in. */
export const Route = createFileRoute("/_shell/_settings/settings/members")({
  validateSearch: (search: Record<string, unknown>): MembersSearch => {
    const role = search["role"];
    return {
      ...readListSearch(search),
      ...(role === Role.OWNER || role === Role.ADMIN || role === Role.DEVELOPER || role === Role.VIEWER
        ? { role }
        : {}),
    };
  },
  component: function Members() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <MembersPage
        search={search}
        onSearch={(next) => void navigate({ to: "/settings/members", search: next, replace: true, resetScroll: false })}
      />
    );
  },
});
