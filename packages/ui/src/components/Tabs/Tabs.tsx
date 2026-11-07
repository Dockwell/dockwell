// SPDX-License-Identifier: AGPL-3.0-only

import {
  type AnchorHTMLAttributes,
  type ReactNode,
  type Ref,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { cx } from "../../lib/cx";
import { handleRovingKeyDown, tabStop } from "../../lib/roving";
import { useControlled } from "../../lib/useControlled";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./Tabs.css";

/**
 * Says which edges of a list that scrolls sideways have more past them, for
 * its fade. Nothing at all when it all fits.
 */
function useMoreEdges<E extends HTMLElement>() {
  const ref = useRef<E>(null);
  const [more, setMore] = useState<"start" | "end" | "both" | undefined>();
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => {
      const start = el.scrollLeft > 1;
      const end = el.scrollLeft + el.clientWidth < el.scrollWidth - 1;
      setMore(start && end ? "both" : start ? "start" : end ? "end" : undefined);
    };
    measure();
    el.addEventListener("scroll", measure, { passive: true });
    const seen = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    seen?.observe(el);
    return () => {
      el.removeEventListener("scroll", measure);
      seen?.disconnect();
    };
  }, []);
  return { ref, more };
}

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
  const { ref, more } = useMoreEdges<HTMLDivElement>();
  return (
    <div
      ref={ref}
      data-more={more}
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
  const { ref, more } = useMoreEdges<HTMLElement>();
  // Where the list scrolls, as on a phone, the page you're on is in sight when you land on it.
  // Only the list moves: scrollIntoView would move the page to it too.
  const shown = useRef<Element | null>(null);
  useLayoutEffect(() => {
    const nav = ref.current;
    const on = nav?.querySelector('[aria-current="page"]');
    if (!nav || !on || on === shown.current) return;
    shown.current = on;
    const box = on.getBoundingClientRect();
    const edge = nav.getBoundingClientRect();
    const room = 32;
    if (box.right > edge.right) nav.scrollLeft += box.right - edge.right + room;
    else if (box.left < edge.left) nav.scrollLeft -= edge.left - box.left + room;
  });
  return (
    <nav ref={ref} data-more={more} className={cx("dw-tabs", className)} aria-label={label}>
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
      {/* The space keeps the name "Deployments 128" where styles don't separate the two. */}
      {count != null ? (
        <>
          {" "}
          <span className={cx("dw-tab-count", countTone && `dw-tone-${countTone}`)}>{count}</span>
        </>
      ) : null}
    </a>
  );
}
