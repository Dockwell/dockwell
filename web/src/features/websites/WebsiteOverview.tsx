// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { ServiceInfo } from "@dockwell/api";
import { Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useDomainState, useSilent } from "../apps/DomainNote";
import { useHealthWords } from "../apps/Health";
import { DomainPanel } from "./WebsiteConfiguration";
import { useServesWords } from "./WebsiteFields";
import { useWebsiteValue } from "./WebsiteLayout";
import { kindOf, targetOf } from "./website";

/**
 * A website at a glance: each part of what serves it and how it runs, from the
 * domain to the code, and what it serves, with the way to change it. Everything here
 * is read from what the other tabs show in full.
 */
export function WebsiteOverview() {
  const { t } = useLingui();
  const { row } = useWebsiteValue();
  const { site } = row;
  const kind = kindOf(site.spec);
  const php = site.spec?.serves.case === "php" ? site.spec.serves.value : undefined;
  return (
    <div className="website-tab">
      <div className="website-columns">
        <div className="website-settings">
          <Panel title={<Trans>How it serves</Trans>} flush>
            <ul className="website-parts">
              <DomainPart />
              {site.serviceId ? (
                <ServicePart
                  name={t`Web server`}
                  detail="Caddy"
                  service={row.service}
                  id={site.serviceId}
                  logs={{ websiteId: site.id }}
                />
              ) : (
                <li className="website-part">
                  <span className="website-part-name">
                    <Trans>The ingress</Trans>
                  </span>
                  <span className="website-part-detail">
                    <Trans>Sends each request straight to the port, with nothing of its own between</Trans>
                  </span>
                </li>
              )}
              {php && site.phpServiceId ? (
                <ServicePart
                  name={t`PHP ${php.version}`}
                  detail="php-fpm"
                  service={row.php}
                  id={site.phpServiceId}
                  logs={{ websiteId: site.id, which: "php" }}
                />
              ) : null}
            </ul>
          </Panel>
          <DomainPanel />
        </div>
        <WhatItServes kind={kind} />
      </div>
    </div>
  );
}

/** The domain, as the first part a visitor reaches, and where its certificate stands. */
function DomainPart() {
  const { t } = useLingui();
  const { row, read } = useWebsiteValue();
  if (!row.domain || !row.look) {
    return (
      <li className="website-part">
        <span className="website-part-name">
          <Trans>Domain</Trans>
        </span>
        <StatusBadge status="unknown" label={t`No domain`} labelIsState />
      </li>
    );
  }
  return <DomainStanding name={row.site.domain} read={read} />;
}

function DomainStanding({ name, read }: { name: string; read: Parameters<typeof useDomainState>[3] }) {
  const { row } = useWebsiteValue();
  const domain = row.domain as NonNullable<typeof row.domain>;
  const look = row.look as NonNullable<typeof row.look>;
  const silent = useSilent(domain, look);
  const [status, word] = useDomainState(domain, look, silent, read);
  return (
    <li className="website-part">
      <span className="website-part-name">
        <code>{name}</code>
      </span>
      <StatusBadge status={status} label={word} labelIsState />
      <Link to="/websites/$websiteId/certificate" params={{ websiteId: row.site.id }} className="website-part-link">
        <Trans>Certificate</Trans>
      </Link>
    </li>
  );
}

/** A service that serves the website, how it runs, and its logs. */
function ServicePart({
  name,
  detail,
  service,
  id,
  logs,
}: {
  name: string;
  detail: string;
  service: ServiceInfo | undefined;
  id: string;
  logs: { websiteId: string; which?: "php" };
}) {
  const { health } = useWebsiteValue();
  const words = useHealthWords();
  const runs = health.of(service ?? { id });
  return (
    <li className="website-part">
      <span className="website-part-name">{name}</span>
      <span className="website-part-detail code-sm">{detail}</span>
      <StatusBadge status={runs.status} label={words.word(runs)} labelIsState />
      <Link
        to="/websites/$websiteId/logs"
        params={{ websiteId: logs.websiteId }}
        search={logs.which ? { which: logs.which } : {}}
        className="website-part-link"
      >
        <Trans>Logs</Trans>
      </Link>
    </li>
  );
}

/** What a website serves, in the words its configuration has, and when that last changed. */
function WhatItServes({ kind }: { kind: ReturnType<typeof kindOf> }) {
  const { t, i18n } = useLingui();
  const { row } = useWebsiteValue();
  const { site } = row;
  const serves = useServesWords();
  const facts: [string, ReactNode][] = [];
  if (kind !== "unknown") facts.push([t`Serves`, serves[kind]]);
  if (kind === "folder" || kind === "php") facts.push([t`Folder`, <code key="f">{targetOf(site.spec)}</code>]);
  if (kind === "url") facts.push([t`URL`, <code key="u">{targetOf(site.spec)}</code>]);
  if (kind === "port") facts.push([t`Port`, <code key="p">{targetOf(site.spec)}</code>]);
  if (site.spec?.serves.case === "php") {
    const p = site.spec.serves.value;
    facts.push(
      [t`PHP`, p.version],
      [
        t`Runs as`,
        <span key="r">
          <code>{p.user || "82:82"}</code>
          {(p.user || "82:82") === "82:82" ? <span className="websites-muted"> · www-data</span> : null}
        </span>,
      ],
      [t`Upload limit`, t`${p.uploadLimitMb || 64} MiB`],
      [t`What isn't a file`, p.filesOnly ? t`Gets a 404` : t`Goes to index.php`],
    );
  }
  if (site.serverName) facts.push([t`Server`, <code key="s">{site.serverName}</code>]);
  if (site.updateTime) facts.push([t`Changed`, formatDateTime(timestampDate(site.updateTime), i18n.locale)]);
  return (
    <Panel
      title={<Trans>What it serves</Trans>}
      className="website-serves"
      actions={
        <RouterButtonLink to="/websites/$websiteId/configuration" params={{ websiteId: site.id }} size="sm">
          <Trans>Change it</Trans>
        </RouterButtonLink>
      }
    >
      <dl className="website-facts">
        {facts.map(([name, value]) => (
          <div key={name} className="website-fact">
            <dt>{name}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </Panel>
  );
}
