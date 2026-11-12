// SPDX-License-Identifier: AGPL-3.0-only

export interface CatalogSearch {
  q?: string | undefined;
  category?: string | undefined;
  /** The catalog an app is read and installed from, by its source's name, where several have it (ADR 0078). */
  catalog?: string | undefined;
}

/** Carry browsing filters through app details, installation and the way back. */
export function catalogSearch(search: Record<string, unknown>): CatalogSearch {
  const out: CatalogSearch = {};
  for (const key of ["q", "category", "catalog"] as const) {
    const value = search[key];
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  return out;
}
