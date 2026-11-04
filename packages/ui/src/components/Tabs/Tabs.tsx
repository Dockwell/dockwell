// SPDX-License-Identifier: AGPL-3.0-only

import type { AnchorHTMLAttributes, ReactNode, Ref } from "react";

import { cx } from "../../lib/cx";
import { handleRovingKeyDown, tabStop } from "../../lib/roving";
import { useControlled } from "../../lib/useControlled";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./Tabs.css";

export interface TabItem<Id extends string = string> {
  id: Id;
  /** One or two words. */
  label: ReactNode;
  icon?: IconName;
  /** A number only. */
  count?: number;
  /** Only when the count is a problem, such as open alerts; never for totals. */
  countTone?: "warning" | "critical" | "cobalt";
  /** The id of the tab panel this tab shows, for aria-controls. */
  controls?: string;
}

export interface TabsProps<Id extends string = string> {
  tabs: readonly TabItem<Id>[];
  value?: Id;
  /** Defaults to the first tab, which also stands in while this one isn't among the tabs. */
  defaultValue?: Id;
  onChange?: (id: Id) => void;
  /** Accessible name of the tab list. */
  label?: string;
  className?: string;
}

/**
 * Moves between sections of one resource, such as an app's overview,
 * deployments, logs and settings. A tab list: arrow keys move between tabs
 * and select them, and only the selected tab is in the tab order.
 */
export function Tabs<Id extends string = string>({
  tabs,
  value,
  defaultValue,
  onChange,
  label,
  className,
}: TabsProps<Id>) {
  const [stored, setCurrent] = useControlled<Id | undefined, Id>(value, defaultValue, onChange);
  const ids = tabs.map((tab) => tab.id);
  // Uncontrolled, the first tab stands in while the stored one isn't among the tabs, as when they load after mount.
  const current = value !== undefined ? value : tabStop(ids, stored);
  const stop = tabStop(ids, current);
  return (
    <div
      className={cx("dw-tabs", className)}
      role="tablist"
      aria-label={label}
      onKeyDown={(event) => handleRovingKeyDown(event, ids, current, setCurrent)}
    >
      {tabs.map((tab) => {
        const on = tab.id === current;
        return (
          <button
            key={tab.id}
            type="button"
            role="tab"
            aria-selected={on}
            aria-controls={tab.controls}
            tabIndex={tab.id === stop ? 0 : -1}
            className="dw-tab"
            data-roving-item=""
            onClick={() => {
              if (!on) setCurrent(tab.id);
            }}
          >
            {tab.icon ? <Icon name={tab.icon} size={15} /> : null}
            <span>{tab.label}</span>
            {tab.count != null ? (
              <span className={cx("dw-tab-count", tab.countTone && `dw-tone-${tab.countTone}`)}>{tab.count}</span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}

export interface TabNavProps {
  /** Names the navigation for assistive technology. */
  label: string;
  /** TabLinks. */
  children: ReactNode;
  className?: string;
}

/**
 * Tabs that are places: each one is a page with its own URL, so they are
 * links in a navigation landmark rather than a tab list. It looks the same
 * as Tabs. For a view inside one page, use Tabs.
 */
export function TabNav({ label, children, className }: TabNavProps) {
  return (
    <nav className={cx("dw-tabs", className)} aria-label={label}>
      {children}
    </nav>
  );
}

export interface TabLinkProps extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "children"> {
  /** One or two words. */
  children: ReactNode;
  /** A number only. */
  count?: number | undefined;
  /** Only when the count is a problem, such as open alerts; never for totals. */
  countTone?: "warning" | "critical" | "cobalt" | undefined;
  ref?: Ref<HTMLAnchorElement>;
}

/**
 * One tab of a TabNav. Inside the panel, wrap it with the router's link
 * factory, such as TanStack Router's createLink, so it navigates without a
 * reload and sets aria-current on the page it names.
 */
export function TabLink({ children, count, countTone, className, ...rest }: TabLinkProps) {
  return (
    <a {...rest} className={cx("dw-tab", className)}>
      <span>{children}</span>
      {count != null ? <span className={cx("dw-tab-count", countTone && `dw-tone-${countTone}`)}>{count}</span> : null}
    </a>
  );
}
