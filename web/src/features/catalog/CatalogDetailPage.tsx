// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { CatalogService } from "@dockwell/api";
import { Button, Callout, Icon } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { Page } from "../shell/Page";
import { Facts, useEntryFacts } from "./access";
import { AppIcon, AppScreenshot, projectLink } from "./media";
import type { CatalogSearch } from "./search";
import { categoryLabel, useInstalls } from "./useCatalog";
import "./Catalog.css";

/** Browsing an app is read-only. Its installation form opens only from the Install action. */
export function CatalogDetailPage({
  id,
  search,
  children,
}: {
  id: string;
  search: CatalogSearch;
  children?: ReactNode;
}) {
  const { t, i18n } = useLingui();
  const got = useQuery(CatalogService.method.getCatalogEntry, { id });
  const installs = (useInstalls().data?.installs ?? []).filter((install) => install.pin?.entry === id);
  const entry = got.data?.entry;
  const facts = useEntryFacts(entry);
  const app = entry?.name ?? id;
  const label = categoryLabel(entry?.category ?? "");
  const category = typeof label === "string" ? label : i18n._(label);
  const back = (
    <Link to="/catalog" search={search} className="catalog-back" aria-label={t`Back to catalog`}>
      <Icon name="arrow-left" size={16} />
      <Trans>Catalog</Trans>
    </Link>
  );
  // A phone's header has room for an arrow, as every other page's has, not for the word beside the app's name.
  const phoneBack = (
    <Link to="/catalog" search={search} className="mobile-header-button" aria-label={t`Back to catalog`}>
      <Icon name="arrow-left" size={20} />
    </Link>
  );
  const install = entry ? (
    <RouterButtonLink
      to="/catalog/$entryId/install"
      params={{ entryId: id }}
      search={search}
      variant="primary"
      icon="download"
    >
      {installs.length ? t`Install another` : t`Install ${app}`}
    </RouterButtonLink>
  ) : undefined;
  return (
    <Page
      title={app}
      breadcrumb={back}
      backLink={phoneBack}
      lead={entry?.summary}
      badge={entry ? <AppIcon name={app} path={entry.icon} large /> : undefined}
      actions={install}
      phoneAction={
        entry ? (
          <RouterButtonLink
            to="/catalog/$entryId/install"
            params={{ entryId: id }}
            search={search}
            variant="primary"
            size="sm"
            aria-label={t`Install ${app}`}
          >
            <Trans>Install</Trans>
          </RouterButtonLink>
        ) : undefined
      }
      className="catalog-detail"
    >
      {got.error ? (
        <Callout tone="critical" title={t`Couldn't read this app`}>
          <p>{sentence(got.error)}</p>
          <Button onClick={() => void got.refetch()} icon="refresh">
            <Trans>Try again</Trans>
          </Button>
        </Callout>
      ) : !entry ? (
        <p className="catalog-note" aria-busy="true">
          <Trans>Reading this app</Trans>
        </p>
      ) : (
        <>
          <div className="catalog-detail-phone-intro">
            <AppIcon name={app} path={entry.icon} large />
            <p>{entry.summary}</p>
          </div>
          <dl className="catalog-facts">
            <div>
              <dt>
                <Trans>Category</Trans>
              </dt>
              <dd>{category}</dd>
            </div>
            <div>
              <dt>
                <Trans>Version</Trans>
              </dt>
              <dd>
                <code>{entry.version}</code>
              </dd>
            </div>
            {entry.appLicense ? (
              <div>
                <dt>
                  <Trans>Licence</Trans>
                </dt>
                <dd>{entry.appLicense}</dd>
              </div>
            ) : null}
            {installs.length > 0 ? (
              <div>
                <dt>
                  <Trans>On your servers</Trans>
                </dt>
                <dd>{plural(installs.length, { one: "# installation", other: "# installations" })}</dd>
              </div>
            ) : null}
          </dl>
          <div className="catalog-detail-layout">
            <div className="catalog-detail-main">
              {entry.screenshots.length > 0 ? (
                <section className="catalog-section" aria-labelledby="catalog-screenshots-heading">
                  <div className="catalog-section-head">
                    <h2 id="catalog-screenshots-heading">
                      <Trans>Screenshots</Trans>
                    </h2>
                    <span>
                      <Trans>Screens may vary by version</Trans>
                    </span>
                  </div>
                  <div className="catalog-screenshots">
                    {entry.screenshots.map((shot) => (
                      <figure key={shot.path}>
                        <AppScreenshot path={shot.path} caption={shot.caption} />
                        <figcaption>
                          {shot.caption}
                          {projectLink(shot.source) ? (
                            <a
                              href={projectLink(shot.source)}
                              target="_blank"
                              rel="noreferrer"
                              aria-label={t`Source for: ${shot.caption}`}
                            >
                              <Trans>Source</Trans>
                            </a>
                          ) : null}
                        </figcaption>
                      </figure>
                    ))}
                  </div>
                </section>
              ) : null}
              <section className="catalog-section" aria-labelledby="catalog-about-heading">
                <h2 id="catalog-about-heading">
                  <Trans>About {app}</Trans>
                </h2>
                <div className="catalog-description">
                  {(entry.description || entry.summary)
                    .split(/\n\s*\n/u)
                    .filter(Boolean)
                    .map((paragraph) => (
                      <p key={paragraph}>{paragraph}</p>
                    ))}
                </div>
                {entry.features.length > 0 ? (
                  <ul className="catalog-features">
                    {entry.features.map((feature) => (
                      <li key={feature}>
                        <Icon name="check" size={16} />
                        <span>{feature}</span>
                      </li>
                    ))}
                  </ul>
                ) : null}
              </section>
              {facts.length > 0 ? (
                <section className="catalog-section" aria-labelledby="catalog-start-heading">
                  <h2 id="catalog-start-heading">
                    <Trans>Good to know</Trans>
                  </h2>
                  <Facts facts={facts} />
                </section>
              ) : null}
            </div>
            <aside className="catalog-detail-aside" aria-label={t`App information`}>
              {[entry.homepage, entry.documentation, entry.appSource].some(projectLink) ? (
                <section className="dw-panel catalog-info" aria-labelledby="catalog-links-heading">
                  <h2 id="catalog-links-heading">
                    <Trans>Explore the app</Trans>
                  </h2>
                  <ProjectLink address={entry.homepage}>
                    <Trans>Website</Trans>
                  </ProjectLink>
                  <ProjectLink address={entry.documentation}>
                    <Trans>Documentation</Trans>
                  </ProjectLink>
                  <ProjectLink address={entry.appSource}>
                    <Trans>Source code</Trans>
                  </ProjectLink>
                </section>
              ) : null}
              {got.data && got.data.parts.length > 0 ? (
                <section className="dw-panel catalog-info" aria-labelledby="catalog-includes-heading">
                  <h2 id="catalog-includes-heading">
                    <Trans>Included in this install</Trans>
                  </h2>
                  <ul className="catalog-includes">
                    {got.data.parts.map((part) => (
                      <li key={part.service}>
                        <Icon name={part.database ? "database" : "service"} size={16} />
                        <div>
                          <strong>{part.service}</strong>
                          <code>{part.image}</code>
                          {part.volumes.length > 0 ? (
                            <span>
                              <Trans>Persistent storage</Trans>
                            </span>
                          ) : null}
                        </div>
                      </li>
                    ))}
                  </ul>
                  <p className="catalog-info-note">
                    <Icon name="commit" size={14} />
                    <Trans>You review the plan before anything runs.</Trans>
                  </p>
                </section>
              ) : null}
              {entry.hostAccess ? (
                <Callout tone="warning" title={t`Server access`}>
                  {entry.hostAccess}
                </Callout>
              ) : null}
              {installs.length > 0 ? (
                <section className="dw-panel catalog-info" aria-labelledby="catalog-installed-heading">
                  <h2 id="catalog-installed-heading">
                    <Trans>Your installations</Trans>
                  </h2>
                  {installs.map((installed) => (
                    <div className="catalog-installation" key={installed.projectId || installed.project}>
                      <span className="catalog-installation-name">
                        <strong>{installed.project}</strong>
                        <code>{installed.pin?.version}</code>
                      </span>
                      {installed.webServiceId ? (
                        <RouterButtonLink
                          to="/apps/$serviceId"
                          params={{ serviceId: installed.webServiceId }}
                          size="sm"
                          aria-label={t`Open app: ${installed.project}`}
                        >
                          <Trans>Open app</Trans>
                        </RouterButtonLink>
                      ) : (
                        <RouterButtonLink to="/apps" size="sm">
                          <Trans>Show apps</Trans>
                        </RouterButtonLink>
                      )}
                    </div>
                  ))}
                </section>
              ) : null}
            </aside>
          </div>
        </>
      )}
      {children}
    </Page>
  );
}

function ProjectLink({ address, children }: { address: string; children: ReactNode }) {
  const href = projectLink(address);
  return href ? (
    <a href={href} target="_blank" rel="noreferrer" className="catalog-project-link">
      {children}
      <Icon name="external" size={14} />
    </a>
  ) : null;
}
