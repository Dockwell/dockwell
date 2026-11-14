// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, type ServiceInfo, ServiceService, type WebsiteRuntime, WebsiteService } from "@dockwell/api";
import { Button, Icon, StatusBadge, useOff } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link, type LinkProps } from "@tanstack/react-router";
import { useId } from "react";
import { RouterButtonLink } from "../../lib/links";
import { environmentOf, nameOf } from "../apps/app";
import { useHealthWords } from "../apps/Health";
import { type HealthView, worstHealth } from "../apps/health";
import { APP_LANGUAGES, LANGUAGE_MARKS, newerBuild, runtimeOf, versionsOf } from "../apps/runtime";
import { useApps } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import type { Row } from "./Standing";
import { useRuntimes } from "./website";

/** How many extensions a card names before it counts the rest. */
const NAMED = 5;

/**
 * What code on a server runs on, as Websites-List's Runtimes draws it: each version of
 * PHP a website can run, with its release, how the websites that use it run, which
 * those are, and the extensions it has; then each language an app from a folder runs
 * on (ADR 0058), with its versions and the apps on each. Each website and app runs its
 * own, so a version is in use rather than installed, and one's upgrade moves no other.
 */
export function Runtimes({ rows, health }: { rows: readonly Row[]; health: HealthView }) {
  const { t } = useLingui();
  const runtimes = useRuntimes();
  // A tile someone may not use stays, off, with why (#408, ADR 0059). An app from a folder is a plan, as New app's is.
  const may = useMay();
  const mayWebsite = may(WebsiteService.method.createWebsite);
  const mayApp = may(ChangeService.method.proposeChange);
  const services = useApps().data;
  const fromFolders = (services ?? []).filter((s) => runtimeOf(s) !== undefined);
  // An older control plane lists PHP alone, with no ID.
  const php = (runtimes.data ?? []).filter((r) => r.id === "php" || r.id === "");
  const used = (version: string) =>
    rows.filter((r) => r.site.spec?.serves.case === "php" && r.site.spec.serves.value.version === version);
  const languages = APP_LANGUAGES.filter((id) => versionsOf(id, runtimes.data).length > 0);
  const inUse =
    php.filter((r) => used(r.version).length > 0).length +
    new Set(fromFolders.map((s) => `${runtimeOf(s)?.language} ${runtimeOf(s)?.version}`)).size;
  if (!runtimes.data || runtimes.data.length === 0) return null;
  if (rows.length === 0 && fromFolders.length === 0) return null;
  // A card for each in use, as Websites-List draws them; the rest a tile each, PHP's versions together,
  // each starting a website or an app on its newest, as its Add runtime row does.
  const phpUsed = php.filter((r) => used(r.version).length > 0);
  const phpFree = php.filter((r) => used(r.version).length === 0);
  const appsOn = (id: string) => fromFolders.filter((s) => runtimeOf(s)?.language === id);
  const languagesUsed = languages.filter((id) => appsOn(id).length > 0);
  const languagesFree = languages.filter((id) => appsOn(id).length === 0);
  const [phpNewest] = phpFree;
  return (
    <section className="websites-runtimes" aria-labelledby="websites-runtimes">
      <div className="websites-runtimes-head">
        <h2 id="websites-runtimes" className="dw-text-heading">
          <Trans>Runtimes</Trans>
        </h2>
        <span className="websites-muted">
          {plural(inUse, {
            one: "# in use · each website and app runs its own",
            other: "# in use · each website and app runs its own",
          })}
        </span>
      </div>
      {phpUsed.length + languagesUsed.length > 0 ? (
        <div className="websites-runtimes-grid">
          {phpUsed.map((r) => (
            <RuntimeCard key={r.version} runtime={r} sites={used(r.version)} health={health} />
          ))}
          {languagesUsed.map((id) => (
            <LanguageCard key={id} versions={versionsOf(id, runtimes.data)} apps={appsOn(id)} health={health} />
          ))}
        </div>
      ) : null}
      {phpNewest || languagesFree.length > 0 ? (
        <div className="dw-panel websites-runtimes-free">
          <div className="websites-runtimes-free-head">
            <h3>
              <Trans>Not in use</Trans>
            </h3>
            {/* What a tile does, for whoever may use one: neither, and they're there to read, each saying why. */}
            {mayWebsite.allowed || mayApp.allowed ? (
              <span className="websites-muted">
                <Trans>Pick one to start a website or an app on it</Trans>
              </span>
            ) : null}
          </div>
          <ul className="websites-runtimes-tiles">
            {phpNewest ? (
              <li>
                <RuntimeTile
                  link={{ to: "/websites/new", search: { php: phpNewest.version } }}
                  mark="Ph"
                  name={phpNewest.language}
                  versions={phpFree.map((r) => r.version).join(", ")}
                  label={t`New website on ${phpNewest.language} ${phpNewest.version}`}
                  off={mayWebsite.off.off}
                />
              </li>
            ) : null}
            {languagesFree.map((id) => {
              const versions = versionsOf(id, runtimes.data);
              const [newest] = versions;
              if (!newest) return null;
              const language = newest.language;
              return (
                <li key={id}>
                  <RuntimeTile
                    link={{
                      to: "/apps/new",
                      search: { source: "folder", language: newest.id, version: newest.version },
                    }}
                    mark={LANGUAGE_MARKS[newest.id] ?? newest.id.slice(0, 2)}
                    name={language}
                    versions={versions.map((v) => v.version).join(", ")}
                    label={t`New app on ${language} ${newest.version}`}
                    off={mayApp.off.off}
                  />
                </li>
              );
            })}
          </ul>
        </div>
      ) : null}
      <p className="websites-runtimes-elsewhere">
        <Trans>
          Other languages run as apps from an image, or from the <Link to="/catalog">Catalog</Link>, with a domain of
          their own.
        </Trans>
      </p>
    </section>
  );
}

/**
 * A runtime not in use, as a tile that starts a website or an app on it: named by what it does, with its versions
 * as its description, and off, with why, for someone who may not.
 */
function RuntimeTile({
  link,
  mark,
  name,
  versions,
  label,
  off,
}: {
  link: Pick<LinkProps, "to" | "search">;
  mark: string;
  name: string;
  versions: string;
  label: string;
  off: string | undefined;
}) {
  const id = useId();
  const offed = useOff(off);
  const described = [id, offed.props["aria-describedby"]].filter(Boolean).join(" ");
  return (
    <Link {...link} className="websites-runtime-tile" aria-label={label} {...offed.props} aria-describedby={described}>
      <span className="websites-runtime-mark" aria-hidden="true">
        {mark}
      </span>
      <span className="websites-runtime-tile-text">
        <span className="websites-runtime-tile-name">{name}</span>
        <span id={id} className="code-sm websites-muted">
          {versions}
        </span>
      </span>
      <Icon name="plus" size={16} />
      {offed.why}
    </Link>
  );
}

/**
 * A language an app from a folder runs on: its versions, the apps on each and how they
 * run, an offer to move one on an older build to its version's newest, and New app on
 * the newest version.
 */
function LanguageCard({
  versions,
  apps,
  health,
}: {
  versions: readonly WebsiteRuntime[];
  apps: readonly ServiceInfo[];
  health: HealthView;
}) {
  const { t } = useLingui();
  const words = useHealthWords();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay();
  const [newest] = versions;
  if (!newest) return null;
  const language = newest.language;
  const runs = apps.length > 0 ? worstHealth(apps.map((a) => health.of(a))) : undefined;
  const older = apps.flatMap((a) => {
    const newer = newerBuild(a, versions);
    return newer ? [{ app: a, newer }] : [];
  });
  const [first] = apps;
  const more = apps.length - 1;
  return (
    <article className="dw-panel websites-runtime">
      <div className="websites-runtime-head">
        <span className="websites-runtime-mark" aria-hidden="true">
          {LANGUAGE_MARKS[newest.id] ?? newest.id.slice(0, 2)}
        </span>
        <div className="websites-runtime-name">
          <h3>{language}</h3>
          <span className="code-sm websites-muted">{versions.map((v) => v.release).join(" · ")}</span>
        </div>
        {runs ? <StatusBadge status={runs.status} label={words.word(runs)} labelIsState /> : null}
      </div>
      <dl className="websites-runtime-facts">
        <dt>
          <Trans>Used by</Trans>
        </dt>
        <dd>
          {first ? (
            <>
              {plural(apps.length, { one: "# app", other: "# apps" })}
              {" · "}
              <Link to="/apps/$serviceId" params={{ serviceId: first.id }} className="websites-runtime-site">
                {nameOf(first)}
              </Link>
              <span className="websites-muted"> {runtimeOf(first)?.version}</span>
              {more > 0 ? (
                <span className="websites-muted"> {plural(more, { one: "and # more", other: "and # more" })}</span>
              ) : null}
            </>
          ) : (
            <Trans>No app yet</Trans>
          )}
        </dd>
        {older.length > 0 ? (
          <>
            <dt>
              <Trans>Build</Trans>
            </dt>
            <dd>
              {older.map(({ app, newer }) => {
                const name = nameOf(app);
                const version = newer.version;
                const release = newer.release;
                return (
                  <span key={app.id} className="websites-runtime-older">
                    <span>
                      <Trans>
                        <code>{name}</code> runs an older build of {version}
                      </Trans>
                    </span>
                    <Button
                      size="sm"
                      {...may(ServiceService.method.changeServiceRuntime, { environment: environmentOf(app) }).off}
                      onClick={() =>
                        sheet.open({
                          kind: "plan",
                          make: () =>
                            createClient(ServiceService, transport).changeServiceRuntime({
                              id: app.id,
                              version,
                              summary: t`Update ${name} to ${language} ${release}`,
                            }),
                        })
                      }
                    >
                      <Trans>Update to {release}</Trans>
                    </Button>
                  </span>
                );
              })}
            </dd>
          </>
        ) : null}
        <dt>
          <Trans>Versions</Trans>
        </dt>
        <dd>{versions.map((v) => v.version).join(", ")}</dd>
      </dl>
      <div className="websites-runtime-actions">
        <RouterButtonLink
          to="/apps/new"
          search={{ source: "folder", language: newest.id, version: newest.version }}
          size="sm"
          icon="plus"
        >
          <Trans>
            New app on {language} {newest.version}
          </Trans>
        </RouterButtonLink>
      </div>
    </article>
  );
}

function RuntimeCard({ runtime, sites, health }: { runtime: WebsiteRuntime; sites: Row[]; health: HealthView }) {
  const { t } = useLingui();
  const may = useMay();
  const words = useHealthWords();
  const transport = useTransport();
  const sheet = useChangeSheet();
  // A website keeps the build of its version it was planned with, until it's planned again.
  const older = sites.filter((s) => s.site.phpImage !== "" && s.site.phpImage !== runtime.image);
  const runs =
    sites.length > 0 ? worstHealth(sites.map((s) => health.of(s.php ?? { id: s.site.phpServiceId }))) : undefined;
  const [first] = sites;
  const more = sites.length - 1;
  const extra = runtime.extensions.length - NAMED;
  return (
    <article className="dw-panel websites-runtime">
      <div className="websites-runtime-head">
        <span className="websites-runtime-mark" aria-hidden="true">
          Ph
        </span>
        <div className="websites-runtime-name">
          <h3>{`${runtime.language} ${runtime.version}`}</h3>
          <span className="code-sm websites-muted">{runtime.release} · php-fpm</span>
        </div>
        {runs ? <StatusBadge status={runs.status} label={words.word(runs)} labelIsState /> : null}
      </div>
      <dl className="websites-runtime-facts">
        <dt>
          <Trans>Used by</Trans>
        </dt>
        <dd>
          {first ? (
            <>
              {plural(sites.length, { one: "# website", other: "# websites" })}
              {" · "}
              <Link to="/websites/$websiteId" params={{ websiteId: first.id }} className="websites-runtime-site">
                {first.site.domain || first.site.name}
              </Link>
              {more > 0 ? (
                <span className="websites-muted"> {plural(more, { one: "and # more", other: "and # more" })}</span>
              ) : null}
            </>
          ) : (
            <Trans>No website yet</Trans>
          )}
        </dd>
        {older.length > 0 ? (
          <>
            <dt>
              <Trans>Build</Trans>
            </dt>
            <dd>
              {older.map((s) => {
                const name = s.site.domain || s.site.name;
                return (
                  <span key={s.id} className="websites-runtime-older">
                    <span>
                      <Trans>
                        <code>{name}</code> runs an older build of {runtime.version}
                      </Trans>
                    </span>
                    <Button
                      size="sm"
                      {...may(WebsiteService.method.updateWebsite, { project: s.id }).off}
                      onClick={() =>
                        sheet.open({
                          kind: "plan",
                          make: () =>
                            createClient(WebsiteService, transport).updateWebsite({
                              id: s.id,
                              website: s.site.spec,
                              summary: t`Update ${name}'s PHP to ${runtime.release}`,
                            }),
                        })
                      }
                    >
                      <Trans>Update to {runtime.release}</Trans>
                    </Button>
                  </span>
                );
              })}
            </dd>
          </>
        ) : null}
        <dt>
          <Trans>Extensions</Trans>
        </dt>
        <dd className="websites-runtime-extensions">
          {runtime.extensions.slice(0, NAMED).join(", ")}
          {extra > 0 ? (
            <span className="websites-muted"> {plural(extra, { one: "and # more", other: "and # more" })}</span>
          ) : null}
        </dd>
      </dl>
      <div className="websites-runtime-actions">
        <RouterButtonLink to="/websites/new" search={{ php: runtime.version }} size="sm" icon="plus">
          <Trans>New website on {runtime.version}</Trans>
        </RouterButtonLink>
      </div>
    </article>
  );
}
