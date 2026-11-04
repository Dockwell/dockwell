// SPDX-License-Identifier: AGPL-3.0-only

import { type KeyboardEvent, type RefObject, useCallback, useEffect, useRef } from "react";

const FOCUSABLE = [
  "a[href]",
  "area[href]",
  "button:not([disabled])",
  'input:not([disabled]):not([type="hidden"])',
  "select:not([disabled])",
  "textarea:not([disabled])",
  "iframe",
  "audio[controls]",
  "video[controls]",
  '[contenteditable]:not([contenteditable="false"])',
  "[tabindex]",
].join(",");

/** The elements inside root that Tab reaches, in document order. */
export function tabbableIn(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (el) => el.tabIndex >= 0 && !el.closest("[hidden], [inert]"),
  );
}

// Open modal layers, innermost last. Only the top one handles keys and guards focus.
const layers: object[] = [];
const isTop = (layer: object) => layers[layers.length - 1] === layer;

let locks = 0;
let saved: { overflow: string; paddingRight: string } | null = null;

/** Stops the page behind a modal layer from scrolling. Returns the unlock. Nested locks count. */
export function lockBodyScroll(): () => void {
  const body = document.body;
  if (locks === 0) {
    saved = { overflow: body.style.overflow, paddingRight: body.style.paddingRight };
    // Keep the layout still when the scrollbar disappears.
    const gutter = window.innerWidth - document.documentElement.clientWidth;
    if (gutter > 0) body.style.paddingRight = `${gutter}px`;
    body.style.overflow = "hidden";
  }
  locks += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    locks -= 1;
    if (locks === 0 && saved) {
      body.style.overflow = saved.overflow;
      body.style.paddingRight = saved.paddingRight;
      saved = null;
    }
  };
}

export interface ModalLayerOptions {
  open: boolean;
  /** The dialog. Focus stays inside it while open. */
  containerRef: RefObject<HTMLElement | null>;
  /** Receives focus on open. Defaults to the container, which needs tabIndex={-1}. */
  initialFocusRef?: RefObject<HTMLElement | null>;
  /** Called on Escape. Without it, Escape does nothing. */
  onClose?: (() => void) | undefined;
}

/**
 * Makes an element a modal layer while open: focus moves in, Tab and
 * Shift+Tab cycle inside it, Escape closes it, the page behind stops
 * scrolling, and focus returns to the opener on close. Layers nest: only the
 * innermost one responds. Spread the returned handler as the layer's
 * onKeyDown.
 */
export function useModalLayer({
  open,
  containerRef,
  initialFocusRef,
  onClose,
}: ModalLayerOptions): (event: KeyboardEvent<HTMLElement>) => void {
  const layer = useRef<object>({}).current;
  const opener = useRef<HTMLElement | null>(null);
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  });

  useEffect(() => {
    if (!open) return;
    const container = containerRef.current;
    if (!container) return;
    const active = document.activeElement;
    // Under StrictMode the effect runs twice; keep the first opener.
    if (active instanceof HTMLElement && !container.contains(active)) opener.current = active;
    layers.push(layer);
    const unlock = lockBodyScroll();
    (initialFocusRef?.current ?? container).focus({ preventScroll: true });

    // Focus that leaves by other means (a click outside, a script) comes back.
    const onFocusIn = (event: FocusEvent) => {
      if (!isTop(layer) || !(event.target instanceof Node) || container.contains(event.target)) return;
      (tabbableIn(container)[0] ?? container).focus({ preventScroll: true });
    };
    document.addEventListener("focusin", onFocusIn);

    return () => {
      document.removeEventListener("focusin", onFocusIn);
      const at = layers.lastIndexOf(layer);
      if (at !== -1) layers.splice(at, 1);
      unlock();
      const back = opener.current;
      if (back?.isConnected) back.focus({ preventScroll: true });
    };
  }, [open, containerRef, initialFocusRef, layer]);

  return useCallback(
    (event: KeyboardEvent<HTMLElement>) => {
      if (!isTop(layer)) return;
      if (event.key === "Escape") {
        // Stop here, so a layer underneath in the React tree doesn't close too.
        event.stopPropagation();
        const close = onCloseRef.current;
        if (close) {
          event.preventDefault();
          close();
        }
        return;
      }
      if (event.key !== "Tab") return;
      const container = containerRef.current;
      if (!container) return;
      const items = tabbableIn(container);
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (!first || !last) {
        event.preventDefault();
        container.focus({ preventScroll: true });
      } else if (event.shiftKey && (active === first || active === container)) {
        event.preventDefault();
        last.focus({ preventScroll: true });
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus({ preventScroll: true });
      }
    },
    [containerRef, layer],
  );
}
