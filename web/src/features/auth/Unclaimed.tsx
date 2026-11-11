// SPDX-License-Identifier: AGPL-3.0-only

import { Trans, useLingui } from "@lingui/react/macro";

import { CopyCommand } from "../system/CopyCommand";
import { StandalonePage } from "../system/StandalonePage";
import { useClaimCommand } from "./useAuthState";

/** The install has no owner: only a claim link from the server can make one. */
export function Unclaimed({ serverName }: { serverName: string }) {
  const { t } = useLingui();
  const command = useClaimCommand();
  const name = serverName || window.location.hostname;
  return (
    <StandalonePage title={t`No owner yet`}>
      <header className="standalone-head">
        <h1 className="dw-text-title">
          <Trans>{name} has no owner yet</Trans>
        </h1>
        <p className="standalone-lead">
          <Trans>Claim it with the link the install printed. For a new one, run this on {name}:</Trans>
        </p>
      </header>
      <CopyCommand command={command} />
    </StandalonePage>
  );
}
