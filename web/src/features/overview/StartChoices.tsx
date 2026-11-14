// SPDX-License-Identifier: AGPL-3.0-only

import { CatalogOrder } from "@dockwell/api";
import { cx, Icon, type IconName, PHONE_QUERY, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { CSSProperties, ReactNode } from "react";
import { AppIcon } from "../catalog/media";
import { useCatalog } from "../catalog/useCatalog";
import "./Start.css";

/** What a first app can come from. */
export type Way = "catalog" | "git" | "image" | "compose" | "website";

interface Choice {
  key: Way;
  icon: IconName;
  title: string;
  line: string;
  to: "/catalog" | "/apps/new" | "/websites/new";
  search?: { source: "git" | "image" | "compose" };
  extra?: ReactNode;
}

/** How many apps a row of the catalog's card holds, by name, each a click from its install. */
const ROW = 4;

/**
 * The ways a first app comes in, each opening its own create flow: the catalog, a
 * Git repository, a Docker image, a Compose file, a website. Large on the start page and
 * in an empty list, a row of choices where the overview has no apps yet: one way in,
 * wherever a newcomer is. A list of one kind offers only the ways to that kind.
 *
 * Large, the catalog leads: the easiest way in, as tall as the others stacked beside it,
 * with apps to install straight away. So the set reads as one block at every width, with
 * no card stretched past what it holds and none left alone on a row.
 */
export function StartChoices({ compact = false, ways }: { compact?: boolean; ways?: Way[] }) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  // A row of apps for each card beside the catalog's, less the row its own words take, so its height holds apps
  // rather than space; two rows on a phone, where nothing stands beside it. The last place says how many more.
  const beside = (ways ?? ["catalog", "git", "image", "compose", "website"]).length - 1;
  const places = ROW * (phone ? 2 : Math.max(beside - 1, 1));
  // The apps a newcomer most likely knows first, as the control plane orders them (ADR 0092).
  const featured = useCatalog({ pageSize: places, order: CatalogOrder.FEATURED }).data;
  const catalog = featured?.entries ?? [];
  const total = featured?.total ?? catalog.length;
  const shown = catalog.slice(0, total > places ? places - 1 : places);
  const more = total - shown.length;
  // Fewer than a row of tiles stand centred, each as wide as in a full row, rather than to one side.
  const tiles = shown.length + (more > 0 ? 1 : 0);
  const row = tiles < ROW ? ({ "--start-cols": tiles } as CSSProperties) : undefined;

  const all: Choice[] = [
    {
      key: "catalog",
      icon: "grid",
      title: t`From the catalog`,
      line: t`Databases, CMSs and tools, set up for you`,
      to: "/catalog",
      extra:
        shown.length > 0 && !compact ? (
          <span className="start-apps" style={row}>
            {/* Each straight into its install form in New app, which starts from its defaults (#617). */}
            {shown.map((e) => {
              const app = e.name;
              return (
                <Link
                  key={e.id}
                  to="/apps/new"
                  search={{ entry: e.id }}
                  className="start-app"
                  aria-label={t`Install ${app}`}
                  title={app}
                >
                  <AppIcon name={e.name} path={e.icon} />
                  <span className="start-app-name" aria-hidden="true">
                    {app}
                  </span>
                </Link>
              );
            })}
            {/* The rest are the card's own link, the catalog, so this only says how many. */}
            {more > 0 ? (
              <span className="start-app is-more" aria-hidden="true">
                <span className="start-more-count">+{more}</span>
                <span className="start-app-name">
                  {t({ message: "more", context: "apps in the catalog past those shown" })}
                </span>
              </span>
            ) : null}
            {more > 0 ? <span className="dw-sr">{t`and ${more} more`}</span> : null}
          </span>
        ) : null,
    },
    {
      key: "git",
      icon: "branch",
      title: t`A Git repository`,
      line: t`Built with its Dockerfile, or Railpack`,
      to: "/apps/new",
      search: { source: "git" },
    },
    {
      key: "image",
      icon: "service",
      title: t`A Docker image`,
      line: t`Any image from a registry`,
      to: "/apps/new",
      search: { source: "image" },
    },
    {
      key: "compose",
      icon: "layers",
      title: t`A Compose file`,
      line: t`Several containers as one stack`,
      to: "/apps/new",
      search: { source: "compose" },
    },
    {
      key: "website",
      icon: "globe",
      title: t`A website`,
      line: t`Static files or PHP, with HTTPS`,
      to: "/websites/new",
    },
  ];
  const choices = ways ? all.filter((c) => ways.includes(c.key)) : all;
  // The catalog leads when it is first and there are others to stack beside it.
  const lead = !compact && choices[0]?.key === "catalog" && choices.length > 1;
  const rest = { "--start-rest": choices.length - 1 } as CSSProperties;

  return (
    <ul
      className={cx("start-choices", compact && "is-compact", lead && "has-lead")}
      style={lead ? rest : undefined}
      aria-label={t`Ways to run your first app`}
    >
      {choices.map((c, i) => (
        <li key={c.key} className={cx("start-choice", lead && i === 0 && "is-lead")}>
          <span className="start-icon" aria-hidden="true">
            <Icon name={c.icon} size={compact ? 16 : 20} />
          </span>
          <span className="start-text">
            {/* The title is the link, and it stretches over the whole card. */}
            <Link to={c.to} search={c.search} className="start-title">
              {c.title}
            </Link>
            <span className="start-line">{c.line}</span>
          </span>
          {compact ? null : <Icon name="arrow-right" size={16} className="start-go" />}
          {c.extra}
        </li>
      ))}
    </ul>
  );
}

/** The start page's lead: what's ready, in a line. */
export function StartLead({ server }: { server: string }) {
  return server ? <Trans>{server} is ready. Pick what your first app comes from.</Trans> : null;
}
