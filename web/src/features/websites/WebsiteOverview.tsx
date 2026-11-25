// SPDX-License-Identifier: AGPL-3.0-only

import { clone } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  BackupState,
  type ServiceInfo,
  UpdateProblem,
  WebsitePauseMode,
  WebsiteSchema,
  WebsiteService,
} from "@dockwell/api";
import { Button, Callout, Panel, StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { formatBytes, formatDateTime } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { WatchAddress } from "../alerts/WatchAddress";
import { useDomainState, useSilent } from "../apps/DomainNote";
import { visitorUrl } from "../apps/domains";
import { useHealthWords } from "../apps/Health";
import { useMay } from "../auth/may";
import { BackUpSheet } from "../backups/BackUpSheet";
import { useWebsiteBackups } from "../backups/useBackups";
import { useChangeSheet } from "../changes/ChangeSheet";
import { RefLink } from "../shell/RefLink";
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
  const transport = useTransport();
  const sheet = useChangeSheet();
  const { row } = useWebsiteValue();
  const { site } = row;
  const kind = kindOf(site.spec);
  const php = site.spec?.serves.case === "php" ? site.spec.serves.value : undefined;
  const paused = Boolean(site.spec?.paused);
  const mayUpdate = useMay()(WebsiteService.method.updateWebsite, { project: site.id });

  const resume = () => {
    if (!site.spec) return;
    const next = clone(WebsiteSchema, site.spec);
    next.paused = false;
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(WebsiteService, transport).updateWebsite({
          id: site.id,
          website: next,
          summary: t`Resume ${site.domain || site.name}`,
        }),
    });
  };

  return (
    <div className="website-tab">
      {paused ? (
        <Callout
          tone="warning"
          title={t`This website is paused`}
          actions={
            <Button size="sm" variant="primary" icon="play" onClick={resume} {...mayUpdate.off}>
              <Trans>Resume</Trans>
            </Button>
          }
        >
          {site.spec?.pauseMode === WebsitePauseMode.PLAIN_503 ? (
            <Trans>Visitors currently receive a plain 503 response. Domains, certificates and files stay intact.</Trans>
          ) : (
            <Trans>Visitors currently see a 503 maintenance page. Domains, certificates and files stay intact.</Trans>
          )}
        </Callout>
      ) : null}
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
                    <Trans>Sends each request straight to the port</Trans>
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
          {kind === "folder" || kind === "php" ? <WebsiteBackupsCard /> : null}
        </div>
        <WhatItServes kind={kind} />
      </div>
    </div>
  );
}

function WebsiteBackupsCard() {
  const { t, i18n } = useLingui();
  const { row } = useWebsiteValue();
  const { site } = row;
  const [backingUp, setBackingUp] = useState(false);
  const backups = useWebsiteBackups(site.id);
  const latest = backups.data?.backups[0];
  const root = targetOf(site.spec);
  return (
    <Panel
      title={<Trans>Backups</Trans>}
      actions={
        <Button size="sm" variant="secondary" icon="archive" onClick={() => setBackingUp(true)}>
          <Trans>Back up now</Trans>
        </Button>
      }
    >
      <div className="website-fields">
        {latest ? (
          <p className="websites-muted">
            {latest.state === BackupState.DONE && latest.startedAt
              ? t`Latest folder backup: ${formatDateTime(timestampDate(latest.startedAt), i18n.locale)} (${formatBytes(latest.sizeBytes, i18n.locale)}).`
              : latest.state === BackupState.RUNNING
                ? t`A folder backup is running now.`
                : t`Latest folder backup failed.`}
          </p>
        ) : (
          <p className="websites-muted">
            <Trans>Back up this website's folder on demand or on a schedule in Backups.</Trans>
          </p>
        )}
        <RouterButtonLink to="/backups" size="sm" variant="ghost">
          <Trans>Open Backups</Trans>
        </RouterButtonLink>
      </div>
      {backingUp ? (
        <BackUpSheet
          volumes={[
            {
              id: site.id,
              name: site.domain || site.name,
              environment: site.environmentId,
              websiteId: site.id,
              websitePath: root,
            },
          ]}
          chosen={site.id}
          serverId={site.serverId}
          onClose={() => setBackingUp(false)}
        />
      ) : null}
    </Panel>
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
      {read === "read" ? (
        <WatchAddress url={visitorUrl(domain)} where={name} serving={look.state === "serving"} />
      ) : null}
      <Link to="/websites/$websiteId/certificate" params={{ websiteId: row.site.id }} className="website-part-link">
        <Trans>Certificate</Trans>
      </Link>
    </li>
  );
}

/**
 * A service that serves the website, how it runs, and its logs, and that Dockwell keeps
 * its image current, as it pins it, once the check says so (ADR 0079).
 */
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
      {service?.update?.problem === UpdateProblem.KEPT_BY_DOCKWELL ? (
        <span className="website-part-note">
          <Trans>Kept current by Dockwell</Trans>
        </span>
      ) : null}
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
      [t`Memory limit`, t`${p.memoryLimitMb || 128} MiB`],
      [t`What isn't a file`, p.filesOnly ? t`Gets a 404` : t`Goes to index.php`],
    );
  }
  const rules = site.spec?.rules;
  const activeRules = [
    rules?.basicAuth?.users.length ? t`Basic Auth` : "",
    rules?.cors?.enabled ? t`CORS` : "",
    rules?.rateLimit?.enabled ? t`Rate limit (${rules.rateLimit.requestsPerMinute || 60}/min)` : "",
    rules?.pathRules.length ? t`${rules.pathRules.length} redirect/rewrite` : "",
    rules?.subpathProxies.length ? t`${rules.subpathProxies.length} sub-path` : "",
    rules?.headers.length ? t`${rules.headers.length} headers` : "",
    rules?.staticCache?.enabled ? t`Static cache` : "",
    site.spec?.customCaddy ? t`Custom Caddy` : "",
  ].filter(Boolean);
  if (activeRules.length > 0) {
    facts.push([t`Rules`, activeRules.join(" · ")]);
  }
  if (site.serverName)
    facts.push([
      t`Server`,
      <code key="s">
        <RefLink kind="server" id={site.serverId} look="plain">
          {site.serverName}
        </RefLink>
      </code>,
    ]);
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
