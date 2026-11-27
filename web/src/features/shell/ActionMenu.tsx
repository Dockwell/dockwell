// SPDX-License-Identifier: AGPL-3.0-only

import { Button, cx, Icon, type IconName } from "@dockwell/ui";
import {
  type CSSProperties,
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import "./ActionMenu.css";

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
      <span key={action.id} role="menuitem" tabIndex={-1} aria-disabled="true" className={classes}>
        {body}
      </span>
    );
  if (action.href)
    return (
      <a
        key={action.id}
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
  const root = useRef<HTMLSpanElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const id = useId();
  // Where it opens, on the viewport, so a table's scrolling box doesn't cut it off (#798):
  // under the button, or over it where there's no room under it.
  const [at, setAt] = useState<CSSProperties>();
  // biome-ignore lint/correctness/useExhaustiveDependencies: A live eligibility reason changes the menu height.
  useLayoutEffect(() => {
    if (!open) {
      setAt(undefined);
      return;
    }
    const button = root.current?.querySelector("button")?.getBoundingClientRect();
    const box = menu.current?.getBoundingClientRect();
    if (!button || !box) return;
    const gutter = 16;
    const right = Math.max(gutter, Math.min(window.innerWidth - button.right, window.innerWidth - box.width - gutter));
    const below = button.bottom + 4 + box.height <= window.innerHeight || button.top - 4 - box.height < 0;
    setAt(below ? { top: button.bottom + 4, right } : { bottom: window.innerHeight - button.top + 4, right });
  }, [actions, open]);
  useEffect(() => {
    if (!open) return;
    root.current
      ?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])')
      ?.focus({ preventScroll: true });
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    // Placed on the viewport, it would stay put as the page moves its button away, so
    // it closes then, and only then: a scroll that leaves the button where it was is no
    // reason to.
    const button = () => root.current?.querySelector("button")?.getBoundingClientRect();
    const was = button();
    const moved = () => {
      const now = button();
      if (!was || !now || Math.abs(now.top - was.top) > 1 || Math.abs(now.left - was.left) > 1) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    window.addEventListener("scroll", moved, true);
    window.addEventListener("resize", moved);
    return () => {
      document.removeEventListener("pointerdown", away);
      window.removeEventListener("scroll", moved, true);
      window.removeEventListener("resize", moved);
    };
  }, [open]);
  if (actions.length === 0) return null;
  const close = () => {
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
  // Focus leaving the menu and its button, as a Tab past the last item, closes it.
  const left = (e: FocusEvent<HTMLDivElement>) => {
    if (!root.current?.contains(e.relatedTarget as Node | null)) setOpen(false);
  };
  const item = (action: MenuAction) => menuItem(action, () => setOpen(false));
  const triggerProps: ActionMenuTriggerProps = {
    "aria-label": label,
    "aria-haspopup": "menu",
    "aria-expanded": open,
    "aria-controls": open ? id : undefined,
    onClick: () => {
      if (!open) onOpen?.();
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
          onBlur={left}
        >
          {actions.map(item)}
        </div>
      ) : null}
    </span>
  );
}
