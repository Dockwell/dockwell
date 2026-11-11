// SPDX-License-Identifier: AGPL-3.0-only

import type { CatalogInstall, Entry } from "@dockwell/api";
import { Button, Callout, cx, Icon, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { Page } from "../shell/Page";
import { AppIcon } from "./media";
import type { CatalogSearch } from "./search";
import { categoryLabel, installOf, useCatalog, useInstalls, useOrigins } from "./useCatalog";
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
  // Both reads again, whichever failed.
  const retry = () => {
    void catalog.refetch();
    void origins.refetch();
  };
  const installs = useInstalls().data?.installs;
  const entries = catalog.data?.entries;
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
    (e) =>
      (!search.category || e.category === search.category) &&
      (query === "" ||
        `${e.name} ${e.summary} ${e.category} ${words(e.category)} ${e.features.join(" ")}`
          .toLowerCase()
          .includes(query)),
  );
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
      ) : !entries || (!origins.data && !origins.error) ? (
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
              search={search}
              source={origins.origins.get(entry.id)?.name}
              installs={(installs ?? []).filter((i) => installOf(i, entry.id, origins.origins))}
            />
          ))}
        </div>
      )}

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
function Card({
  entry,
  category,
  source,
  installs,
  search,
}: {
  entry: Entry;
  /** Its category, in words, under its name as Catalog has it. */
  category: string;
  /** The catalog source it comes from, if it isn't Dockwell's own. */
  source?: string;
  installs: readonly CatalogInstall[];
  search: CatalogSearch;
}) {
  const { t } = useLingui();
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
      <span className="catalog-card-summary">{entry.summary}</span>
      <span className="catalog-card-foot">
        <span className="catalog-card-about">
          <code>{entry.version}</code>
          {source ? (
            <span className="catalog-card-from" title={t`From the catalog ${source}`}>
              <Icon name="branch" size={12} />
              <span>{source}</span>
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
