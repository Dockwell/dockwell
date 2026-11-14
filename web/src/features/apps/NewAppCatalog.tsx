// SPDX-License-Identifier: AGPL-3.0-only

import { CatalogOrder } from "@dockwell/api";
import { Button, Icon, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { RefObject } from "react";

import { sentence } from "../../lib/errors";
import { AppIcon } from "../catalog/media";
import { useCatalogPages, useEntryWords } from "../catalog/useCatalog";
import { EmptyState } from "../shell/EmptyState";
import "../catalog/Catalog.css";
import "./Apps.css";

/** How many apps the list shows at first, and adds at each Show more. */
const PAGE = 20;

/**
 * New app from the catalog, its first source and its default (#617): a search over
 * the catalog, the apps most people start with under it until words are typed, and a
 * page more at a time. Picking one hands the sheet to that app's install form, the
 * Catalog page's own, with its needs, values and plan; Back comes here with the words
 * as they were. An image, a Compose file, a Git repository or a folder are the other
 * sources, a step away, in the same sheet.
 */
export function CatalogSource({
  query,
  onQuery,
  onPick,
  onImage,
  fieldRef,
}: {
  query: string;
  onQuery: (query: string) => void;
  onPick: (id: string) => void;
  /** Where to go for an app the catalog doesn't have. */
  onImage: () => void;
  fieldRef: RefObject<HTMLInputElement | null>;
}) {
  const { t } = useLingui();
  const said = useEntryWords();
  const typed = query.trim();
  const catalog = useCatalogPages(
    { q: query },
    { pageSize: PAGE, order: typed === "" ? CatalogOrder.FEATURED : CatalogOrder.UNSPECIFIED },
  );
  const entries = catalog.entries;
  const busy = catalog.isPlaceholderData || catalog.settling;
  return (
    <>
      <TextField
        ref={fieldRef}
        icon="search"
        aria-label={t`Search the catalog`}
        placeholder={t`Search the catalog`}
        value={query}
        onChange={(event) => onQuery(event.target.value)}
      />
      {catalog.error ? (
        <EmptyState icon="grid" title={t`Couldn't read the catalog`}>
          {sentence(catalog.error)}
        </EmptyState>
      ) : !entries ? (
        <ul className="new-app-entries" aria-busy="true">
          <li className="dw-sr">{t`Reading the catalog`}</li>
          {Array.from({ length: 6 }, (_, i) => `placeholder-${i}`).map((key) => (
            <li key={key} className="new-app-entry is-placeholder" />
          ))}
        </ul>
      ) : entries.length === 0 && !busy ? (
        <EmptyState
          icon="search"
          title={t`No app matches`}
          action={
            <Button icon="layers" onClick={onImage}>
              <Trans>Run an image</Trans>
            </Button>
          }
        />
      ) : (
        <section className="new-app-results" aria-labelledby="new-app-results">
          <h3 id="new-app-results" className="new-app-results-head">
            {typed === ""
              ? t({ message: "Featured", context: "catalog apps people start with" })
              : plural(catalog.total ?? entries.length, { one: "# app", other: "# apps" })}
          </h3>
          <ul className="new-app-entries" aria-busy={busy || undefined}>
            {entries.map((entry) => (
              <li key={entry.id}>
                <button type="button" className="new-app-entry" onClick={() => onPick(entry.id)}>
                  <AppIcon name={entry.name} path={entry.icon} />
                  <span className="new-app-entry-text">
                    <span className="new-app-entry-name">{entry.name}</span>
                    <span className="new-app-entry-summary">{said(entry).summary}</span>
                  </span>
                  <code className="new-app-entry-version">{entry.version}</code>
                  <Icon name="arrow-right" size={16} />
                </button>
              </li>
            ))}
          </ul>
          {catalog.hasNextPage ? (
            <div className="new-app-more">
              <Button variant="ghost" loading={catalog.isFetchingNextPage} onClick={() => void catalog.fetchNextPage()}>
                {plural((catalog.total ?? 0) - entries.length, { one: "Show # more", other: "Show # more" })}
              </Button>
            </div>
          ) : null}
        </section>
      )}
    </>
  );
}
