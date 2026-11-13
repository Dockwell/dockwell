// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { CatalogService, type CatalogSourceInfo, type CatalogStore } from "@dockwell/api";
import { Button, cx, Icon } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo } from "react";

import { useChangeSheet } from "../changes/ChangeSheet";
import { useStoreFormatWords } from "./SourceFormats";
import { STORE_MARKS } from "./storeMarks";
import { monogram } from "./useCatalog";
import "./Stores.css";

/** The stores, the one with the most apps that install first. */
export function useStores() {
  return useQuery(CatalogService.method.listCatalogStores, {});
}

/** A store's mark on the light tile logos are drawn for, or its initials. */
export function StoreMark({ id, name, size = 40 }: { id: string; name: string; size?: 32 | 40 }) {
  const mark = STORE_MARKS[id]?.mark;
  return (
    <span className={cx("store-mark", mark && "has-art", size === 32 && "is-small")} aria-hidden="true">
      {mark ? <img src={mark} alt="" width={size} height={size} /> : monogram(name)}
    </span>
  );
}

/** What a store is called, in the reader's language: its format's words, as the store's own repository reads. */
export function useStoreName(): (store: CatalogStore) => string {
  const formatWords = useStoreFormatWords();
  return (store) => formatWords(store.format) ?? store.name;
}

/** A name for a store's source: the store's ID, or the next one no source has. */
export function storeSourceName(store: CatalogStore, taken: readonly string[]): string {
  if (!taken.includes(store.id)) return store.id;
  for (let n = 2; ; n++) if (!taken.includes(`${store.id}-${n}`)) return `${store.id}-${n}`;
}

/**
 * Adding a store: the plan, at once, for a source named by the store's ID, following
 * its default branch at the commit it's at, read as that store. Nothing to type; the
 * plan says what it brings.
 */
export function useAddStore(sources: readonly CatalogSourceInfo[]): (store: CatalogStore) => void {
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  return (store) =>
    sheet.open({
      kind: "plan",
      make: () =>
        client.setCatalogSource({
          name: storeSourceName(
            store,
            sources.map((s) => s.name),
          ),
          repository: store.repository,
          path: "",
          follow: store.branch,
          format: store.format,
        }),
    });
}

/**
 * What a store brings, in a line: that a source reads it, where one does, how many of
 * its apps install, and its licence, which never wraps.
 */
export function StoreFacts({ store }: { store: CatalogStore }) {
  const { t } = useLingui();
  const at = store.commit.slice(0, 12);
  return (
    <span className="store-facts">
      {store.source ? (
        <span className="store-added">
          <Icon name="check" size={12} />
          <Trans context="catalog store">Added</Trans>
        </span>
      ) : null}
      <span title={store.live ? t`As read at ${at}` : t`As counted at ${at}`}>
        {store.live
          ? plural(store.apps, {
              one: `${store.installs} of # app installs`,
              other: `${store.installs} of # apps install`,
            })
          : plural(store.apps, {
              one: `About ${store.installs} of # app installs`,
              other: `About ${store.installs} of # apps install`,
            })}
      </span>
      <span className="store-licence">{store.license}</span>
    </span>
  );
}

/**
 * One store, as a tile: its mark, its name on a line of its own, what it brings under
 * it, and at most one action at the end, the same height as every tile in its row.
 */
export function StoreTile({
  store,
  action,
  small = false,
}: {
  store: CatalogStore;
  action?: ReactNode;
  small?: boolean;
}) {
  const named = useStoreName();
  return (
    <li className="store-tile">
      <StoreMark id={store.id} name={store.label} size={small ? 32 : 40} />
      <span className="store-tile-words">
        <span className="store-tile-name">{named(store)}</span>
        <StoreFacts store={store} />
      </span>
      {action ? <span className="store-tile-action">{action}</span> : null}
    </li>
  );
}

/**
 * The stores Dockwell reads, each with its mark, what it brings, its licence, and Add,
 * or once a source reads it, Added and the way to that source. A store never leaves
 * the list once added.
 */
export function StoreGallery({
  sources,
  off,
  onGo,
}: {
  sources: readonly CatalogSourceInfo[];
  /** Spread onto Add: off, with why, for whoever may not add a source. */
  off: { off?: string; disabled?: true };
  /** Goes to the source that reads a store, by its name. */
  onGo: (source: string) => void;
}) {
  const { t } = useLingui();
  const stores = useStores();
  const add = useAddStore(sources);
  const named = useStoreName();
  const list = stores.data?.stores;
  if (stores.error || !list?.length) return null;
  return (
    <section className="store-gallery" aria-labelledby="store-gallery-heading">
      <h2 id="store-gallery-heading" className="store-gallery-heading">
        <Trans>App stores</Trans>
      </h2>
      <ul className="store-tiles">
        {list.map((store) => (
          <StoreTile
            key={store.id}
            store={store}
            action={
              store.source ? (
                <Button
                  size="sm"
                  variant="ghost"
                  icon="arrow-right"
                  aria-label={t`Go to ${store.label}`}
                  onClick={() => onGo(store.source)}
                >
                  <Trans context="catalog store">Go to it</Trans>
                </Button>
              ) : (
                <Button size="sm" icon="plus" aria-label={t`Add ${named(store)}`} {...off} onClick={() => add(store)}>
                  <Trans context="catalog store">Add</Trans>
                </Button>
              )
            }
          />
        ))}
      </ul>
    </section>
  );
}
