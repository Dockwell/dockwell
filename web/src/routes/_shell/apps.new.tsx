// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";

import { AppsPage } from "../../features/apps/AppsPage";
import { NewAppSheet, type NewAppSource } from "../../features/apps/NewAppSheet";
import { InstallSheet } from "../../features/catalog/InstallSheet";

interface NewAppSearch {
  /** Where the app comes from: the catalog unless it says otherwise (#617). */
  source?: "git" | "image" | "compose" | "folder";
  /** For a folder: the language and version a runtime's card asked for. */
  language?: string;
  version?: string;
  /** From the catalog: the words searched for, and the app picked, by its entry's ID. */
  q?: string;
  entry?: string;
}

const text = (value: unknown): string | undefined => (typeof value === "string" && value !== "" ? value : undefined);

/** New app: its sheet over the list, as Apps-New draws it, at an address of its own. */
export const Route = createFileRoute("/_shell/apps/new")({
  validateSearch: (search: Record<string, unknown>): NewAppSearch => {
    if (search["source"] === "compose") return { source: "compose" };
    if (search["source"] === "git") return { source: "git" };
    if (search["source"] === "image") return { source: "image" };
    if (search["source"] === "folder") {
      const language = text(search["language"]);
      const version = search["version"];
      return {
        source: "folder",
        ...(language ? { language } : {}),
        ...(typeof version === "string" || typeof version === "number" ? { version: String(version) } : {}),
      };
    }
    const q = text(search["q"]);
    const entry = text(search["entry"]);
    return { ...(q ? { q } : {}), ...(entry ? { entry } : {}) };
  },
  component: function NewApp() {
    const { source, language, version, q, entry } = Route.useSearch();
    const navigate = useNavigate();
    const onSource = (next: NewAppSource) =>
      void navigate({ to: "/apps/new", search: next === "catalog" ? {} : { source: next }, replace: true });
    const onClose = () => void navigate({ to: "/apps" });
    return (
      <AppsPage>
        {source ? (
          <NewAppSheet
            source={source}
            runtime={language ? { language, version } : undefined}
            onSource={onSource}
            onClose={onClose}
          />
        ) : entry ? (
          // The app picked, as the Catalog page installs it, in the same sheet; Back keeps the words.
          <InstallSheet
            id={entry}
            onBack={() => void navigate({ to: "/apps/new", search: q ? { q } : {} })}
            onClose={onClose}
          />
        ) : (
          <NewAppSheet
            source="catalog"
            onSource={onSource}
            onClose={onClose}
            query={q ?? ""}
            onQuery={(next) => void navigate({ to: "/apps/new", search: next ? { q: next } : {}, replace: true })}
            onPick={(id) => void navigate({ to: "/apps/new", search: { ...(q ? { q } : {}), entry: id } })}
          />
        )}
      </AppsPage>
    );
  },
});
