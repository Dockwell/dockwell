// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { FirewallPage } from "../../features/servers/FirewallPage";
import { readListSearch } from "../../features/shell/list";

/** What filters a server's inbound traffic. */
export const Route = createFileRoute("/_shell/servers/$serverId/firewall")({
  validateSearch: readListSearch,
  component: function Firewall() {
    const { serverId } = Route.useParams();
    const search = Route.useSearch();
    const navigate = useNavigate();
    return (
      <FirewallPage
        id={serverId}
        search={search}
        onSearch={(next) =>
          void navigate({
            to: "/servers/$serverId/firewall",
            params: { serverId },
            search: next,
            replace: true,
            resetScroll: false,
          })
        }
      />
    );
  },
});
