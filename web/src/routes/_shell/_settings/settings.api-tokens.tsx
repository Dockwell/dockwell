// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { TokensPage, type TokensSearch } from "../../../features/settings/TokensPage";
import { readListSearch } from "../../../features/shell/list";

/** Your API tokens: where each was last used, revoking one, and making another. */
export const Route = createFileRoute("/_shell/_settings/settings/api-tokens")({
  validateSearch: (search: Record<string, unknown>): TokensSearch => ({
    ...readListSearch(search),
    ...(search["scope"] === "read" || search["scope"] === "write" || search["scope"] === "deploy"
      ? { scope: search["scope"] }
      : {}),
  }),
  component: function Tokens() {
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <TokensPage
        search={search}
        onSearch={(next) =>
          void navigate({ to: "/settings/api-tokens", search: next, replace: true, resetScroll: false })
        }
      />
    );
  },
});
