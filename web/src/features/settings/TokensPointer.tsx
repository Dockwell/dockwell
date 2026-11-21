// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { TokenService } from "@dockwell/api";
import { Panel, Skeleton } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";

import { useControlPlaneNow } from "../servers/useFleet";
import { expiresWithin, SOON } from "./TokensPage";
import "./Settings.css";

/**
 * Your API tokens in brief, on Profile and security: how many act for you, how many
 * expire soon, and Manage, which goes where they're listed and made (#801).
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
    if (list.length === 0) said = t`None yet`;
    else if (soon > 0) said = [acting, expiring].join(" · ");
    else said = acting;
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
        {tokens.data ? said : <Skeleton width={200} inline />}
      </p>
    </Panel>
  );
}
