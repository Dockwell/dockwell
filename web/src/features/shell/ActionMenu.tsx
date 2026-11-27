// SPDX-License-Identifier: AGPL-3.0-only

import { Button, cx, Icon, type IconName } from "@dockwell/ui";
import { msg } from "@lingui/core/macro";
import {
  type CSSProperties,
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { i18n } from "../../app/i18n";
import "./ActionMenu.css";

const UNAVAILABLE_FALLBACK = msg`No longer available`;

/**
 * Holds an open menu's slots and verbs steady across live refreshes (#956):
 * an item whose verb still applies keeps its latest details, one that no longer
 * applies turns disabled in place with its reason, and newly applicable verbs
 * wait until the menu opens again.
 */
export function stabilizeMenuActions(opened: readonly MenuAction[], latest: readonly MenuAction[]): MenuAction[] {
  const byId = new Map(latest.map((action) => [action.id, action]));
  return opened.map((initial) => {
    const current = byId.get(initial.id);
    if (current) return current;
    const reason = initial.off ?? initial.unavailable ?? i18n._(UNAVAILABLE_FALLBACK);
    return { ...initial, off: reason, disabled: true };
  });
}

/** The nearest focusable list region holding an element, to take focus back when its row goes away. */
export function listContainerOf(el: HTMLElement | null): HTMLElement | null {
  return (
    el?.closest<HTMLElement>('section[tabindex="-1"], [role="region"][tabindex]') ??
    el?.closest<HTMLElement>('.dw-table-wrap, [role="list"], ul, ol') ??
    null
  );
}

/** Moves focus cleanly back to a list container without scrolling the page. */
export function focusList(list: HTMLElement | null): void {
  if (!list?.isConnected) return;
  if (!list.hasAttribute("tabindex") && list.tabIndex < 0) {
    list.setAttribute("tabindex", "-1");
    list.addEventListener(
      "blur",
      () => {
        if (list.getAttribute("tabindex") === "-1") list.removeAttribute("tabindex");
      },
      { once: true },
    );
  }
  list.focus({ preventScroll: true });
}

/** Scroll-clipping ancestors and sticky chrome bars cached once when a menu opens. */
export interface MenuClips {
  ancestors: HTMLElement[];
  topBars: HTMLElement[];
  bottomBars: HTMLElement[];
  thead: HTMLElement | null;
}

/** The visible region for an open menu's anchor and fixed viewport placement, excluding sticky chrome. */
export interface VisibleBounds {
  /** Anchor visibility bounds (viewport minus sticky chrome, clipping ancestors, and sticky table header). */
  top: number;
  bottom: number;
  left: number;
  right: number;
  /** Fixed menu viewport bounds (viewport minus sticky page top/bottom chrome, never clipped by .dw-table-wrap). */
  viewportTop: number;
  viewportBottom: number;
  /** Sticky chrome edges that close the menu if the anchor slides behind them. */
  stickyTop: number;
  stickyBottom: number;
}

/** Collects overflow-clipping ancestors and sticky chrome bars once on open so scroll frames avoid style walks. */
export function collectMenuClips(el: HTMLElement | null): MenuClips {
  const ancestors: HTMLElement[] = [];
  for (let parent = el?.parentElement ?? null; parent && parent !== document.body; parent = parent.parentElement) {
    const style = getComputedStyle(parent);
    if (/(auto|scroll|hidden|clip)/.test(`${style.overflowX} ${style.overflowY}`)) {
      ancestors.push(parent);
    }
  }
  const notContaining = (bar: HTMLElement) => !el || !bar.contains(el);
  const topBars = [...document.querySelectorAll<HTMLElement>(".topbar, .mobile-header")].filter(notContaining);
  const bottomBars = [...document.querySelectorAll<HTMLElement>(".tabbar, .list-batch-bar")].filter(notContaining);
  const tbody = el?.closest("tbody");
  const thead = tbody ? (tbody.closest("table")?.querySelector<HTMLElement>("thead") ?? null) : null;
  return { ancestors, topBars, bottomBars, thead };
}

/** Computes the visible anchor rect and fixed-menu viewport bounds minus sticky chrome and cached clips. */
export function visibleBounds(clips: MenuClips | null): VisibleBounds {
  let top = 0;
  let bottom = window.innerHeight;
  let left = 0;
  let right = window.innerWidth;
  let viewportTop = 0;
  let viewportBottom = window.innerHeight;
  let stickyTop = 0;
  let stickyBottom = window.innerHeight;
  if (!clips) return { top, bottom, left, right, viewportTop, viewportBottom, stickyTop, stickyBottom };

  for (const bar of clips.topBars) {
    if (!bar.isConnected) continue;
    const rect = bar.getBoundingClientRect();
    if (rect.width > 0 && rect.height > 0 && rect.bottom > top && rect.top <= 4) {
      top = Math.max(top, rect.bottom);
      viewportTop = Math.max(viewportTop, rect.bottom);
      stickyTop = Math.max(stickyTop, rect.bottom);
    }
  }
  for (const bar of clips.bottomBars) {
    if (!bar.isConnected) continue;
    const rect = bar.getBoundingClientRect();
    if (rect.width > 0 && rect.height > 0 && rect.top < bottom && rect.bottom >= window.innerHeight - 40) {
      bottom = Math.min(bottom, rect.top);
      viewportBottom = Math.min(viewportBottom, rect.top);
      stickyBottom = Math.min(stickyBottom, rect.top);
    }
  }
  for (const parent of clips.ancestors) {
    if (!parent.isConnected) continue;
    const clip = parent.getBoundingClientRect();
    if (clip.width === 0 && clip.height === 0) continue;
    top = Math.max(top, clip.top);
    bottom = Math.min(bottom, clip.bottom);
    left = Math.max(left, clip.left);
    right = Math.min(right, clip.right);
  }
  if (clips.thead?.isConnected) {
    const rect = clips.thead.getBoundingClientRect();
    if (rect.width > 0 && rect.height > 0 && rect.bottom > top && rect.top <= top + 4) {
      top = Math.max(top, rect.bottom);
      stickyTop = Math.max(stickyTop, rect.bottom);
    }
  }
  return { top, bottom, left, right, viewportTop, viewportBottom, stickyTop, stickyBottom };
}

/** True when an anchor has measured dimensions and has moved outside the visible bounds or under sticky chrome. */
export function isAnchorOutOfView(rect: DOMRect, bounds: VisibleBounds): boolean {
  if (rect.width === 0 && rect.height === 0) return false;
  if (bounds.bottom <= bounds.top || bounds.right <= bounds.left) return true;
  if (
    rect.bottom <= bounds.top ||
    rect.top >= bounds.bottom ||
    rect.right <= bounds.left ||
    rect.left >= bounds.right
  ) {
    return true;
  }
  if (bounds.stickyTop > 0 && rect.top < bounds.stickyTop) return true;
  if (bounds.stickyBottom < window.innerHeight && rect.bottom > bounds.stickyBottom) return true;
  return false;
}

/** Observes layout shifts on the anchor, list, and page main container while a menu is open. */
export function observeLayoutShift(
  anchor: HTMLElement | null,
  list: HTMLElement | null,
  onChange: () => void,
): () => void {
  if (typeof ResizeObserver === "undefined") return () => {};
  const observer = new ResizeObserver(onChange);
  const targets = new Set<HTMLElement>();
  for (const el of [
    anchor,
    anchor?.parentElement ?? null,
    list,
    document.querySelector<HTMLElement>("main") ?? document.body,
  ]) {
    if (el?.isConnected) targets.add(el);
  }
  for (const el of targets) observer.observe(el);
  return () => observer.disconnect();
}

/** Finds a menu item by its action ID, or falls back to the same index, then the first enabled item. */
export function focusMenuItem(container: HTMLElement | null, actionId: string | null, fallbackIndex?: number): void {
  if (!container) return;
  const items = [...container.querySelectorAll<HTMLElement>('[role="menuitem"]')];
  if (items.length === 0) return;
  const same = actionId ? items.find((item) => item.dataset["actionId"] === actionId) : undefined;
  const atIndex =
    !same && fallbackIndex !== undefined && fallbackIndex >= 0
      ? items[Math.min(fallbackIndex, items.length - 1)]
      : undefined;
  const target = same ?? atIndex ?? items.find((item) => item.getAttribute("aria-disabled") !== "true") ?? items[0];
  target?.focus({ preventScroll: true });
}

/** One menu item, as ActionMenu and a row's right-click menu both draw it. */
export function menuItem(action: MenuAction, onDone: () => void) {
  const off = action.off || action.disabled;
  const body = (
    <>
      {action.icon ? <Icon name={action.icon} size={16} /> : null}
      <span className="action-menu-words">
        <span>{action.label}</span>
        {action.off ? <span className="action-menu-why">{action.off}</span> : null}
      </span>
      {action.badge}
    </>
  );
  const classes = cx("action-menu-item", action.danger && "is-danger", action.current && "is-current", off && "is-off");
  if (off)
    return (
      <span
        key={action.id}
        data-action-id={action.id}
        role="menuitem"
        tabIndex={-1}
        aria-disabled="true"
        className={classes}
      >
        {body}
      </span>
    );
  if (action.href)
    return (
      <a
        key={action.id}
        data-action-id={action.id}
        role="menuitem"
        className={classes}
        href={action.href}
        target="_blank"
        rel="noreferrer"
        onClick={onDone}
      >
        {body}
      </a>
    );
  return (
    <button
      key={action.id}
      data-action-id={action.id}
      type="button"
      role="menuitem"
      className={classes}
      onClick={() => {
        onDone();
        action.onSelect?.();
      }}
    >
      {body}
    </button>
  );
}

/** One thing a menu does: a step on this page, or a link out. */
export type MenuAction = {
  id: string;
  label: ReactNode;
  icon?: IconName;
  /** Removes or ends something, so it reads as a warning. */
  danger?: boolean;
  /** Marks the currently active navigation item inside a menu. */
  current?: boolean;
  /** Optional trailing badge or count aligned to the item's far edge. */
  badge?: ReactNode;
  /** Why it can't be done, said under its name, as a phone has no hover (ADR 0059). */
  off?: string | undefined;
  /** Why it can't be done if a live refresh makes it inapplicable while the menu is already open (#956). */
  unavailable?: string | undefined;
  /** Can't be done now, with nothing to say, such as while something loads. */
  disabled?: boolean;
} & ({ onSelect: () => void; href?: undefined } | { href: string; onSelect?: undefined });

export interface ActionMenuTriggerProps {
  "aria-label": string;
  "aria-haspopup": "menu";
  "aria-expanded": boolean;
  "aria-controls": string | undefined;
  onClick: () => void;
}

/**
 * A row's or a header's actions behind one button, where a phone has no room for
 * them side by side (#795): each item a step, or a link out in a new tab. One that
 * can't be done stays, off, with why under its name. Arrows move, Escape closes and
 * goes back to the button, as the top bar's New does.
 */
export function ActionMenu({
  label,
  actions,
  icon = "more",
  className,
  text,
  trigger,
  onOpen,
}: {
  /** What the button is called for a screen reader, such as "Actions for git.example.com". */
  label: string;
  actions: readonly MenuAction[];
  icon?: IconName;
  className?: string;
  text?: ReactNode;
  /** Optional custom trigger button (e.g. a tab trigger) keeping ActionMenu's positioning and keyboard handling. */
  trigger?: ((props: ActionMenuTriggerProps) => ReactNode) | undefined;
  onOpen?: (() => void) | undefined;
}) {
  const [openedActions, setOpenedActions] = useState<readonly MenuAction[] | null>(null);
  const open = openedActions !== null;
  const openRef = useRef(false);
  const root = useRef<HTMLSpanElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLElement | null>(null);
  const clipsRef = useRef<MenuClips | null>(null);
  const focusedAction = useRef<string | null>(null);
  const focusedIndex = useRef<number | undefined>(undefined);
  const id = useId();
  const shown = openedActions ? stabilizeMenuActions(openedActions, actions) : actions;
  // Where it opens, on the viewport, so a table's scrolling box doesn't cut it off (#798):
  // under the button, or over it where there's no room under it, clamped inside sticky chrome.
  const [at, setAt] = useState<CSSProperties>();
  const place = useCallback(() => {
    const button = root.current?.querySelector("button")?.getBoundingClientRect();
    const box = menu.current?.getBoundingClientRect();
    if (!button || !box) return;
    const bounds = visibleBounds(clipsRef.current);
    const gutter = 16;
    const right = Math.max(gutter, Math.min(window.innerWidth - button.right, window.innerWidth - box.width - gutter));
    const fitsBelow = button.bottom + 4 + box.height <= bounds.viewportBottom;
    const fitsAbove = button.top - 4 - box.height >= bounds.viewportTop;
    let next: CSSProperties;
    if (fitsBelow) {
      next = { top: Math.max(bounds.viewportTop + 4, button.bottom + 4), right };
    } else if (fitsAbove) {
      next = {
        bottom: Math.max(window.innerHeight - bounds.viewportBottom + 4, window.innerHeight - button.top + 4),
        right,
      };
    } else {
      const clampedTop = Math.max(
        bounds.viewportTop + 4,
        Math.min(button.bottom + 4, bounds.viewportBottom - box.height - 4),
      );
      next = { top: clampedTop, right };
    }
    setAt((was) =>
      was && was.top === next.top && was.bottom === next.bottom && was.right === next.right ? was : next,
    );
  }, []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: Re-measure and preserve focus when live actions update while open.
  useLayoutEffect(() => {
    openRef.current = open;
    if (!open) {
      setAt(undefined);
      return;
    }
    if (!clipsRef.current) clipsRef.current = collectMenuClips(root.current);
    place();
    // If a refresh disabled an item (swapping <button> to <span>), keep focus on that exact item.
    const el = menu.current;
    if (
      el &&
      !el.contains(document.activeElement) &&
      (document.activeElement === document.body || document.activeElement === null)
    ) {
      focusMenuItem(el, focusedAction.current, focusedIndex.current);
    }
  }, [actions, open, place]);
  useEffect(() => {
    if (!open) {
      clipsRef.current = null;
      focusedAction.current = null;
      focusedIndex.current = undefined;
      return;
    }
    const buttonEl = root.current?.querySelector("button") ?? null;
    listRef.current = listContainerOf(root.current);
    clipsRef.current = collectMenuClips(buttonEl ?? root.current);
    place();
    focusMenuItem(menu.current ?? root.current, focusedAction.current, focusedIndex.current);
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) {
        openRef.current = false;
        setOpenedActions(null);
      }
    };
    // Follow the button across layout shifts and scrolls while it stays in view outside sticky chrome;
    // close once it disconnects, scrolls out of view, or goes under a sticky bar.
    const moved = () => {
      const currentButton = root.current?.querySelector("button");
      const bounds = visibleBounds(clipsRef.current);
      if (!currentButton?.isConnected || isAnchorOutOfView(currentButton.getBoundingClientRect(), bounds)) {
        openRef.current = false;
        setOpenedActions(null);
        return;
      }
      place();
    };
    const stopResize = observeLayoutShift(buttonEl, listRef.current, moved);
    document.addEventListener("pointerdown", away);
    window.addEventListener("scroll", moved, true);
    window.addEventListener("resize", moved);
    return () => {
      stopResize();
      document.removeEventListener("pointerdown", away);
      window.removeEventListener("scroll", moved, true);
      window.removeEventListener("resize", moved);
    };
  }, [open, place]);
  // If this row disappears while its menu is open, return focus cleanly to the list.
  useEffect(
    () => () => {
      if (!openRef.current) return;
      const list = listRef.current;
      if (!list?.isConnected) return;
      queueMicrotask(() => {
        if (!list.isConnected) return;
        const active = document.activeElement;
        if (active === null || active === document.body) focusList(list);
      });
    },
    [],
  );
  if (shown.length === 0 && !open) return null;
  const close = () => {
    openRef.current = false;
    setOpenedActions(null);
    root.current?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true });
  };
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(root.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      close();
    } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      items[(at + step + items.length) % items.length]?.focus();
    } else if (e.key === "Home" || e.key === "End") {
      e.preventDefault();
      items[e.key === "Home" ? 0 : items.length - 1]?.focus();
    }
  };
  const focused = (e: FocusEvent<HTMLDivElement>) => {
    const el = (e.target as HTMLElement | null)?.closest<HTMLElement>("[data-action-id]");
    const actionId = el?.dataset["actionId"];
    if (actionId) {
      focusedAction.current = actionId;
      const items = [...(menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
      const idx = items.indexOf(el);
      if (idx >= 0) focusedIndex.current = idx;
    }
  };
  // Focus leaving the menu and its button, as a Tab past the last item, closes it.
  // An item unmounting during a refresh must not close the menu.
  const left = (e: FocusEvent<HTMLDivElement>) => {
    if (!e.target.isConnected) return;
    if (!root.current?.contains(e.relatedTarget as Node | null)) {
      openRef.current = false;
      setOpenedActions(null);
    }
  };
  const item = (action: MenuAction) =>
    menuItem(action, () => {
      openRef.current = false;
      setOpenedActions(null);
      // Restore focus to the trigger button before onSelect opens any sheet so closing returns here.
      if (!action.href) {
        root.current?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true });
      }
    });
  const triggerProps: ActionMenuTriggerProps = {
    "aria-label": label,
    "aria-haspopup": "menu",
    "aria-expanded": open,
    "aria-controls": open ? id : undefined,
    onClick: () => {
      if (!open) {
        onOpen?.();
        openRef.current = true;
        setOpenedActions(actions);
      } else {
        openRef.current = false;
        setOpenedActions(null);
      }
    },
  };
  return (
    <span className={cx("action-menu-anchor", className)} ref={root}>
      {trigger ? (
        trigger(triggerProps)
      ) : (
        <Button
          variant={text ? "secondary" : "ghost"}
          size="sm"
          icon={icon}
          className={text ? undefined : "action-menu-button"}
          {...triggerProps}
        >
          {text}
        </Button>
      )}
      {open ? (
        <div
          id={id}
          ref={menu}
          className={cx("action-menu", at && "is-placed")}
          style={at}
          role="menu"
          aria-label={label}
          onKeyDown={keys}
          onFocus={focused}
          onBlur={left}
        >
          {shown.map(item)}
        </div>
      ) : null}
    </span>
  );
}
