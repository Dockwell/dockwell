// SPDX-License-Identifier: AGPL-3.0-only

import { Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import type { CatalogSearch } from "./search";

import "./CatalogBack.css";

/** The way back to the catalog's apps, above the title of each page under it (#725). */
export function CatalogBack({ search }: { search?: CatalogSearch }) {
  const { t } = useLingui();
  return (
    <Link to="/catalog" search={search ?? {}} className="catalog-back" aria-label={t`Back to catalog`}>
      <Icon name="arrow-left" size={16} />
      <Trans>Catalog</Trans>
    </Link>
  );
}
