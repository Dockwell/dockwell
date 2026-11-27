// SPDX-License-Identifier: AGPL-3.0-only

import { Button, cx, Icon, type IconName } from "@dockwell/ui";
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

import "./ActionMenu.css";

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
  if (!list.hasAttribute("tabindex") && list.tabIndex < 0) list.setAttribute("tabindex", "-1");
  list.focus({ preventScroll: true });
}

/** True when an element has measured dimensions and sits completely outside the viewport or a clipped ancestor. */
export function isOutOfView(rect: DOMRect, el: HTMLElement): boolean {
  if (rect.width === 0 && rect.height === 0) return false;
  if (rect.bottom <= 0 || rect.top >= window.innerHeight || rect.right <= 0 || rect.left >= window.innerWidth) {
    return true;
  }
  for (let parent = el.parentElement; parent && parent !== document.body; parent = parent.parentElement) {
    const style = getComputedStyle(parent);
    if (!/(auto|scroll|hidden|clip)/.test(`${style.overflowX} ${style.overflowY}`)) continue;
    const clip = parent.getBoundingClientRect();
    if (clip.width === 0 && clip.height === 0) continue;
    if (rect.bottom <= clip.top || rect.top >= clip.bottom || rect.right <= clip.left || rect.left >= clip.right) {
      return true;
    }
  }
  return false;
}

/** Finds a menu item by its action ID, or falls back to the first enabled item. */
export function focusMenuItem(container: HTMLElement | null, actionId: string | null): void {
  if (!container) return;
  const items = [...container.querySelectorAll<HTMLElement>('[role="menuitem"]')];
  const same = actionId ? items.find((item) => item.dataset["actionId"] === actionId) : undefined;
  const target = same ?? items.find((item) => item.getAttribute("aria-disabled") !== "true") ?? items[0];
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
  const [open, setOpen] = useState(false);
  const openRef = useRef(false);
  const root = useRef<HTMLSpanElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLElement | null>(null);
  const focusedAction = useRef<string | null>(null);
  const id = useId();
  // Where it opens, on the viewport, so a table's scrolling box doesn't cut it off (#798):
  // under the button, or over it where there's no room under it.
  const [at, setAt] = useState<CSSProperties>();
  const place = useCallback(() => {
    const button = root.current?.querySelector("button")?.getBoundingClientRect();
    const box = menu.current?.getBoundingClientRect();
    if (!button || !box) return;
    const gutter = 16;
    const right = Math.max(gutter, Math.min(window.innerWidth - button.right, window.innerWidth - box.width - gutter));
    const below = button.bottom + 4 + box.height <= window.innerHeight || button.top - 4 - box.height < 0;
    const next: CSSProperties = below
      ? { top: button.bottom + 4, right }
      : { bottom: window.innerHeight - button.top + 4, right };
    setAt((was) =>
      was && was.top === next.top && was.bottom === next.bottom && was.right === next.right ? was : next,
    );
  }, []);
  useLayoutEffect(() => {
    openRef.current = open;
    if (!open) {
      setAt(undefined);
      return;
    }
    if (actions.length === 0) {
      openRef.current = false;
      setOpen(false);
      const button = root.current?.querySelector<HTMLButtonElement>("button");
      if (button?.isConnected) button.focus({ preventScroll: true });
      else focusList(listRef.current);
      return;
    }
    place();
    // If a refresh replaced or removed the focused menu item, keep focus inside the open menu.
    const el = menu.current;
    if (
      el &&
      !el.contains(document.activeElement) &&
      (document.activeElement === document.body || document.activeElement === null)
    ) {
      focusMenuItem(el, focusedAction.current);
    }
  }, [actions, open, place]);
  useEffect(() => {
    if (!open) {
      focusedAction.current = null;
      return;
    }
    listRef.current = listContainerOf(root.current);
    focusMenuItem(menu.current ?? root.current, focusedAction.current);
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) {
        openRef.current = false;
        setOpen(false);
      }
    };
    // Follow the button across layout shifts and minor scrolls while it stays in view;
    // close only once the button scrolls completely out of view or disconnects.
    const moved = () => {
      const buttonEl = root.current?.querySelector("button");
      if (!buttonEl?.isConnected || isOutOfView(buttonEl.getBoundingClientRect(), buttonEl)) {
        openRef.current = false;
        setOpen(false);
        return;
      }
      place();
    };
    document.addEventListener("pointerdown", away);
    window.addEventListener("scroll", moved, true);
    window.addEventListener("resize", moved);
    return () => {
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
  if (actions.length === 0) return null;
  const close = () => {
    openRef.current = false;
    setOpen(false);
    root.current?.querySelector<HTMLButtonElement>("button")?.focus();
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
    const actionId = (e.target as HTMLElement | null)?.closest<HTMLElement>("[data-action-id]")?.dataset["actionId"];
    if (actionId) focusedAction.current = actionId;
  };
  // Focus leaving the menu and its button, as a Tab past the last item, closes it.
  // An item unmounting during a refresh must not close the menu.
  const left = (e: FocusEvent<HTMLDivElement>) => {
    if (!e.target.isConnected) return;
    if (!root.current?.contains(e.relatedTarget as Node | null)) {
      openRef.current = false;
      setOpen(false);
    }
  };
  const item = (action: MenuAction) =>
    menuItem(action, () => {
      openRef.current = false;
      setOpen(false);
    });
  const triggerProps: ActionMenuTriggerProps = {
    "aria-label": label,
    "aria-haspopup": "menu",
    "aria-expanded": open,
    "aria-controls": open ? id : undefined,
    onClick: () => {
      if (!open) onOpen?.();
      openRef.current = !open;
      setOpen(!open);
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
          {actions.map(item)}
        </div>
      ) : null}
    </span>
  );
}
