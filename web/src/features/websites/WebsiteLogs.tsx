// SPDX-License-Identifier: AGPL-3.0-only

import { Trans } from "@lingui/react/macro";

import { LogsPage, type LogsSearch } from "../logs/LogsPage";
import { EmptyState } from "../shell/EmptyState";
import { useWebsiteValue } from "./WebsiteLayout";
import { kindOf } from "./website";

/**
 * A website's logs: its web server's, as an app's logs read, with each request it sent
 * on. A website that sends to a port on the server has no web server of its own, so
 * what listens there keeps its own.
 */
export function WebsiteLogs({ search, onSearch }: { search: LogsSearch; onSearch: (next: LogsSearch) => void }) {
  const { row } = useWebsiteValue();
  const { site } = row;
  if (site.serviceId) return <LogsPage id={site.serviceId} search={search} onSearch={onSearch} />;
  const port = site.spec?.serves.case === "proxy" ? site.spec.serves.value.port : 0;
  return (
    <EmptyState icon="file" title={<Trans>Nothing here writes logs</Trans>}>
      <p>
        {kindOf(site.spec) === "port" ? (
          <Trans>
            The ingress sends this website straight to port {port} on the server, with nothing of its own between. What
            listens there keeps its own logs.
          </Trans>
        ) : (
          <Trans>This website has no web server of its own now, so there's nothing of its own to read.</Trans>
        )}
      </p>
    </EmptyState>
  );
}
