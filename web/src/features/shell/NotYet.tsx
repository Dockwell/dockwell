// SPDX-License-Identifier: AGPL-3.0-only

import { Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { Page } from "./Page";
import { UPCOMING, type UpcomingSection } from "./sections";
import "./NotYet.css";

/**
 * A section of the design that has no screen yet. It says so plainly rather
 * than showing a mock-up, and points back to what works today.
 */
export function NotYet({ section }: { section: UpcomingSection }) {
  const { i18n } = useLingui();
  const { label, icon, inMore } = UPCOMING[section];
  return (
    <Page title={i18n._(label)} back={inMore ? "/more" : undefined}>
      <div className="not-yet">
        <span className="not-yet-icon">
          <Icon name={icon} size={20} />
        </span>
        <h2 className="dw-text-heading">
          <Trans>Not in this release yet</Trans>
        </h2>
        <p className="not-yet-text">
          <Trans>
            This part of Dockwell isn't built yet; it arrives in a later release. Until then, the overview shows how
            your servers are doing.
          </Trans>
        </p>
        <Link to="/" className="dw-btn dw-btn-secondary dw-btn-lg">
          <Trans>Go to the overview</Trans>
        </Link>
      </div>
    </Page>
  );
}
