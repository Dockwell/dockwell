// SPDX-License-Identifier: AGPL-3.0-only

import { Button, cx, Icon, type IconName } from "@dockwell/ui";
import { type FocusEvent, type KeyboardEvent, type ReactNode, useEffect, useId, useRef, useState } from "react";

import "./ActionMenu.css";

/** One thing a menu does: a step on this page, or a link out. */
export type MenuAction = {
  id: string;
  label: ReactNode;
  icon?: IconName;
  /** Removes or ends something, so it reads as a warning. */
  danger?: boolean;
  /** Why it can't be done, said under its name, as a phone has no hover (ADR 0059). */
  off?: string | undefined;
  /** Can't be done now, with nothing to say, such as while something loads. */
  disabled?: boolean;
} & ({ onSelect: () => void; href?: undefined } | { href: string; onSelect?: undefined });

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
}: {
  /** What the button is called for a screen reader, such as "Actions for git.example.com". */
  label: string;
  actions: readonly MenuAction[];
  icon?: IconName;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLSpanElement>(null);
  const id = useId();
  useEffect(() => {
    if (!open) return;
    root.current?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])')?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
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
  const item = (action: MenuAction) => {
    const off = action.off || action.disabled;
    const body = (
      <>
        {action.icon ? <Icon name={action.icon} size={16} /> : null}
        <span className="action-menu-words">
          <span>{action.label}</span>
          {action.off ? <span className="action-menu-why">{action.off}</span> : null}
        </span>
      </>
    );
    const classes = cx("action-menu-item", action.danger && "is-danger", off && "is-off");
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
          onClick={() => setOpen(false)}
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
          setOpen(false);
          action.onSelect?.();
        }}
      >
        {body}
      </button>
    );
  };
  return (
    <span className={cx("action-menu-anchor", className)} ref={root}>
      <Button
        variant="ghost"
        size="sm"
        icon={icon}
        className="action-menu-button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => setOpen(!open)}
      />
      {open ? (
        <div id={id} className="action-menu" role="menu" aria-label={label} onKeyDown={keys} onBlur={left}>
          {actions.map(item)}
        </div>
      ) : null}
    </span>
  );
}
