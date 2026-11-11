// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { AppsPage } from "../../features/apps/AppsPage";
import { NewAppSheet, type NewAppSource } from "../../features/apps/NewAppSheet";

interface NewAppSearch {
  source?: "git" | "image" | "compose" | "folder";
  /** For a folder: the language and version a runtime's card asked for. */
  language?: string;
  version?: string;
}

/** New app: its sheet over the list, as Apps-New draws it, at an address of its own. */
export const Route = createFileRoute("/_shell/apps/new")({
  validateSearch: (search: Record<string, unknown>): NewAppSearch => {
    if (search["source"] === "compose") return { source: "compose" };
    if (search["source"] === "git") return { source: "git" };
    if (search["source"] === "image") return { source: "image" };
    if (search["source"] !== "folder") return {};
    const language = typeof search["language"] === "string" ? search["language"] : undefined;
    const version = search["version"];
    return {
      source: "folder",
      ...(language ? { language } : {}),
      ...(typeof version === "string" || typeof version === "number" ? { version: String(version) } : {}),
    };
  },
  component: function NewApp() {
    const { source, language, version } = Route.useSearch();
    const navigate = useNavigate();
    return (
      <AppsPage>
        <NewAppSheet
          source={source ?? "image"}
          runtime={language ? { language, version } : undefined}
          onSource={(next: NewAppSource) =>
            void navigate({ to: "/apps/new", search: next === "image" ? {} : { source: next }, replace: true })
          }
          onClose={() => void navigate({ to: "/apps" })}
        />
      </AppsPage>
    );
  },
});
