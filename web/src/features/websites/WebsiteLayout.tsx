// SPDX-License-Identifier: AGPL-3.0-only

import { ButtonLink, Callout, Icon, PHONE_QUERY, StatusBadge, TabNav, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, useMatch } from "@tanstack/react-router";
import { createContext, type ReactNode, useContext } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink, RouterTabLink } from "../../lib/links";
import type { DomainRead } from "../apps/domains";
import { visitorUrl } from "../apps/domains";
import type { HealthView } from "../apps/health";
import { EmptyState } from "../shell/EmptyState";
import { Page } from "../shell/Page";
import { type Row, SiteStanding, useCertificateWords, useWebsiteRows } from "./Standing";
import { kindOf, targetOf } from "./website";
import "../apps/Apps.css";
import "./Websites.css";

/** One website, as its frame read it, for the tab under it. */
export interface WebsiteValue {
  row: Row;
  health: HealthView;
  read: DomainRead;
}

const WebsiteContext = createContext<WebsiteValue | undefined>(undefined);

/** The website the frame around this tab shows. */
export function useWebsiteValue(): WebsiteValue {
  const value = useContext(WebsiteContext);
  if (!value) throw new Error("useWebsiteValue is for a tab inside WebsiteLayout");
  return value;
}

/**
 * One website's frame, whichever tab is open, as Website-Detail draws it: where it
 * sits, its domain and how it serves, what it is and its certificate, a way to visit
 * it, and its tabs, each a route of its own under /websites/<id>.
 */
export function WebsiteLayout({ websiteId, children }: { websiteId: string; children: ReactNode }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const { websites, rows, read, health } = useWebsiteRows();
  const row = rows.find((r) => r.id === websiteId);
  const certificate = useCertificateWords();
  const onLogs = useMatch({ from: "/_shell/websites/$websiteId/logs", shouldThrow: false }) !== undefined;
  const backLink = (
    <Link to="/websites" className="mobile-header-button" aria-label={t`Back`}>
      <Icon name="arrow-left" size={20} />
    </Link>
  );

  if (!row) {
    return (
      <Page title={t`Website`} backLink={backLink} breadcrumb={<Crumbs />}>
        {websites.isPending ? (
          <div className="website-loading" aria-busy="true">
            <span className="dw-sr">
              <Trans>Loading the website</Trans>
            </span>
            <div className="app-skeleton is-tabs" />
            <div className="app-skeleton is-panel" />
          </div>
        ) : websites.error ? (
          <Callout tone="critical" title={t`Can't load this website`}>
            {sentence(websites.error)} <Trans>Nothing changed on the server. Reload the page to try again.</Trans>
          </Callout>
        ) : (
          <EmptyState
            icon="browser"
            title={<Trans>No website with this ID</Trans>}
            action={
              <RouterButtonLink to="/websites" size="lg">
                <Trans>Go to the websites</Trans>
              </RouterButtonLink>
            }
          >
            <p>
              <Trans>
                <code>{websiteId}</code> may have been deleted, or the link may be wrong.
              </Trans>
            </p>
          </EmptyState>
        )}
      </Page>
    );
  }

  const { site, domain } = row;
  const name = site.domain || site.name;
  const kind = kindOf(site.spec);
  const port = site.spec?.serves.case === "proxy" ? site.spec.serves.value.port : 0;
  const root = targetOf(site.spec);
  const what =
    kind === "php"
      ? t`PHP ${site.spec?.serves.case === "php" ? site.spec.serves.value.version : ""} from ${root}`
      : kind === "folder"
        ? t`Static files from ${root}`
        : kind === "port"
          ? t`Reverse proxy to port ${port} on the server`
          : kind === "url"
            ? t`Reverse proxy to ${root}`
            : t`Dockwell can't read what this website serves`;
  const lead = [
    what,
    kind === "url" || kind === "folder" || kind === "php" ? "Caddy" : "",
    row.look && domain ? certificate(domain, row.look, read) : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const visit = domain ? visitorUrl(domain) : site.domain ? `https://${site.domain}` : "";
  return (
    <WebsiteContext value={{ row, health, read }}>
      <Page
        title={name}
        machineTitle
        backLink={backLink}
        breadcrumb={<Crumbs name={name} />}
        badge={
          <SiteStanding row={row} health={health} read={read}>
            {({ status, word }) => <StatusBadge status={status} label={word} labelIsState title={word} />}
          </SiteStanding>
        }
        lead={lead}
        phoneSub={what}
        actions={
          <>
            {/* A folder's files are edited where every file on its server is, at the folder. */}
            {(kind === "folder" || kind === "php") && site.serverId ? (
              <RouterButtonLink
                to="/servers/$serverId/files"
                params={{ serverId: site.serverId }}
                search={{ path: root }}
                icon="folder"
              >
                <Trans>Edit files</Trans>
              </RouterButtonLink>
            ) : null}
            {visit ? (
              <ButtonLink href={visit} target="_blank" rel="noreferrer" iconAfter="external">
                <Trans>Visit site</Trans>
              </ButtonLink>
            ) : null}
          </>
        }
        className={phone ? "website-page is-phone" : "website-page"}
        // A log view takes the whole width, as an app's does, under the head and tabs of a standard page.
        fullBody={onLogs && Boolean(site.serviceId)}
      >
        <TabNav label={t`Website sections`}>
          <RouterTabLink to="/websites/$websiteId" params={{ websiteId }} activeOptions={{ exact: true }}>
            <Trans>Overview</Trans>
          </RouterTabLink>
          <RouterTabLink to="/websites/$websiteId/configuration" params={{ websiteId }}>
            <Trans>Configuration</Trans>
          </RouterTabLink>
          {kind === "folder" || kind === "php" ? (
            <RouterTabLink to="/websites/$websiteId/files" params={{ websiteId }}>
              <Trans>Files</Trans>
            </RouterTabLink>
          ) : null}
          <RouterTabLink to="/websites/$websiteId/logs" params={{ websiteId }}>
            <Trans>Logs</Trans>
          </RouterTabLink>
          <RouterTabLink to="/websites/$websiteId/certificate" params={{ websiteId }}>
            <Trans>Certificate</Trans>
          </RouterTabLink>
        </TabNav>
        {children}
      </Page>
    </WebsiteContext>
  );
}

function Crumbs({ name }: { name?: string }) {
  const { t } = useLingui();
  return (
    <nav className="app-crumbs" aria-label={t`Breadcrumb`}>
      <Link to="/websites">
        <Trans>Websites</Trans>
      </Link>
      {name ? (
        <>
          <span aria-hidden="true">/</span>
          <span aria-current="page" className="website-crumb">
            {name}
          </span>
        </>
      ) : null}
    </nav>
  );
}
