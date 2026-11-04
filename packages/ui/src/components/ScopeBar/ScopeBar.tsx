// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment } from "react";

import { cx } from "../../lib/cx";
import { useUIStrings } from "../../lib/strings";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./ScopeBar.css";

export type ScopeLevel = "org" | "project" | "environment";

export interface ScopeBarProps {
  org?: string;
  project?: string;
  environment?: string;
  /** Shows a lock after the environment when changes need approval. */
  protected?: boolean;
  /** Opens the switcher for a level. Without it the levels are plain text. */
  onSelect?: (level: ScopeLevel) => void;
  className?: string;
}

const LEVELS: readonly { key: ScopeLevel; icon: IconName }[] = [
  { key: "org", icon: "org" },
  { key: "project", icon: "folder" },
  { key: "environment", icon: "layers" },
];

/**
 * Shows and switches where every action on the page applies: organization,
 * project, environment. Never hide a level to save space; the project name
 * truncates instead.
 */
export function ScopeBar({ org, project, environment, protected: isProtected, onSelect, className }: ScopeBarProps) {
  const strings = useUIStrings();
  const names: Record<ScopeLevel, string | undefined> = { org, project, environment };
  const parts = LEVELS.filter((level) => names[level.key]);
  return (
    <nav className={cx("dw-scope", className)} aria-label={strings.scopeBar.label}>
      {parts.map((level, i) => {
        const current = i === parts.length - 1;
        const content = (
          <>
            <Icon name={level.icon} size={15} />
            <span className="dw-scope-name">{names[level.key]}</span>
            {level.key === "environment" && isProtected ? (
              <>
                {/* Keeps the name and the lock apart when read together. Flex layout ignores it. */}{" "}
                <Icon name="lock" size={12} label={strings.scopeBar.protectedEnvironment} className="dw-scope-lock" />
              </>
            ) : null}
            {onSelect ? <Icon name="chevron-up-down" size={12} className="dw-scope-caret" /> : null}
          </>
        );
        const itemClass = cx(
          "dw-scope-item",
          `dw-scope-${level.key}`,
          current && "is-current",
          !onSelect && "is-static",
        );
        return (
          <Fragment key={level.key}>
            {i > 0 ? (
              <span className="dw-scope-sep" aria-hidden="true">
                /
              </span>
            ) : null}
            {onSelect ? (
              <button
                type="button"
                className={itemClass}
                aria-current={current ? "page" : undefined}
                onClick={() => onSelect(level.key)}
              >
                {content}
              </button>
            ) : (
              <span className={itemClass} aria-current={current ? "page" : undefined}>
                {content}
              </span>
            )}
          </Fragment>
        );
      })}
    </nav>
  );
}
