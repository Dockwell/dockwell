// SPDX-License-Identifier: AGPL-3.0-only

import { SegmentedControl } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { LogsPage, type LogsSearch } from "../logs/LogsPage";
import { EmptyState } from "../shell/EmptyState";
import { useWebsiteValue } from "./WebsiteLayout";
import { kindOf } from "./website";

/** Whose logs a website's tab shows: its web server's, or its PHP's. */
export type Which = "web" | "php";

/**
 * A website's logs: its web server's, as an app's logs read, with each request it
 * served, or for a PHP website, its PHP's, with what the code says. A website that
 * sends to a port on the server has no web server of its own, so what listens there
 * keeps its own.
 */
export function WebsiteLogs({
  which,
  search,
  onSearch,
  onWhich,
}: {
  which: Which;
  search: LogsSearch;
  onSearch: (next: LogsSearch) => void;
  onWhich: (next: Which) => void;
}) {
  const { t } = useLingui();
  const { row } = useWebsiteValue();
  const { site } = row;
  if (site.serviceId) {
    const php = which === "php" && site.phpServiceId !== "";
    const id = php ? site.phpServiceId : site.serviceId;
    return (
      <div className="website-tab">
        {site.phpServiceId ? (
          <SegmentedControl
            label={t`Whose logs`}
            size="sm"
            options={[
              { value: "web", label: t`Web server` },
              { value: "php", label: t`PHP` },
            ]}
            value={php ? "php" : "web"}
            onChange={(value) => onWhich(value === "php" ? "php" : "web")}
            className="website-logs-which"
          />
        ) : null}
        {/* Each service's logs are their own reading, so a switch starts afresh. */}
        <LogsPage key={id} id={id} search={search} onSearch={onSearch} />
      </div>
    );
  }
  const port = site.spec?.serves.case === "proxy" ? site.spec.serves.value.port : 0;
  return (
    <EmptyState icon="file" title={<Trans>Nothing here writes logs</Trans>}>
      <p>
        {kindOf(site.spec) === "port" ? (
          <Trans>What listens on port {port} keeps its own.</Trans>
        ) : (
          <Trans>It has no web server of its own now.</Trans>
        )}
      </p>
    </EmptyState>
  );
}
