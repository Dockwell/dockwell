// SPDX-License-Identifier: AGPL-3.0-only

import { Trans, useLingui } from "@lingui/react/macro";

import { CopyCommand } from "../system/CopyCommand";
import { StandalonePage } from "../system/StandalonePage";

/** The install has no owner: only a claim link from the server can make one. */
export function Unclaimed({ serverName }: { serverName: string }) {
  const { t } = useLingui();
  const name = serverName || window.location.hostname;
  return (
    <StandalonePage title={t`No owner yet`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>{name} has no owner yet</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>
            Claim it with the one-time link that <code>dockwell run</code> printed when it started. For a new link, run
            this on {name} over SSH:
          </Trans>
        </p>
      </header>
      <CopyCommand command="dockwell claim" />
    </StandalonePage>
  );
}
