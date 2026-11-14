// SPDX-License-Identifier: AGPL-3.0-only

import { type CatalogInstall, CatalogOrder, type CatalogSourceInfo, type Entry } from "@dockwell/api";
import { Button, Callout, cx, Icon, SegmentedControl, SelectField, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { AppIcon } from "./media";
import { StoreMark } from "./Stores";
import type { CatalogSearch } from "./search";
import {
  categoryLabel,
  type EntryWords,
  installOf,
  monogram,
  stranger,
  useCatalogPages,
  useEntryWords,
  useInstalls,
  useOrigins,
  useSourceNames,
} from "./useCatalog";
import "./Catalog.css";

export interface CatalogPageProps {
  search?: CatalogSearch;
  onSearch?: (next: CatalogSearch) => void;
  /** What's over the page, such as the install sheet. */
  children?: ReactNode;
}

/**
 * The catalog (Catalog): the apps you can install, by search and category, with
 * what's already installed marked. Every card opens the app's details before the
 * reader chooses to install it or open an existing installation. The control plane
 * searches and pages it, and the next page comes as the reader nears the end
 * (ADR 0092).
 */
export function CatalogPage({ search = {}, onSearch, children }: CatalogPageProps) {
  const { t, i18n } = useLingui();
  const origins = useOrigins();
  const sources = origins.data?.sources ?? [];
  // The catalog the grid is narrowed to, by its source's name, as a source's card links to it.
  const from = search.catalog ? sources.find((source) => source.name === search.catalog) : undefined;
  // Once the sources are read, only a catalog there is narrows the grid, and one that
  // isn't leaves the address, so an old link never empties the grid with no way out.
  // The apps most people start with first, then the most installed, as Catalog opens on them; by name a
  // step away, and kept in the address (#621).
  const catalog = useCatalogPages(
    { ...search, catalog: origins.data ? from?.name : search.catalog },
    { order: search.sort === "name" ? CatalogOrder.UNSPECIFIED : CatalogOrder.FEATURED },
  );
  useEffect(() => {
    if (origins.data && search.catalog && !from) onSearch?.({ ...search, catalog: undefined });
  }, [origins.data, search, from, onSearch]);
  const also = catalog.also;
  const named = useSourceNames();
  // Both reads again, whichever failed.
  const retry = () => {
    void catalog.refetch();
    void origins.refetch();
  };
  const installs = useInstalls().data?.installs;
  const said = useEntryWords();
  // The control plane narrows to the chosen catalog, each app as that catalog has it.
  const entries = catalog.entries;
  const words = (category: string) => {
    const label = categoryLabel(category);
    return typeof label === "string" ? label : i18n._(label);
  };
  // The categories of what the words and the catalog find, whichever is chosen, in the
  // order the reader's language sorts their words, so zh-CN reads in pinyin; the chosen
  // one stays a choice when the words find nothing in it.
  const counted = catalog.categories;
  // biome-ignore lint/correctness/useExhaustiveDependencies: words reads only the locale, which is here
  const categories = useMemo(() => {
    const collator = new Intl.Collator(i18n.locale);
    const all = counted.filter((c) => c.listed > 0).map((c) => c.category);
    if (search.category && !all.includes(search.category)) all.push(search.category);
    return all.sort((a, b) => collator.compare(words(a), words(b)));
  }, [counted, search.category, i18n.locale]);
  const chips = useRef<HTMLFieldSetElement>(null);
  const [more, setMore] = useState(false);
  // A fade at the row's end while there's more of it to scroll to, as on a phone.
  const measure = useCallback(() => {
    const row = chips.current;
    setMore(row !== null && row.scrollLeft + row.clientWidth < row.scrollWidth - 1);
  }, []);
  // The chosen category in view, as the row scrolls sideways on a phone, on arrival and on each choice.
  // biome-ignore lint/correctness/useExhaustiveDependencies: a choice and the chips arriving are what it runs on
  useEffect(() => {
    chips.current
      ?.querySelector<HTMLElement>('[aria-pressed="true"]')
      ?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    measure();
  }, [search.category, categories, measure]);
  useEffect(() => {
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [measure]);
  const shown = entries ?? [];
  const typed = (search.q ?? "").trim();
  // Whether the words find an app in a category other than the chosen one.
  const elsewhere = counted.some((c) => c.category !== search.category && c.count > 0);
  const count = new Set((installs ?? []).map((install) => install.project)).size;
  // The next page, once the reader is near the end of the grid.
  const end = useRef<HTMLDivElement>(null);
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = catalog;
  useEffect(() => {
    const el = end.current;
    if (!el || !hasNextPage || typeof IntersectionObserver === "undefined") return;
    const seen = new IntersectionObserver(
      (items) => {
        if (items.some((item) => item.isIntersecting) && !isFetchingNextPage) void fetchNextPage();
      },
      { rootMargin: "800px 0px" },
    );
    seen.observe(el);
    return () => seen.disconnect();
  }, [hasNextPage, isFetchingNextPage, fetchNextPage]);
  return (
    <Page
      width="wide"
      title={t`Catalog`}
      back="/more"
      lead={t`Find an app for your server. Explore what it does before you install.`}
      actions={
        <>
          <RouterButtonLink to="/catalog/sources" variant="ghost" icon="branch">
            {t`Sources`}
          </RouterButtonLink>
          {count > 0 ? (
            <RouterButtonLink to="/apps" variant="secondary" icon="service">
              {t`${count} installed`}
            </RouterButtonLink>
          ) : null}
        </>
      }
      className="catalog-page"
    >
      {/* The sources say which apps come from another catalog. Without them the grid still shows,
          unmarked, and installing waits for them (InstallSheet). */}
      {origins.error && !catalog.error ? (
        <Callout
          tone="warning"
          title={t`Couldn't read the sources, so apps from them aren't marked`}
          actions={
            <Button size="sm" icon="refresh" onClick={retry}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(origins.error)}
        </Callout>
      ) : null}
      {catalog.error ? (
        <Callout
          tone="critical"
          title={t`Couldn't read the catalog`}
          actions={
            <Button size="sm" icon="refresh" onClick={retry}>
              <Trans>Try again</Trans>
            </Button>
          }
        >
          {sentence(catalog.error)}
        </Callout>
      ) : null}

      {/* The search and the categories narrow the grid under them, so the three are one block, as Catalog draws it. */}
      <section className="catalog-browse" aria-label={t`Apps`}>
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
          {sources.length > 1 ? (
            <div className="catalog-from-pick">
              <SelectField
                aria-label={t`Catalog`}
                value={from?.name ?? ""}
                options={[
                  { value: "", label: t`Every catalog` },
                  ...sources.map((source) => ({ value: source.name, label: named(source.name) })),
                ]}
                onChange={(event) =>
                  onSearch?.({ ...search, catalog: event.target.value || undefined, category: undefined })
                }
              />
            </div>
          ) : null}
          {/* Shown wherever there's something to choose, never hidden by a choice made, so the
              way back from one is always there (#632). */}
          {categories.length > 0 || search.installed || search.category ? (
            <fieldset
              ref={chips}
              className={cx("catalog-chips", more && "has-more")}
              aria-label={t`Categories`}
              onScroll={measure}
            >
              <Chip
                on={!search.category && !search.installed}
                onClick={() => onSearch?.({ ...search, category: undefined, installed: undefined })}
              >
                {t`All`}
              </Chip>
              {/* It turns on and off in place, as a category does. */}
              <Chip
                on={search.installed === true}
                onClick={() => onSearch?.({ ...search, installed: search.installed ? undefined : true })}
              >
                {t({ message: "Installed", context: "catalog filter" })}
              </Chip>
              {categories.map((category) => (
                <Chip
                  key={category}
                  on={search.category === category}
                  onClick={() => onSearch?.({ ...search, category })}
                  // With Installed on, how many of its apps are installed, none greyed rather than gone.
                  count={search.installed ? (counted.find((c) => c.category === category)?.count ?? 0) : undefined}
                >
                  {words(category)}
                </Chip>
              ))}
            </fieldset>
          ) : null}
        </div>
        {from ? (
          <div className="catalog-from">
            <SourceMark source={from} said={named(from.name)} />
            <span className="catalog-from-words">
              <strong>{t`From ${named(from.name)}`}</strong>
              <span>{plural(catalog.total ?? 0, { one: "# app", other: "# apps" })}</span>
            </span>
            <Button
              size="sm"
              variant="ghost"
              icon="x"
              onClick={() => onSearch?.({ ...search, catalog: undefined, category: undefined })}
            >
              <Trans>Show every catalog</Trans>
            </Button>
          </div>
        ) : null}
        {catalog.error ? null : !entries ? (
          // Where the cards go, while the first page is read: no spinner over the grid.
          <div className="catalog-grid" aria-busy="true">
            <span className="dw-sr">{t`Reading the catalog`}</span>
            {Array.from({ length: 12 }, (_, i) => `placeholder-${i}`).map((key) => (
              <span key={key} className="dw-panel catalog-card is-placeholder" />
            ))}
          </div>
        ) : shown.length === 0 && !catalog.isPlaceholderData && !catalog.settling ? (
          typed !== "" ? (
            <EmptyState
              icon="search"
              title={<Trans>No app matches</Trans>}
              action={
                // Where other categories have it, the way on is to look there; otherwise to start the search again.
                search.category && elsewhere ? (
                  <Button size="lg" onClick={() => onSearch?.({ ...search, category: undefined })}>
                    <Trans>Search every category</Trans>
                  </Button>
                ) : (
                  <Button size="lg" onClick={() => onSearch?.({ ...search, q: undefined, category: undefined })}>
                    <Trans>Clear the search</Trans>
                  </Button>
                )
              }
            >
              <p>
                <Trans>
                  No app here has <code>{typed}</code> in its name or what it does.
                </Trans>
              </p>
            </EmptyState>
          ) : (
            // No words typed: a filter, such as Installed, holds nothing; the way on is every app.
            <EmptyState
              icon={search.installed ? "service" : "grid"}
              title={
                search.installed ? (
                  <Trans>Nothing from the catalog is installed yet</Trans>
                ) : (
                  <Trans>No app here yet</Trans>
                )
              }
              action={
                <Button
                  size="lg"
                  onClick={() =>
                    // The order isn't a filter, so it stays.
                    onSearch?.({
                      q: undefined,
                      category: undefined,
                      catalog: undefined,
                      installed: undefined,
                      sort: search.sort,
                    })
                  }
                >
                  <Trans>Show every app</Trans>
                </Button>
              }
            />
          )
        ) : (
          <>
            <div className="catalog-count-row">
              {from ? null : (
                // Narrowed to one catalog, its own line says how many.
                <p className="catalog-count" aria-live="polite">
                  {plural(catalog.total ?? shown.length, { one: "# app", other: "# apps" })}
                </p>
              )}
              <SegmentedControl
                className="catalog-order"
                size="sm"
                label={t`Order`}
                value={search.sort ?? "popular"}
                options={[
                  { value: "popular", label: t`Popular first` },
                  { value: "name", label: t`By name` },
                ]}
                onChange={(order) => onSearch?.({ ...search, sort: order === "name" ? "name" : undefined })}
              />
            </div>
            <div className="catalog-grid" aria-busy={catalog.isPlaceholderData || catalog.settling || undefined}>
              {shown.map((entry) => (
                <Card
                  key={entry.id}
                  entry={entry}
                  words={said(entry)}
                  category={words(entry.category)}
                  search={search}
                  source={from ? (from.own ? undefined : from.name) : stranger(origins.origins, entry.id)?.name}
                  also={
                    // Narrowed to one catalog, the others that have the app, the one it's listed from among them.
                    from
                      ? [origins.origins.get(entry.id)?.name ?? "", ...(also.get(entry.id) ?? [])].filter(
                          (name) => name !== from.name,
                        )
                      : also.get(entry.id)
                  }
                  named={named}
                  installs={(installs ?? []).filter((i) => installOf(i, entry.id, origins.origins, origins.also))}
                />
              ))}
            </div>
            {hasNextPage ? (
              <div ref={end} className="catalog-more">
                <Button variant="secondary" loading={isFetchingNextPage} onClick={() => void fetchNextPage()}>
                  {plural((catalog.total ?? 0) - shown.length, { one: "Show # more", other: "Show # more" })}
                </Button>
              </div>
            ) : null}
          </>
        )}
      </section>

      <div className="catalog-foot">
        <p className="catalog-note">
          <Trans>
            Not listed? Deploy any image or Compose file as an app.{" "}
            <Link to="/apps/new" search={{ source: "image" }} className="catalog-link">
              New app
            </Link>
          </Trans>
        </p>
        <p className="catalog-note">
          <Trans>
            Read apps from a catalog in a Git repository too.{" "}
            <Link to="/catalog/sources" className="catalog-link">
              Sources
            </Link>
          </Trans>
        </p>
      </div>
      {children}
    </Page>
  );
}

function Chip({
  on,
  onClick,
  count,
  children,
}: {
  on: boolean;
  onClick: () => void;
  count?: number | undefined;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-pressed={on}
      className={cx("catalog-chip", on && "is-on", count === 0 && !on && "is-none")}
      onClick={onClick}
    >
      {children}
      {count === undefined ? null : (
        <>
          {" "}
          <span className="catalog-chip-count">{count}</span>
        </>
      )}
    </button>
  );
}

/** One app: its own logo, what it does, its version and any existing installations. */
/** A catalog's mark: Dockwell's own, a store's, or its initials. */
function SourceMark({ source, said }: { source: CatalogSourceInfo; said: string }) {
  if (source.own) {
    return (
      <span className="store-mark has-art is-small" aria-hidden="true">
        <img src="/favicon.svg" alt="" width={32} height={32} />
      </span>
    );
  }
  if (source.store) return <StoreMark id={source.store} name={said} size={32} />;
  return (
    <span className="store-mark is-small" aria-hidden="true">
      {monogram(said)}
    </span>
  );
}

function Card({
  entry,
  words,
  category,
  source,
  also,
  named,
  installs,
  search,
}: {
  entry: Entry;
  /** What it says, in the reader's language where its catalog writes it there. */
  words: EntryWords;
  /** Its category, in words, under its name as Catalog has it. */
  category: string;
  /** The catalog source it comes from, if it isn't Dockwell's own. */
  source?: string | undefined;
  /** The other catalogs that have it, by their sources' names. */
  also?: readonly string[] | undefined;
  /** How a person says a catalog, by its source's name. */
  named: (name: string) => string;
  installs: readonly CatalogInstall[];
  search: CatalogSearch;
}) {
  const { t, i18n } = useLingui();
  const list = (names: readonly string[]) => new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format(names);
  const [first] = installs;
  const count = installs.length;
  const behind = installs.some((install) => install.upgrade);
  const sameVersion = installs.every((install) => !install.upgrade || install.upgrade.version === install.pin?.version);
  const latest = entry.version;
  const body = (
    <>
      <span className="catalog-card-head">
        <AppIcon name={entry.name} path={entry.icon} />
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
      <span className="catalog-card-summary">{words.summary}</span>
      <span className="catalog-card-foot">
        <span className="catalog-card-about">
          <code>{entry.version}</code>
          {also?.length ? (
            <span
              className="catalog-card-from"
              title={t`From the catalogs ${list([source ?? "", ...also].filter(Boolean))}`}
            >
              <Icon name="branch" size={12} />
              <span>
                {named(source ?? "")} · {t`also in ${list(also.map(named))}`}
              </span>
            </span>
          ) : source ? (
            <span className="catalog-card-from" title={t`From the catalog ${source}`}>
              <Icon name="branch" size={12} />
              <span>{named(source)}</span>
            </span>
          ) : null}
        </span>
        <span className="catalog-card-more" aria-hidden="true">
          <Icon name="arrow-right" size={14} />
        </span>
      </span>
    </>
  );
  return (
    <Link to="/catalog/$entryId" params={{ entryId: entry.id }} search={search} className="dw-panel catalog-card">
      {body}
    </Link>
  );
}
