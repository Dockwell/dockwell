// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, type Ref, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { useControlled } from "../../lib/useControlled";
import "./Switch.css";

export interface SwitchProps {
  checked?: boolean;
  defaultChecked?: boolean;
  onChange?: (checked: boolean) => void;
  label?: ReactNode;
  /** One sentence on what happens when on. A disabled switch says why here. */
  description?: ReactNode;
  disabled?: boolean;
  /** Accessible name when there is no visible label. */
  "aria-label"?: string;
  id?: string;
  className?: string;
  ref?: Ref<HTMLButtonElement>;
}

/** Turns a setting on or off immediately, with no save step. Anything that changes running workloads goes through a plan instead. */
export function Switch({
  checked,
  defaultChecked = false,
  onChange,
  label,
  description,
  disabled = false,
  "aria-label": ariaLabel,
  id,
  className,
  ref,
}: SwitchProps) {
  const [on, setOn] = useControlled(checked, defaultChecked, onChange);
  const autoId = useId();
  const switchId = id ?? autoId;
  const labelId = `${switchId}-label`;
  const descriptionId = `${switchId}-description`;
  const hasLabel = hasContent(label);
  const hasDescription = hasContent(description);
  return (
    <label htmlFor={switchId} className={cx("dw-switch", disabled && "is-disabled", className)}>
      <button
        ref={ref}
        id={switchId}
        type="button"
        role="switch"
        aria-checked={on}
        aria-label={ariaLabel}
        aria-labelledby={hasLabel && !ariaLabel ? labelId : undefined}
        aria-describedby={hasDescription ? descriptionId : undefined}
        disabled={disabled}
        className="dw-switch-track"
        onClick={() => setOn(!on)}
      >
        <span className="dw-switch-thumb" />
      </button>
      {hasLabel || hasDescription ? (
        <span className="dw-switch-text">
          {hasLabel ? (
            <span id={labelId} className="dw-switch-label">
              {label}
            </span>
          ) : null}
          {hasDescription ? (
            <span id={descriptionId} className="dw-switch-desc">
              {description}
            </span>
          ) : null}
        </span>
      ) : null}
    </label>
  );
}
