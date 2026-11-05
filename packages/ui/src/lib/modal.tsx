// SPDX-License-Identifier: AGPL-3.0-only

import {
  createContext,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useState,
  useSyncExternalStore,
} from "react";

import { Portal } from "./Portal";
import "../styles/overlay.css";

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

interface Layer {
  /** The layer this one renders inside, in the React tree. */
  parent: Layer | null;
  /** Where focus goes back to when this layer closes. */
  opener: HTMLElement | null;
}

// Open modal layers, innermost last. Only the top one handles keys and guards focus.
const layers: Layer[] = [];
const isTop = (layer: Layer) => layers[layers.length - 1] === layer;
const isWithin = (layer: Layer, ancestor: Layer) => {
  for (let at: Layer | null = layer; at; at = at.parent) if (at === ancestor) return true;
  return false;
};

const ParentLayer = createContext<Layer | null>(null);

// Who wants to know when the layers change, which isn't something React sees.
const watchers = new Set<() => void>();
const watchLayers = (watcher: () => void) => {
  watchers.add(watcher);
  return () => {
    watchers.delete(watcher);
  };
};
const layersMoved = () => {
  for (const watcher of watchers) watcher();
};

/**
 * True while another modal layer is open above the one this renders in, or above
 * the page, when it renders in none. What is under a layer is still on the screen,
 * and mustn't act on keys it hears on the window: they are the layer's.
 */
export function useCoveredByLayer(): boolean {
  const layer = useContext(ParentLayer);
  const covered = () => {
    if (layer === null) return layers.length > 0;
    const at = layers.indexOf(layer);
    return at !== -1 && at < layers.length - 1;
  };
  return useSyncExternalStore(watchLayers, covered, () => false);
}

let locks = 0;
let saved: { overflow: string; paddingRight: string } | null = null;

/** Stops the page behind a modal layer from scrolling. Returns the unlock. Nested locks count. */
export function lockBodyScroll(): () => void {
  const body = document.body;
  if (locks === 0) {
    saved = { overflow: body.style.overflow, paddingRight: body.style.paddingRight };
    // Keep the layout still when the scrollbar disappears: the gutter adds to the body's own padding.
    const gutter = window.innerWidth - document.documentElement.clientWidth;
    if (gutter > 0) {
      const padding = Number.parseFloat(getComputedStyle(body).paddingRight) || 0;
      body.style.paddingRight = `${padding + gutter}px`;
    }
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

/** How long, in frames, a page that is moving on gets to show its heading once a layer has closed. */
const SETTLE_FRAMES = 30;

/**
 * Puts focus back on what opened a layer, or on the page's heading when that has
 * gone, as it does when what it opened changed the page: focus never falls to
 * the body, where a keyboard has to start again from the top. A page on its way
 * has no heading yet, and a control still there may yet go, so it looks again for
 * a moment, unless someone has moved on or opened something else.
 */
function returnFocus(opener: HTMLElement | null): void {
  const nowhere = () => document.activeElement === null || document.activeElement === document.body;
  const heading = () => {
    const h1 = document.querySelector<HTMLElement>("main h1") ?? document.querySelector<HTMLElement>("h1");
    if (!h1) return;
    if (!h1.hasAttribute("tabindex")) h1.setAttribute("tabindex", "-1");
    h1.focus({ preventScroll: true });
  };
  if (opener && opener !== document.body && opener.isConnected) opener.focus({ preventScroll: true });
  let frames = SETTLE_FRAMES;
  const look = () => {
    if (layers.length > 0) return;
    if (nowhere()) heading();
    frames -= 1;
    if (frames > 0) requestAnimationFrame(look);
  };
  look();
}

export interface ModalLayerProps {
  /** Positions the dialog over the scrim, such as dw-sheet-layer. */
  className: string;
  /** The dialog. Focus stays inside it while the layer is open. */
  containerRef: RefObject<HTMLElement | null>;
  /** Receives focus on open. Defaults to the container, which then needs tabIndex={-1}. */
  initialFocusRef?: RefObject<HTMLElement | null> | undefined;
  /** Called on Escape and on a click on the scrim. Without it, neither closes the layer. */
  onClose?: (() => void) | undefined;
  /** The dialog, drawn over the scrim. */
  children: ReactNode;
}

/**
 * A modal layer, open while it is mounted: the dialog renders over a scrim
 * in a portal, focus moves in, Tab and Shift+Tab cycle inside it, Escape and
 * the scrim close it, the page behind stops scrolling, and focus returns to
 * the opener on close. Layers nest by where they render: only the innermost
 * one responds, even when it opens in the same render as its parent.
 */
export function ModalLayer({ className, containerRef, initialFocusRef, onClose, children }: ModalLayerProps) {
  const parent = useContext(ParentLayer);
  // A layer lives for one opening: the component unmounts when it closes.
  const [layer] = useState<Layer>(() => ({ parent, opener: null }));

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const target = initialFocusRef?.current ?? container;
    // React runs a child's effects before its parent's, so a layer nested in this one that opened in
    // the same render is already registered and holds focus. This one goes under it, takes over the
    // outer opener, and becomes where focus goes back when the nested layer closes. Under StrictMode
    // the effect runs twice; openers that are set stay.
    const nested = layers.findIndex((other) => isWithin(other, layer));
    const inner = layers[nested];
    if (inner) {
      layer.opener ??= inner.opener;
      inner.opener = target;
      layers.splice(nested, 0, layer);
      layersMoved();
    } else {
      const active = document.activeElement;
      if (!layer.opener && active instanceof HTMLElement && !container.contains(active)) layer.opener = active;
      layers.push(layer);
      layersMoved();
      target.focus({ preventScroll: true });
    }
    const unlock = lockBodyScroll();

    // Focus that leaves by other means (a click outside, a script) comes back.
    const onFocusIn = (event: FocusEvent) => {
      if (!isTop(layer) || !(event.target instanceof Node) || container.contains(event.target)) return;
      (tabbableIn(container)[0] ?? container).focus({ preventScroll: true });
    };
    document.addEventListener("focusin", onFocusIn);

    return () => {
      document.removeEventListener("focusin", onFocusIn);
      const at = layers.indexOf(layer);
      if (at !== -1) layers.splice(at, 1);
      layersMoved();
      unlock();
      returnFocus(layer.opener);
    };
  }, [containerRef, initialFocusRef, layer]);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLElement>) => {
      // Keys pressed while an input method composes belong to it: there, Escape dismisses the
      // candidates, not the layer. Safari ends composition before keydown, so it only shows in keyCode 229.
      if (!isTop(layer) || event.nativeEvent.isComposing || event.keyCode === 229) return;
      if (event.key === "Escape") {
        // Stop here, so a layer underneath in the React tree doesn't close too.
        event.stopPropagation();
        if (onClose) {
          event.preventDefault();
          onClose();
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
      } else if (event.shiftKey && (active === first || !items.includes(active as HTMLElement))) {
        // Focus that isn't on a stop, such as a heading that took it, goes back round from the end.
        event.preventDefault();
        last.focus({ preventScroll: true });
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus({ preventScroll: true });
      }
    },
    [containerRef, layer, onClose],
  );

  return (
    <Portal>
      {/* Keys from inside the dialog bubble here: Escape closes, Tab stays inside. */}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: the layer only listens to keys from its dialog */}
      <div className={className} onKeyDown={onKeyDown}>
        <div className="dw-scrim" aria-hidden="true" onClick={onClose} />
        <ParentLayer value={layer}>{children}</ParentLayer>
      </div>
    </Portal>
  );
}
