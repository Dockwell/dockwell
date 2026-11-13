// SPDX-License-Identifier: AGPL-3.0-only

import type { CatalogInstall, CatalogSourceInfo, Entry } from "@dockwell/api";
import { Button, Callout, cx, Icon, SelectField, TextField } from "@dockwell/ui";
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
  useAlso,
  useCatalog,
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
 * reader chooses to install it or open an existing installation.
 */
export function CatalogPage({ search = {}, onSearch, children }: CatalogPageProps) {
  const { t, i18n } = useLingui();
  const catalog = useCatalog();
  const origins = useOrigins();
  const also = useAlso();
  const named = useSourceNames();
  // Both reads again, whichever failed.
  const retry = () => {
    void catalog.refetch();
    void origins.refetch();
  };
  const installs = useInstalls().data?.installs;
  const said = useEntryWords();
  const sources = origins.data?.sources ?? [];
  // The catalog the grid is narrowed to, by its source's name, as a source's card links to it.
  const from = search.catalog ? sources.find((source) => source.name === search.catalog) : undefined;
  // Each app as the chosen catalog has it, where it isn't the one the app is listed from.
  const theirs = useMemo(() => {
    const out = new Map<string, Entry>();
    for (const a of catalog.data?.also ?? []) if (a.catalog === from?.name && a.entry) out.set(a.id, a.entry);
    return out;
  }, [catalog.data, from]);
  const entries = useMemo(() => {
    const all = catalog.data?.entries;
    if (!all || !from) return all;
    return all
      .filter((e) => origins.origins.get(e.id)?.name === from.name || theirs.has(e.id))
      .map((e) => theirs.get(e.id) ?? e);
  }, [catalog.data, from, origins.origins, theirs]);
  const words = (category: string) => {
    const label = categoryLabel(category);
    return typeof label === "string" ? label : i18n._(label);
  };
  // In the order the reader's language sorts their words, so zh-CN reads in pinyin.
  // biome-ignore lint/correctness/useExhaustiveDependencies: words reads only the locale, which is here
  const categories = useMemo(() => {
    const collator = new Intl.Collator(i18n.locale);
    return [...new Set((entries ?? []).map((e) => e.category))].sort((a, b) => collator.compare(words(a), words(b)));
  }, [entries, i18n.locale]);
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
  const query = (search.q ?? "").trim().toLowerCase();
  const shown = (entries ?? []).filter(
    (e) => (!search.category || e.category === search.category) && matches(e, said(e), query, words),
  );
  const typed = (search.q ?? "").trim();
  // Whether the words find an app in a category other than the chosen one.
  const elsewhere = (entries ?? []).some((e) => e.category !== search.category && matches(e, said(e), query, words));
  const count = new Set((installs ?? []).map((install) => install.project)).size;
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
          {categories.length > 1 ? (
            <fieldset
              ref={chips}
              className={cx("catalog-chips", more && "has-more")}
              aria-label={t`Categories`}
              onScroll={measure}
            >
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
        {from ? (
          <div className="catalog-from">
            <SourceMark source={from} said={named(from.name)} />
            <span className="catalog-from-words">
              <strong>{t`From ${named(from.name)}`}</strong>
              <span>{plural(entries?.length ?? 0, { one: "# app", other: "# apps" })}</span>
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
        {catalog.error ? null : !entries || (!origins.data && !origins.error) ? (
          <p className="catalog-note" aria-busy="true">
            <Trans>Reading the catalog</Trans>
          </p>
        ) : shown.length === 0 ? (
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
          <div className="catalog-grid">
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
        )}
      </section>

      <div className="catalog-foot">
        <p className="catalog-note">
          <Trans>
            Not listed? Deploy any image or Compose file as an app.{" "}
            <Link to="/apps/new" className="catalog-link">
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

function Chip({ on, onClick, children }: { on: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" aria-pressed={on} className={cx("catalog-chip", on && "is-on")} onClick={onClick}>
      {children}
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
        <span className="catalog-card-more">
          <Trans>View app</Trans>
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

/**
 * Whether an entry has the words, lowercased, in its name, what it does in its own
 * language or the reader's, its category or its features.
 */
function matches(e: Entry, said: EntryWords, query: string, words: (category: string) => string): boolean {
  return (
    query === "" ||
    `${e.name} ${e.summary} ${said.summary} ${e.category} ${words(e.category)} ${e.features.join(" ")}`
      .toLowerCase()
      .includes(query)
  );
}
