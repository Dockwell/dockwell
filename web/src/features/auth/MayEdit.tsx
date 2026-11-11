// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Icon } from "@dockwell/ui";
import { type ReactNode, useId } from "react";

import type { Offer } from "./may";
import "./MayEdit.css";

/**
 * An editor for someone who may not plan with it: everything in it is there to
 * read, none of it takes a change, and one line above it says why. So a viewer
 * reads an app's settings as they are, and doesn't fill a form in to be refused
 * at the plan (ADR 0059). For someone who may, it's the editor alone.
 *
 * It's for an editor of many controls, which would each say the same reason. A
 * control on its own takes the offer's `off` itself.
 */
export function MayEdit({
  offer,
  children,
  className,
}: {
  /** What `may` answered for planning here. */
  offer: Offer;
  children: ReactNode;
  className?: string;
}) {
  const id = useId();
  if (offer.allowed) return <>{children}</>;
  return (
    <>
      {offer.reason ? (
        <p id={id} className="may-edit-why">
          <Icon name="lock" size={14} />
          <span>{offer.reason}</span>
        </p>
      ) : null}
      {/* A disabled fieldset turns off every field and button in it, and says why as its description. */}
      <fieldset disabled aria-describedby={offer.reason ? id : undefined} className={cx("may-edit", className)}>
        {children}
      </fieldset>
    </>
  );
}
