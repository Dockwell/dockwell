// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { cx } from "../../lib/cx";
import { handleRovingKeyDown, tabStop } from "../../lib/roving";
import { useControlled } from "../../lib/useControlled";
import { Icon } from "../Icon/Icon";
import type { IconName } from "../Icon/icons";
import "./SegmentedControl.css";

export interface SegmentedOption<V extends string = string> {
  value: V;
  label: ReactNode;
  icon?: IconName;
  count?: number;
}

export interface SegmentedControlProps<V extends string = string> {
  /** Two to five views of the same content. More, or options of different kinds, belong in Tabs or a select. */
  options: readonly SegmentedOption<V>[];
  value?: V;
  /** Defaults to the first option. */
  defaultValue?: V;
  onChange?: (value: V) => void;
  /** Accessible name of the group. */
  label?: string;
  size?: "sm" | "md";
  className?: string;
}

/**
 * Switches between views of the same content, such as environment or time
 * range. A radio group: arrow keys move between options, and only the
 * selected one is in the tab order.
 */
export function SegmentedControl<V extends string = string>({
  options,
  value,
  defaultValue,
  onChange,
  label,
  size = "md",
  className,
}: SegmentedControlProps<V>) {
  const [current, setCurrent] = useControlled<V | undefined, V>(value, defaultValue ?? options[0]?.value, onChange);
  const values = options.map((option) => option.value);
  const stop = tabStop(values, current);
  return (
    <div
      className={cx("dw-seg", `dw-seg-${size}`, className)}
      role="radiogroup"
      aria-label={label}
      onKeyDown={(event) => handleRovingKeyDown(event, values, current, setCurrent)}
    >
      {options.map((option) => {
        const on = option.value === current;
        return (
          // Buttons give the segmented look and Home and End, which native radios lack.
          // biome-ignore lint/a11y/useSemanticElements: the WAI-ARIA radio group pattern
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={on}
            tabIndex={option.value === stop ? 0 : -1}
            className={cx("dw-seg-item", on && "is-on")}
            data-roving-item=""
            onClick={() => {
              if (!on) setCurrent(option.value);
            }}
          >
            {option.icon ? <Icon name={option.icon} size={14} /> : null}
            <span>{option.label}</span>
            {option.count != null ? <span className="dw-seg-count">{option.count}</span> : null}
          </button>
        );
      })}
    </div>
  );
}
