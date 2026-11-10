// SPDX-License-Identifier: AGPL-3.0-only

export interface CatalogSearch {
  q?: string | undefined;
  category?: string | undefined;
}

/** Carry browsing filters through app details, installation and the way back. */
export function catalogSearch(search: Record<string, unknown>): CatalogSearch {
  const out: CatalogSearch = {};
  for (const key of ["q", "category"] as const) {
    const value = search[key];
    if (typeof value === "string" && value !== "") out[key] = value;
  }
  return out;
}
