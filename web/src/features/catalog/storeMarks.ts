// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The look of each store Dockwell reads directly (ADR 0083), by the ID the control
 * plane gives it: the short name a source of it reads as, and its mark, a file of the
 * panel's own, so the page asks no other host for it. The rest of what a store is
 * comes from ListCatalogStores.
 */
export const STORE_MARKS: Readonly<Record<string, { label: string; mark: string }>> = {
  onepanel: { label: "1Panel", mark: "/stores/onepanel.png" },
  dokploy: { label: "Dokploy", mark: "/stores/dokploy.svg" },
  coolify: { label: "Coolify", mark: "/stores/coolify.svg" },
};

/** How a person says a store a source reads, short, by the store's ID, such as "1Panel". */
export function storeLabel(id: string): string | undefined {
  return STORE_MARKS[id]?.label;
}
