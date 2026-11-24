// SPDX-License-Identifier: AGPL-3.0-only

import {
  type CSSProperties,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { cx } from "../../lib/cx";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./ContextMenu.css";

/** One entry in a ContextMenu: an action item or a dividing rule. */
export type ContextMenuItem =
  | {
      kind: "separator";
      id: string;
    }
  | {
      kind?: "item";
      id: string;
      label: ReactNode;
      icon?: IconName;
      /** Keyboard hint shown quietly on the right, such as "F2". */
      shortcut?: string;
      /** Destructive action, drawn in critical colour and placed last. */
      danger?: boolean;
      disabled?: boolean;
      href?: string;
      download?: boolean | string;
      onSelect?: () => void;
    };

export interface ContextMenuProps {
  open: boolean;
  /** Accessible name for the menu. */
  label: string;
  items: readonly ContextMenuItem[];
  /** Viewport coordinates for a pointer context menu. */
  position?: { x: number; y: number } | undefined;
  /** Element to anchor under/over when opened from a button or keyboard. */
  anchor?: HTMLElement | null | undefined;
  onClose: () => void;
  className?: string;
}

const GUTTER = 8;

/**
 * A floating menu opened by right-click, a row's More button, or the keyboard
 * (Shift+F10 / ContextMenu key). Clamps to the viewport, cycles enabled items
 * with arrow keys, and closes on Escape, outside pointer click, or scroll.
 */
export function ContextMenu({ open, label, items, position, anchor, onClose, className }: ContextMenuProps) {
  const menuRef = useRef<HTMLDivElement>(null);
  const previousFocus = useRef<HTMLElement | null>(null);
  const [style, setStyle] = useState<CSSProperties | undefined>(undefined);

  useLayoutEffect(() => {
    if (!open) {
      setStyle(undefined);
      return;
    }
    const box = menuRef.current?.getBoundingClientRect();
    if (!box) return;
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    let left: number;
    let top: number;
    if (anchor) {
      const rect = anchor.getBoundingClientRect();
      left = rect.right - box.width;
      if (left < GUTTER) left = rect.left;
      left = Math.max(GUTTER, Math.min(left, vw - box.width - GUTTER));
      const below = rect.bottom + 4;
      const above = rect.top - box.height - 4;
      top = below + box.height <= vh - GUTTER || above < GUTTER ? below : above;
      top = Math.max(GUTTER, Math.min(top, vh - box.height - GUTTER));
    } else if (position) {
      left = position.x + box.width <= vw - GUTTER ? position.x : position.x - box.width;
      top = position.y + box.height <= vh - GUTTER ? position.y : position.y - box.height;
      left = Math.max(GUTTER, Math.min(left, vw - box.width - GUTTER));
      top = Math.max(GUTTER, Math.min(top, vh - box.height - GUTTER));
    } else {
      left = GUTTER;
      top = GUTTER;
    }
    setStyle({ top, left });
  }, [open, position, anchor]);

  useEffect(() => {
    if (!open) return;
    previousFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const first = menuRef.current?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])');
    first?.focus({ preventScroll: true });

    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null;
      if (menuRef.current?.contains(target)) return;
      if (anchor?.contains(target)) return;
      onClose();
    };
    const onScroll = (event: Event) => {
      if (menuRef.current?.contains(event.target as Node | null)) return;
      onClose();
    };
    const onResize = () => onClose();
    document.addEventListener("pointerdown", onPointerDown, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onResize);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onResize);
    };
  }, [open, anchor, onClose]);

  if (!open || items.length === 0) return null;

  const restoreFocus = () => {
    const target = anchor?.isConnected ? anchor : previousFocus.current;
    if (target?.isConnected) target.focus({ preventScroll: true });
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const enabled = [
      ...(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]:not([aria-disabled="true"])') ?? []),
    ];
    const index = enabled.indexOf(document.activeElement as HTMLElement);
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      onClose();
      restoreFocus();
    } else if (event.key === "Tab") {
      onClose();
    } else if (event.key === "ArrowDown") {
      event.preventDefault();
      if (enabled.length > 0) enabled[(index + 1 + enabled.length) % enabled.length]?.focus();
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      if (enabled.length > 0) enabled[(index - 1 + enabled.length) % enabled.length]?.focus();
    } else if (event.key === "Home") {
      event.preventDefault();
      enabled[0]?.focus();
    } else if (event.key === "End") {
      event.preventDefault();
      enabled[enabled.length - 1]?.focus();
    }
  };

  const content = (
    <div
      ref={menuRef}
      role="menu"
      aria-label={label}
      className={cx("dw-context-menu", className)}
      style={style ?? (position ? { top: position.y, left: position.x } : undefined)}
      onKeyDown={onKeyDown}
    >
      {items.map((item) => {
        if (item.kind === "separator") {
          return <hr key={item.id} className="dw-context-menu-sep" />;
        }
        const classes = cx("dw-context-menu-item", item.danger && "is-danger", item.disabled && "is-disabled");
        const inner = (
          <>
            <span className="dw-context-menu-icon" aria-hidden="true">
              {item.icon ? <Icon name={item.icon} size={15} /> : null}
            </span>
            <span className="dw-context-menu-label">{item.label}</span>
            {item.shortcut ? <span className="dw-context-menu-shortcut">{item.shortcut}</span> : null}
          </>
        );
        if (item.disabled) {
          return (
            <span key={item.id} role="menuitem" tabIndex={-1} aria-disabled="true" className={classes}>
              {inner}
            </span>
          );
        }
        if (item.href) {
          return (
            <a
              key={item.id}
              role="menuitem"
              tabIndex={-1}
              className={classes}
              href={item.href}
              download={item.download}
              onClick={() => {
                onClose();
                item.onSelect?.();
              }}
            >
              {inner}
            </a>
          );
        }
        return (
          <button
            key={item.id}
            type="button"
            role="menuitem"
            tabIndex={-1}
            className={classes}
            onClick={() => {
              onClose();
              item.onSelect?.();
            }}
          >
            {inner}
          </button>
        );
      })}
    </div>
  );

  return createPortal(content, document.body);
}
