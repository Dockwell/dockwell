// SPDX-License-Identifier: AGPL-3.0-only

import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useLocation } from "@tanstack/react-router";

import { StandalonePage } from "./StandalonePage";

export function NotFound() {
  const { t } = useLingui();
  const { pathname } = useLocation();
  return (
    <StandalonePage title={t`No page here`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>No page here</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>
            There's nothing at <code>{pathname}</code>. <Link to="/">Go to the overview</Link>
          </Trans>
        </p>
      </header>
    </StandalonePage>
  );
}
