// SPDX-License-Identifier: AGPL-3.0-only

import { Loading, Skeleton, SkeletonCard } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import "./Page.css";

/**
 * A screen whose code is still on its way, which only a slow network shows: a
 * page's head and a panel, at their sizes, where the screen will be.
 */
export function RoutePending() {
  const { t } = useLingui();
  return (
    <div className="page">
      <Loading label={t`Loading the page`} slow={t`This network is slow to bring the page`} className="route-pending">
        <div className="page-head" aria-hidden="true">
          <div className="page-heading">
            <Skeleton text="title" width={200} />
            <Skeleton width={280} />
          </div>
        </div>
        <SkeletonCard lines={4} />
      </Loading>
    </div>
  );
}
