// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type WebsiteRuntime, WebsiteService } from "@dockwell/api";
import { Button, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { RouterButtonLink } from "../../lib/links";
import { useHealthWords } from "../apps/Health";
import { type HealthView, worstHealth } from "../apps/health";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import type { Row } from "./Standing";
import { useRuntimes } from "./website";

/** How many extensions a card names before it counts the rest. */
const NAMED = 5;

/**
 * The versions of PHP a website can run, as Websites-List's Runtimes draws them: each
 * with its release, how the websites that use it run, which those are, and the
 * extensions it has. Each PHP website runs its own PHP-FPM, so a version is in use
 * rather than installed, and one site's upgrade moves no other.
 */
export function Runtimes({ rows, health }: { rows: readonly Row[]; health: HealthView }) {
  const runtimes = useRuntimes();
  const used = (version: string) =>
    rows.filter((r) => r.site.spec?.serves.case === "php" && r.site.spec.serves.value.version === version);
  const inUse = (runtimes.data ?? []).filter((r) => used(r.version).length > 0).length;
  if (!runtimes.data || runtimes.data.length === 0) return null;
  return (
    <section className="websites-runtimes" aria-labelledby="websites-runtimes">
      <div className="websites-runtimes-head">
        <h2 id="websites-runtimes" className="dw-text-heading">
          <Trans>Runtimes</Trans>
        </h2>
        <span className="websites-muted">
          {plural(inUse, {
            one: "# in use · each PHP website runs its own",
            other: "# in use · each PHP website runs its own",
          })}
        </span>
      </div>
      <div className="websites-runtimes-grid">
        {runtimes.data.map((r) => (
          <RuntimeCard key={r.version} runtime={r} sites={used(r.version)} health={health} />
        ))}
      </div>
      <p className="websites-runtimes-elsewhere">
        <Trans>
          Node.js, Python and other languages run as apps, from an image or from the <Link to="/catalog">Catalog</Link>,
          with a domain of their own.
        </Trans>
      </p>
    </section>
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
        {runs ? (
          <StatusBadge status={runs.status} label={words.word(runs)} labelIsState />
        ) : (
          <StatusBadge status="stopped" label={t`Not used`} labelIsState />
        )}
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
