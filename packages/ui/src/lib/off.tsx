// SPDX-License-Identifier: AGPL-3.0-only

import {
  createContext,
  type MouseEvent,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { useCoveredByLayer } from "./modal";
import "./off.css";

/**
 * A control someone may not use, and why. It stays where it is and in the tab
 * order, so a keyboard reaches it and hears why; it's `aria-disabled`, not
 * `disabled`, with its reason as its description, shown beside it on hover and
 * on focus, and its click does nothing. "Disabled controls say why nearby"
 * (design/system/README.md).
 */
export interface Off {
  /** Spread onto the control. Empty when it isn't off. */
  props: {
    "aria-disabled"?: true;
    "aria-describedby"?: string;
    "data-off"?: "";
    onClickCapture?: (event: MouseEvent) => void;
  };
  /** The reason, to render inside the control. Nothing when it isn't off. */
  why: ReactNode;
}

type Report = (id: string, reason: string | undefined) => void;

const OffReasons = createContext<Report | undefined>(undefined);

/** Swallows a click on a control that's off, before any handler of its own or its form's. */
function swallow(event: MouseEvent) {
  event.preventDefault();
  event.stopPropagation();
}

function placeReason(control: HTMLElement, reason: HTMLElement) {
  const inset = 8;
  const gap = 4;
  const viewport = window.visualViewport;
  const width = viewport?.width ?? document.documentElement.clientWidth;
  const height = viewport?.height ?? document.documentElement.clientHeight;
  const left = (viewport?.offsetLeft ?? 0) + inset;
  const top = (viewport?.offsetTop ?? 0) + inset;
  const right = left + width - 2 * inset;
  const bottom = top + height - 2 * inset;
  reason.style.setProperty("--dw-off-width", `${right - left}px`);
  reason.style.setProperty("--dw-off-height", `${bottom - top}px`);

  const anchor = control.getBoundingClientRect();
  const box = reason.getBoundingClientRect();
  const rightAligned = anchor.right - box.width;
  const below = anchor.bottom + gap;
  const above = anchor.top - gap - box.height;
  const x = rightAligned >= left ? rightAligned : anchor.left;
  const y = below + box.height <= bottom || bottom - below >= anchor.top - gap - top ? below : above;
  reason.style.left = `${Math.max(left, Math.min(x, right - box.width))}px`;
  reason.style.top = `${Math.max(top, Math.min(y, bottom - box.height))}px`;
}

/** Shows the reason outside the control's clipping ancestors while keeping its description in place. */
function OffReason({ id, reason }: { id: string; reason: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const covered = useCoveredByLayer();
  useLayoutEffect(() => {
    const popover = ref.current;
    const control = popover?.closest<HTMLElement>("[data-off]");
    if (covered || !control || !popover?.showPopover) return;

    let hovered = control.matches(":hover");
    let focused = control.contains(document.activeElement);
    let tracking: AbortController | undefined;
    const place = () => placeReason(control, popover);
    const hide = () => {
      if (!tracking) return;
      tracking.abort();
      tracking = undefined;
      popover.hidePopover();
    };
    const sync = () => {
      if (!hovered && !focused) {
        hide();
        return;
      }
      if (tracking) return;
      popover.showPopover();
      place();
      tracking = new AbortController();
      const { signal } = tracking;
      window.addEventListener("scroll", place, { capture: true, passive: true, signal });
      window.addEventListener("resize", place, { signal });
      window.visualViewport?.addEventListener("scroll", place, { signal });
      window.visualViewport?.addEventListener("resize", place, { signal });
      if (typeof ResizeObserver !== "undefined") {
        const sizes = new ResizeObserver(place);
        sizes.observe(control);
        sizes.observe(popover);
        signal.addEventListener("abort", () => sizes.disconnect(), { once: true });
      }
    };
    const enter = (event: PointerEvent) => {
      if (event.pointerType === "touch") return;
      hovered = true;
      sync();
    };
    const leave = () => {
      hovered = false;
      sync();
    };
    const focus = () => {
      focused = true;
      sync();
    };
    const blur = (event: FocusEvent) => {
      focused = event.relatedTarget instanceof Node && control.contains(event.relatedTarget);
      sync();
    };
    const events = new AbortController();
    const { signal } = events;
    control.addEventListener("pointerenter", enter, { signal });
    control.addEventListener("pointerleave", leave, { signal });
    control.addEventListener("focusin", focus, { signal });
    control.addEventListener("focusout", blur, { signal });
    sync();
    return () => {
      events.abort();
      hide();
    };
  }, [covered]);
  // Hidden from the control's name, and still its description, which names it by ID.
  return (
    <span id={id} ref={ref} className="dw-off-why" popover="manual" aria-hidden="true">
      {reason}
    </span>
  );
}

/**
 * What a control needs to be off with a reason: pass the reason, or nothing
 * when it isn't off. It tells the screen it's in, so a screen with no hover
 * can say the first reason once (useOffReasons).
 */
export function useOff(reason: string | undefined): Off {
  const id = useId();
  const report = useContext(OffReasons);
  useEffect(() => {
    if (!report || !reason) return;
    report(id, reason);
    return () => report(id, undefined);
  }, [report, id, reason]);
  return useMemo(() => {
    if (!reason) return { props: {}, why: null };
    return {
      props: { "aria-disabled": true, "aria-describedby": id, "data-off": "", onClickCapture: swallow },
      why: <OffReason id={id} reason={reason} />,
    };
  }, [id, reason]);
}

/**
 * Collects why the controls inside `Provider` are off. `first` is the reason of
 * the one that said so first, for a screen to say once where nothing hovers.
 */
export function useOffReasons(): {
  Provider: (props: { children: ReactNode }) => ReactNode;
  first: string | undefined;
} {
  const [reasons, setReasons] = useState<ReadonlyMap<string, string>>(new Map());
  const report = useCallback<Report>((id, reason) => {
    setReasons((was) => {
      if (was.get(id) === reason) return was;
      const next = new Map(was);
      if (reason) next.set(id, reason);
      else next.delete(id);
      return next;
    });
  }, []);
  const Provider = useCallback(
    ({ children }: { children: ReactNode }) => <OffReasons.Provider value={report}>{children}</OffReasons.Provider>,
    [report],
  );
  return { Provider, first: reasons.values().next().value };
}
