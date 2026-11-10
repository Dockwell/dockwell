// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon, type IconName } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import { AppIcon } from "../catalog/media";
import { useCatalog } from "../catalog/useCatalog";
import "./Start.css";

/** Entries a newcomer is most likely to know, shown on the catalog's choice when the catalog has them. */
const KNOWN = ["wordpress", "postgres", "nextcloud", "gitea", "n8n", "uptime-kuma", "ghost", "plausible"];

interface Choice {
  key: string;
  icon: IconName;
  title: string;
  line: string;
  to: "/catalog" | "/apps/new" | "/websites/new";
  search?: { source: "compose" };
  extra?: ReactNode;
}

/**
 * The ways a first app comes in, each opening its own create flow: the catalog, a
 * Docker image, a Compose file, a website. Large on the start page, a row of
 * choices where the overview has no apps yet: one way in, wherever a newcomer is.
 */
export function StartChoices({ compact = false }: { compact?: boolean }) {
  const { t } = useLingui();
  const catalog = useCatalog().data?.entries ?? [];
  const known = KNOWN.map((id) => catalog.find((e) => e.id === id)).filter((e) => e !== undefined);
  const shown = (known.length > 0 ? known : catalog).slice(0, 4);
  const more = catalog.length - shown.length;

  const choices: Choice[] = [
    {
      key: "catalog",
      icon: "grid",
      title: t`From the catalog`,
      line: t`Databases, CMSs and tools, set up for you`,
      to: "/catalog",
      extra:
        shown.length > 0 && !compact ? (
          <span className="start-apps">
            {/* Each straight into its install sheet, which starts from its defaults. */}
            {shown.map((e) => {
              const app = e.name;
              return (
                <Link
                  key={e.id}
                  to="/catalog/$entryId/install"
                  params={{ entryId: e.id }}
                  className="start-app"
                  aria-label={t`Install ${app}`}
                  title={app}
                >
                  <AppIcon name={e.name} path={e.icon} />
                </Link>
              );
            })}
            {more > 0 ? <span className="start-more">{t`and ${more} more`}</span> : null}
          </span>
        ) : null,
    },
    {
      key: "image",
      icon: "service",
      title: t`A Docker image`,
      line: t`Any image from a registry`,
      to: "/apps/new",
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

  return (
    <ul className={cx("start-choices", compact && "is-compact")} aria-label={t`Ways to run your first app`}>
      {choices.map((c) => (
        <li key={c.key} className="start-choice">
          <span className="start-icon" aria-hidden="true">
            <Icon name={c.icon} size={compact ? 16 : 20} />
          </span>
          <span className="start-text">
            {/* The title is the link, and it stretches over the whole card. */}
            <Link to={c.to} search={c.search} className="start-title">
              {c.title}
            </Link>
            <span className="start-line">{c.line}</span>
            {c.extra}
          </span>
          {compact ? null : <Icon name="arrow-right" size={16} className="start-go" />}
        </li>
      ))}
    </ul>
  );
}

/** The start page's lead: what's ready, in a line. */
export function StartLead({ server }: { server: string }) {
  return server ? <Trans>{server} is ready. Pick what your first app comes from.</Trans> : null;
}
