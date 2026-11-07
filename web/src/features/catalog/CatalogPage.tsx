// SPDX-License-Identifier: AGPL-3.0-only

import type { CatalogInstall, Entry } from "@dockwell/api";
import { Callout, cx, Icon, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useMemo } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { Page } from "../shell/Page";
import { categoryLabel, monogram, useCatalog, useInstalls } from "./useCatalog";
import "./Catalog.css";

export interface CatalogSearch {
  q?: string | undefined;
  category?: string | undefined;
}

export interface CatalogPageProps {
  search?: CatalogSearch;
  onSearch?: (next: CatalogSearch) => void;
  /** What's over the page, such as the install sheet. */
  children?: ReactNode;
}

/**
 * The catalog (Catalog): the apps you can install, by search and category, with
 * what's already installed marked and leading to the app. An app's card opens its
 * install sheet. The panel draws a monogram for each, as a logo is its project's to
 * give (ADR 0029).
 */
export function CatalogPage({ search = {}, onSearch, children }: CatalogPageProps) {
  const { t, i18n } = useLingui();
  const catalog = useCatalog();
  const installs = useInstalls().data?.installs;
  const entries = catalog.data?.entries;
  const words = (category: string) => {
    const label = categoryLabel(category);
    return typeof label === "string" ? label : i18n._(label);
  };
  const categories = useMemo(() => [...new Set((entries ?? []).map((e) => e.category))].sort(), [entries]);
  const query = (search.q ?? "").trim().toLowerCase();
  const shown = (entries ?? []).filter(
    (e) =>
      (!search.category || e.category === search.category) &&
      (query === "" || `${e.name} ${e.summary} ${e.category}`.toLowerCase().includes(query)),
  );
  const count = new Set((installs ?? []).map((install) => install.project)).size;
  return (
    <Page
      width="wide"
      title={t`Catalog`}
      back="/more"
      lead={t`Self-hosted apps you can install in one step`}
      actions={
        count > 0 ? (
          <RouterButtonLink to="/apps" variant="secondary" icon="service">
            {t`${count} installed`}
          </RouterButtonLink>
        ) : undefined
      }
      className="catalog-page"
    >
      <div className="catalog-bar">
        <div className="catalog-search">
          <TextField
            icon="search"
            placeholder={t`Search apps`}
            aria-label={t`Search apps`}
            value={search.q ?? ""}
            onChange={(event) => onSearch?.({ ...search, q: event.target.value })}
          />
        </div>
        {categories.length > 1 ? (
          <fieldset className="catalog-chips" aria-label={t`Categories`}>
            <Chip on={!search.category} onClick={() => onSearch?.({ ...search, category: undefined })}>
              {t`All`}
            </Chip>
            {categories.map((category) => (
              <Chip
                key={category}
                on={search.category === category}
                onClick={() => onSearch?.({ ...search, category })}
              >
                {words(category)}
              </Chip>
            ))}
          </fieldset>
        ) : null}
      </div>

      {catalog.error ? (
        <Callout tone="critical" title={t`Couldn't read the catalog`}>
          {sentence(catalog.error)}
        </Callout>
      ) : !entries ? (
        <p className="catalog-note" aria-busy="true">
          <Trans>Reading the catalog</Trans>
        </p>
      ) : shown.length === 0 ? (
        <p className="catalog-note">
          <Trans>No app matches that. Try another word, or all categories.</Trans>
        </p>
      ) : (
        <div className="catalog-grid">
          {shown.map((entry) => (
            <Card
              key={entry.id}
              entry={entry}
              category={words(entry.category)}
              installs={(installs ?? []).filter((i) => i.pin?.entry === entry.id)}
            />
          ))}
        </div>
      )}

      <p className="catalog-note">
        <Trans>
          Not listed? Deploy any image or Compose file as an app.{" "}
          <Link to="/apps/new" className="catalog-link">
            New app
          </Link>
        </Trans>
      </p>
      {children}
    </Page>
  );
}

function Chip({ on, onClick, children }: { on: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" aria-pressed={on} className={cx("catalog-chip", on && "is-on")} onClick={onClick}>
      {children}
    </button>
  );
}

/** One app: what it is and its version. Installed, it leads to the app rather than its install sheet. */
function Card({
  entry,
  category,
  installs,
}: {
  entry: Entry;
  /** Its category, in words, under its name as Catalog has it. */
  category: string;
  installs: readonly CatalogInstall[];
}) {
  const { t } = useLingui();
  const [first] = installs;
  const count = installs.length;
  const app = entry.name;
  const behind = installs.some((install) => install.upgrade);
  const sameVersion = installs.every((install) => !install.upgrade || install.upgrade.version === install.pin?.version);
  const latest = entry.version;
  const body = (
    <>
      <span className="catalog-card-head">
        <span aria-hidden="true" className="catalog-mono">
          {monogram(entry.name)}
        </span>
        <span className="catalog-card-title">
          <span className="catalog-card-name">{entry.name}</span>
          {category ? <span className="catalog-card-category">{category}</span> : null}
        </span>
        {behind ? (
          <span className="catalog-installed is-behind">{sameVersion ? t`Update out` : t`Upgrade to ${latest}`}</span>
        ) : first ? (
          <span className="catalog-installed">
            <Icon name="check" size={12} />
            {plural(count, { one: "Installed", other: "# installed" })}
          </span>
        ) : null}
      </span>
      <span className="catalog-card-summary">{entry.summary}</span>
      <span className="catalog-card-foot">
        <span>{first?.pin?.version ?? entry.version}</span>
        {first ? <span className="catalog-card-project">{first.project}</span> : null}
      </span>
    </>
  );
  if (first?.webServiceId) {
    return (
      <Link
        to="/apps/$serviceId"
        params={{ serviceId: first.webServiceId }}
        className="dw-panel catalog-card"
        aria-label={t`Open ${app}`}
      >
        {body}
      </Link>
    );
  }
  if (first) {
    return (
      <Link to="/apps" className="dw-panel catalog-card" aria-label={t`Open ${app}`}>
        {body}
      </Link>
    );
  }
  return (
    <Link
      to="/catalog/$entryId"
      params={{ entryId: entry.id }}
      className="dw-panel catalog-card"
      aria-label={t`Install ${app}`}
    >
      {body}
    </Link>
  );
}
