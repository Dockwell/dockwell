// SPDX-License-Identifier: AGPL-3.0-only

import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { EmptyState } from "./EmptyState";
import { Page } from "./Page";
import { UPCOMING, type UpcomingSection } from "./sections";

/**
 * A section of the design that has no screen yet. It says so plainly rather
 * than showing a mock-up, and points back to what works today.
 */
export function NotYet({ section }: { section: UpcomingSection }) {
  const { i18n } = useLingui();
  const { label, icon, inMore, width } = UPCOMING[section];
  return (
    <Page title={i18n._(label)} width={width} back={inMore ? "/more" : undefined}>
      <EmptyState
        icon={icon}
        title={<Trans>Not in this release yet</Trans>}
        action={
          <Link to="/" className="dw-btn dw-btn-secondary dw-btn-lg">
            <Trans>Go to the overview</Trans>
          </Link>
        }
      >
        <p>
          <Trans>
            This part of Dockwell isn't built yet; it arrives in a later release. Until then, the overview shows how
            your servers are doing.
          </Trans>
        </p>
      </EmptyState>
    </Page>
  );
}
