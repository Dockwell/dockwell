// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { AppsPage } from "../../features/apps/AppsPage";
import { NewAppSheet, type NewAppSource } from "../../features/apps/NewAppSheet";

interface NewAppSearch {
  source?: "compose";
}

/** New app: its sheet over the list, as Apps-New draws it, at an address of its own. */
export const Route = createFileRoute("/_shell/apps/new")({
  validateSearch: (search: Record<string, unknown>): NewAppSearch =>
    search["source"] === "compose" ? { source: "compose" } : {},
  component: function NewApp() {
    const { source } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <AppsPage>
        <NewAppSheet
          source={source ?? "image"}
          onSource={(next: NewAppSource) =>
            void navigate({ to: "/apps/new", search: next === "compose" ? { source: "compose" } : {}, replace: true })
          }
          onClose={() => void navigate({ to: "/apps" })}
        />
      </AppsPage>
    );
  },
});
