// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { monogram } from "./useCatalog";

/** Catalog artwork is served by the control plane, from what it read of the catalog, never loaded from an entry's host. */
export function mediaPath(path: string): string | undefined {
  return /^\/catalog-media\/[a-z0-9]+(?:[-_/][a-z0-9]+)*\.(?:svg|png|jpg|jpeg|webp|avif)$/.test(path)
    ? path
    : undefined;
}

/** External project links are navigations, and cannot contain executable schemes or credentials. */
export function projectLink(address: string): string | undefined {
  try {
    const url = new URL(address);
    return url.protocol === "https:" && !url.username && !url.password && !/[\s]/u.test(address) ? url.href : undefined;
  } catch {
    return undefined;
  }
}

/** The project's own mark, with a quiet fallback for entries that have no artwork. */
export function AppIcon({ name, path, large = false }: { name: string; path: string; large?: boolean }) {
  const [failed, setFailed] = useState<string>();
  const src = mediaPath(path);
  return (
    <span className={cx("catalog-icon", large && "is-large")} aria-hidden="true">
      {src && failed !== src ? (
        <img src={src} alt="" width={large ? 72 : 48} height={large ? 72 : 48} onError={() => setFailed(src)} />
      ) : (
        monogram(name)
      )}
    </span>
  );
}

/** A real screenshot. A missing image says so rather than showing a broken image or an invented preview. */
export function AppScreenshot({ path, caption }: { path: string; caption: string }) {
  const { t } = useLingui();
  const [failed, setFailed] = useState<string>();
  const src = mediaPath(path);
  if (!src || failed === src) {
    return (
      <div className="catalog-preview-missing">
        <Icon name="browser" size={24} />
        <Trans>Preview unavailable</Trans>
      </div>
    );
  }
  return (
    <a href={src} target="_blank" rel="noreferrer" aria-label={t`Open full size: ${caption}`}>
      <img src={src} alt={caption} loading="lazy" decoding="async" onError={() => setFailed(src)} />
      <span className="catalog-preview-open">
        <Icon name="external" size={14} />
        <Trans>Open full size</Trans>
      </span>
    </a>
  );
}
