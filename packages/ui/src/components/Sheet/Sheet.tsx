// SPDX-License-Identifier: AGPL-3.0-only

import { type CSSProperties, type ReactNode, type Ref, useId, useRef } from "react";

import { cx } from "../../lib/cx";
import { useModalLayer } from "../../lib/modal";
import { Portal } from "../../lib/Portal";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import "../../styles/overlay.css";
import "./Sheet.css";

export interface SheetProps {
  title: ReactNode;
  subtitle?: ReactNode;
  /** Overlay mode: render only while open. */
  open?: boolean;
  /** Called on Escape, on a click on the scrim and from the close button. Unsaved edits ask first. */
  onClose?: () => void;
  /** Actions, the primary one on the right. A dw-sheet-note at the start holds duration and undo window. */
  footer?: ReactNode;
  children?: ReactNode;
  /** Defaults to sheet-width (560px). Wider content belongs on a page. */
  width?: number | string;
  /** Render the panel in place, without scrim, portal or fixed positioning. */
  inline?: boolean;
  className?: string;
}

/**
 * A panel that slides in from the right for plans, settings and details,
 * keeping the page visible behind it. In overlay mode it is a modal dialog:
 * focus moves in, stays inside, and returns to the opener on close.
 */
export function Sheet(props: SheetProps) {
  if (props.inline) return <SheetPanel {...props} modal={false} />;
  if (!props.open) return null;
  return <SheetLayer {...props} />;
}

function SheetLayer(props: SheetProps) {
  const panelRef = useRef<HTMLElement>(null);
  const onKeyDown = useModalLayer({ open: true, containerRef: panelRef, onClose: props.onClose });
  return (
    <Portal>
      {/* Keys from inside the dialog bubble here: Escape closes, Tab stays inside. */}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: the layer only listens to keys from its dialog */}
      <div className="dw-sheet-layer" onKeyDown={onKeyDown}>
        <div className="dw-scrim" aria-hidden="true" onClick={props.onClose} />
        <SheetPanel {...props} modal ref={panelRef} />
      </div>
    </Portal>
  );
}

function SheetPanel({
  title,
  subtitle,
  onClose,
  footer,
  children,
  width,
  inline = false,
  className,
  modal,
  ref,
}: SheetProps & { modal: boolean; ref?: Ref<HTMLElement> }) {
  const strings = useUIStrings();
  const titleId = useId();
  const subtitleId = useId();
  const hasSubtitle = subtitle != null && subtitle !== false;
  const style: CSSProperties | undefined = width !== undefined ? { width } : undefined;
  return (
    <section
      ref={ref}
      tabIndex={-1}
      className={cx("dw-sheet", inline && "is-inline", className)}
      role="dialog"
      aria-modal={modal || undefined}
      aria-labelledby={titleId}
      aria-describedby={hasSubtitle ? subtitleId : undefined}
      style={style}
    >
      <header className="dw-sheet-head">
        <div className="dw-sheet-heading">
          <h2 id={titleId} className="dw-sheet-title">
            {title}
          </h2>
          {hasSubtitle ? (
            <p id={subtitleId} className="dw-sheet-sub">
              {subtitle}
            </p>
          ) : null}
        </div>
        {onClose ? <Button variant="ghost" size="sm" icon="x" aria-label={strings.close} onClick={onClose} /> : null}
      </header>
      <div className="dw-sheet-body">{children}</div>
      {footer != null && footer !== false ? <footer className="dw-sheet-foot">{footer}</footer> : null}
    </section>
  );
}
