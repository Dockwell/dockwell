// SPDX-License-Identifier: AGPL-3.0-only

import { Loading, Logo, PHONE_QUERY, Skeleton, SkeletonCard, useMediaQuery } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useDocumentTitle } from "../../lib/useDocumentTitle";
import "./Page.css";
import "./Shell.css";

/**
 * The shell's shape while session check is in flight on a shell route: mounts
 * the rail and top bar (or bottom tab bar on a phone) with skeletons right away
 * instead of a blank splash, without exposing any interactive navigation before
 * auth resolves.
 */
export function ShellPending() {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  useDocumentTitle(t`Checking your session`);
  return (
    <div className="shell">
      {phone ? null : (
        <div className="rail shell-pending-rail" aria-hidden="true">
          <div className="rail-brand">
            <Logo variant="logotype" height={14} className="rail-logo" />
            <Logo variant="mark" height={14} className="rail-mark" />
          </div>
          <div className="shell-pending-group">
            <Skeleton shape="block" height={28} />
            <Skeleton shape="block" height={28} />
            <Skeleton shape="block" height={28} />
            <Skeleton shape="block" height={28} />
            <Skeleton shape="block" height={28} />
            <Skeleton shape="block" height={28} />
          </div>
        </div>
      )}
      <div className="shell-column">
        {phone ? null : (
          <div className="topbar shell-pending-topbar" aria-hidden="true">
            <div className="shell-pending-topbar-inner">
              <Skeleton width={120} />
              <div className="topbar-fill" />
              <Skeleton shape="block" width={240} height={32} />
            </div>
          </div>
        )}
        <main className="shell-main">
          <div className="page">
            <Loading
              label={t`Checking your session`}
              slow={t`The control server is slow to answer`}
              className="route-pending"
            >
              <div className="page-head" aria-hidden="true">
                <div className="page-heading">
                  <Skeleton text="title" width={200} />
                  <Skeleton width={280} />
                </div>
              </div>
              <SkeletonCard lines={4} />
            </Loading>
          </div>
        </main>
        {phone ? (
          <div className="tabbar shell-pending-tabbar" aria-hidden="true">
            <div className="shell-pending-tabs">
              <Skeleton shape="block" height={34} />
              <Skeleton shape="block" height={34} />
              <Skeleton shape="block" height={34} />
              <Skeleton shape="block" height={34} />
              <Skeleton shape="block" height={34} />
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}
