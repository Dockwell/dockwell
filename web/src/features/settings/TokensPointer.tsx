// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { TokenService } from "@dockwell/api";
import { Panel } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { useControlPlaneNow } from "../servers/useFleet";
import { expiresWithin, SOON } from "./TokensPage";
import "./Settings.css";

/**
 * Your API tokens in brief, on Profile and security, leading to where they're
 * listed and made: Settings, under API tokens.
 */
export function TokensPointer() {
  const { t } = useLingui();
  const tokens = useQuery(TokenService.method.listTokens, {});
  const now = useControlPlaneNow();
  const list = tokens.data?.tokens ?? [];
  const soon = list.filter((token) => expiresWithin(token, now, SOON)).length;
  let said = "";
  if (tokens.data) {
    const acting = plural(list.length, { one: "# acts for you", other: "# act for you" });
    const expiring = plural(soon, { one: "# expires within two weeks", other: "# expire within two weeks" });
    if (list.length === 0) said = t`None yet. Make one for each script or integration that calls the API.`;
    else if (soon > 0) said = t`${acting}, and ${expiring}.`;
    else said = t`${acting}.`;
  }
  return (
    <Panel
      title={t`API tokens`}
      className="profile-panel"
      actions={
        <Link to="/settings/api-tokens" className="cp-head-link">
          <Trans>Manage</Trans>
        </Link>
      }
    >
      <p className="tokens-pointer" aria-busy={tokens.data === undefined}>
        {said}{" "}
        <Trans>
          They're listed, made and revoked in{" "}
          <Link to="/settings/api-tokens" className="cp-link">
            Settings, under API tokens
          </Link>
          .
        </Trans>
      </p>
    </Panel>
  );
}
