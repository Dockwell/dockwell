// SPDX-License-Identifier: AGPL-3.0-only

import {
  type CSSProperties,
  createContext,
  Fragment,
  type ReactNode,
  type Ref,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { cx, hasContent } from "../../lib/cx";
import { ModalLayer, useCoveredByLayer } from "../../lib/modal";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import "./Sheet.css";

export interface SheetProps {
  /** Needed unless over holds all the sheet ever shows. */
  title?: ReactNode;
  subtitle?: ReactNode;
  /** Overlay mode: render only while open. */
  open?: boolean;
  /** Called on Escape, on a click on the scrim and from the close button. Unsaved edits ask first. */
  onClose?: () => void;
  /** Actions, the primary one on the right. A dw-sheet-note at the start holds duration and undo window. */
  footer?: ReactNode;
  children?: ReactNode;
  /**
   * A SheetView that takes the sheet over without closing it, such as the plan
   * a form hands on to: one shell and one scrim, with a crossfade between the
   * two. This view stays as it is underneath, and comes back with its values,
   * its scroll and its focus once over is gone.
   */
  over?: ReactNode;
  /** Defaults to sheet-width (560px). Wider content belongs on a page. */
  width?: number | string;
  /** Render the panel in place, without scrim, portal or fixed positioning. */
  inline?: boolean;
  /** Receives focus on open, such as a form's first field. Defaults to the sheet itself. */
  initialFocusRef?: RefObject<HTMLElement | null> | undefined;
  className?: string;
}

export interface SheetViewProps {
  title: ReactNode;
  subtitle?: ReactNode;
  /** Called on Escape, on a click on the scrim and from the close button, while this view shows. */
  onClose?: (() => void) | undefined;
  footer?: ReactNode;
  children?: ReactNode;
  className?: string | undefined;
  /** The heading, which takes focus when a view hands on to this one, and which a screen reader then reads. */
  headingRef?: Ref<HTMLHeadingElement> | undefined;
}

/** What a view tells the shell around it, once it is the one showing. */
interface Shell {
  showing: (labels: { title: string; subtitle: string | undefined }, onClose: (() => void) | undefined) => void;
  /** The shell was there before this view: it took over from another. */
  handedOn: RefObject<boolean>;
}

const ShellContext = createContext<Shell | null>(null);
/** True for a view that is on its way out, or covered by another. */
const AwayContext = createContext(false);

/**
 * True while a view is leaving or covered, by another view of its sheet or by any
 * modal above it, such as the command bar, when it mustn't act on keys of its own.
 */
export function useSheetViewAway(): boolean {
  const covered = useCoveredByLayer();
  return useContext(AwayContext) || covered;
}

/** How long a sheet's views take to swap, as hover and press do: 120 ms (--motion-fast). */
const SWAP_MS = 120;
/** When a sheet's copy goes, whatever its animation did: past its 260 ms (--motion-slow). */
const EXIT_MS = 400;

/**
 * A panel that slides in from the right for plans, settings and details,
 * keeping the page visible behind it. In overlay mode it is a modal dialog:
 * focus moves in, stays inside, and returns to the opener on close.
 */
export function Sheet(props: SheetProps) {
  if (props.inline) return <SheetShell {...props} modal={false} />;
  if (!props.open) return null;
  return <SheetLayer {...props} />;
}

/**
 * Leaves as it came, whoever closed it and however: a sheet is often unmounted
 * along with what holds it, so what slides away is a copy, inert and hidden from
 * assistive tech, which goes once it has. Under reduced motion there is none. It
 * is taken before the panel goes, and only shown once it has, as StrictMode
 * unmounts a layer that stays.
 */
function leaveWithMotion(panel: HTMLElement | null): void {
  const layer = panel?.parentElement;
  const parent = layer?.parentElement;
  if (!layer || !parent || typeof layer.getAnimations !== "function") return;
  if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) return;
  const ghost = layer.cloneNode(true) as HTMLElement;
  ghost.classList.add("is-leaving");
  ghost.setAttribute("aria-hidden", "true");
  ghost.inert = true;
  // It names nothing any more: what it was is gone, and only looks as if it is on its way.
  for (const el of ghost.querySelectorAll("[id], [aria-label], [aria-labelledby], [aria-describedby], [role]")) {
    for (const name of ["id", "aria-label", "aria-labelledby", "aria-describedby", "role"]) el.removeAttribute(name);
  }
  // A copy keeps what was typed, and how far it had scrolled is for it to be told.
  const scrolls = [...layer.querySelectorAll(".dw-sheet-body")].map((el) => el.scrollTop);
  queueMicrotask(() => {
    if (layer.isConnected) return;
    parent.append(ghost);
    for (const [at, el] of [...ghost.querySelectorAll(".dw-sheet-body")].entries()) el.scrollTop = scrolls[at] ?? 0;
    const gone = () => ghost.remove();
    const timer = setTimeout(gone, EXIT_MS);
    void Promise.allSettled(ghost.getAnimations({ subtree: true }).map((a) => a.finished)).then(() => {
      clearTimeout(timer);
      gone();
    });
  });
}

function SheetLayer(props: SheetProps) {
  const panelRef = useRef<HTMLElement>(null);
  const closer = useRef<(() => void) | undefined>(undefined);
  const close = useCallback(() => closer.current?.(), []);
  useLayoutEffect(() => () => leaveWithMotion(panelRef.current), []);
  return (
    <ModalLayer
      className="dw-sheet-layer"
      containerRef={panelRef}
      initialFocusRef={props.initialFocusRef}
      onClose={close}
    >
      <SheetShell {...props} modal ref={panelRef} closer={closer} />
    </ModalLayer>
  );
}

function SheetShell({
  title,
  subtitle,
  onClose,
  footer,
  children,
  over,
  width,
  inline = false,
  className,
  initialFocusRef,
  modal,
  ref,
  closer,
}: SheetProps & {
  modal: boolean;
  ref?: RefObject<HTMLElement | null>;
  closer?: RefObject<(() => void) | undefined>;
}) {
  const [labels, setLabels] = useState<{ title: string; subtitle: string | undefined }>();
  const handedOn = useRef(false);
  const shell = useMemo<Shell>(
    () => ({
      showing(next, close) {
        if (closer) closer.current = close;
        setLabels((was) => (was?.title === next.title && was?.subtitle === next.subtitle ? was : next));
      },
      handedOn,
    }),
    [closer],
  );
  // Once it has opened, any view that comes takes over from another.
  useEffect(() => {
    handedOn.current = true;
  }, []);

  // Where focus was when another view covered this one, to go back to. It is
  // read as the view is covered, before going inert takes focus away.
  const covering = hasContent(over);
  const wasCovering = useRef(covering);
  const focused = useRef<Element | null>(null);
  if (covering && !wasCovering.current && focused.current === null) focused.current = document.activeElement;
  const panel = useRef<HTMLElement>(null);
  const setPanel = useCallback(
    (el: HTMLElement | null) => {
      panel.current = el;
      if (ref) ref.current = el;
    },
    [ref],
  );
  useLayoutEffect(() => {
    if (wasCovering.current && !covering) {
      const back = focused.current;
      const target =
        back instanceof HTMLElement && back !== document.body && back.isConnected
          ? back
          : (initialFocusRef?.current ?? panel.current);
      target?.focus({ preventScroll: true });
      focused.current = null;
    }
    wasCovering.current = covering;
  }, [covering, initialFocusRef]);

  const style: CSSProperties | undefined = width !== undefined ? { width } : undefined;
  return (
    <section
      ref={setPanel}
      tabIndex={-1}
      className={cx("dw-sheet", inline && "is-inline", className)}
      role="dialog"
      aria-modal={modal || undefined}
      aria-labelledby={labels?.title}
      aria-describedby={labels?.subtitle}
      style={style}
    >
      <ShellContext value={shell}>
        {hasContent(title) ? (
          <AwayContext value={covering}>
            <SheetView title={title} subtitle={subtitle} onClose={onClose} footer={footer}>
              {children}
            </SheetView>
          </AwayContext>
        ) : null}
        <SheetSwap viewKey={covering ? "over" : ""}>{over}</SheetSwap>
      </ShellContext>
    </section>
  );
}

/**
 * Swaps one SheetView for another in the sheet they share, with a crossfade:
 * the old view stays for the moment it takes to fade out. Give each its own
 * viewKey.
 */
export function SheetSwap({ viewKey, children }: { viewKey: string | number; children?: ReactNode }) {
  // A swap inside a view on its way out is on its way out too.
  const away = useContext(AwayContext);
  const latest = useRef({ key: viewKey, node: children });
  const [leaving, setLeaving] = useState<{ key: string | number; node: ReactNode }>();
  const [showing, setShowing] = useState(viewKey);
  if (showing !== viewKey) {
    setShowing(viewKey);
    if (hasContent(latest.current.node)) setLeaving(latest.current);
  }
  useLayoutEffect(() => {
    latest.current = { key: viewKey, node: children };
  });
  useEffect(() => {
    if (!leaving) return;
    const timer = setTimeout(() => setLeaving(undefined), SWAP_MS);
    return () => clearTimeout(timer);
  }, [leaving]);
  const gone = leaving?.key === viewKey ? undefined : leaving;
  return (
    <>
      {gone ? (
        <Fragment key={gone.key}>
          <AwayContext value={true}>{gone.node}</AwayContext>
        </Fragment>
      ) : null}
      <Fragment key={viewKey}>
        <AwayContext value={away}>{children}</AwayContext>
      </Fragment>
    </>
  );
}

/**
 * One view of a sheet: its head, body and footer. Sheet draws one of its own
 * from its props; pass another as over, or inside a SheetSwap, to take over.
 */
export function SheetView({ title, subtitle, onClose, footer, children, className, headingRef }: SheetViewProps) {
  const strings = useUIStrings();
  const shell = useContext(ShellContext);
  const away = useContext(AwayContext);
  const titleId = useId();
  const subtitleId = useId();
  const hasSubtitle = hasContent(subtitle);
  const root = useRef<HTMLDivElement>(null);
  const heading = useRef<HTMLHeadingElement | null>(null);
  const setHeading = useCallback(
    (el: HTMLHeadingElement | null) => {
      heading.current = el;
      if (typeof headingRef === "function") headingRef(el);
      else if (headingRef) headingRef.current = el;
    },
    [headingRef],
  );
  useLayoutEffect(() => {
    if (!away) shell?.showing({ title: titleId, subtitle: hasSubtitle ? subtitleId : undefined }, onClose);
  }, [away, shell, titleId, subtitleId, hasSubtitle, onClose]);
  // A view that takes over takes focus with it, as what had it goes away. The
  // first view of a sheet is the modal layer's to focus, and just slides in.
  const [entering] = useState(() => shell?.handedOn.current === true);
  const tookFocus = useRef(false);
  useLayoutEffect(() => {
    // Only as it first shows; a view covered and uncovered gets its focus back from the shell.
    if (tookFocus.current) return;
    tookFocus.current = true;
    if (entering && !away && !root.current?.contains(document.activeElement)) {
      heading.current?.focus({ preventScroll: true });
    }
  }, [entering, away]);
  return (
    <div
      ref={root}
      className={cx("dw-sheet-view", entering && "is-entering", away && "is-away", className)}
      inert={away || undefined}
      aria-hidden={away || undefined}
    >
      {/* Not a <header> or <footer>: inside a dialog they'd read as the page's banner and footer. */}
      <div className="dw-sheet-head">
        <div className="dw-sheet-heading">
          <h2 ref={setHeading} id={titleId} className="dw-sheet-title" tabIndex={-1}>
            {title}
          </h2>
          {hasSubtitle ? (
            <p id={subtitleId} className="dw-sheet-sub">
              {subtitle}
            </p>
          ) : null}
        </div>
        {onClose ? <Button variant="ghost" size="sm" icon="x" aria-label={strings.close} onClick={onClose} /> : null}
      </div>
      <div className="dw-sheet-body">{children}</div>
      {hasContent(footer) ? <div className="dw-sheet-foot">{footer}</div> : null}
    </div>
  );
}
