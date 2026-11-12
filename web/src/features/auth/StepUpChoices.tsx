// SPDX-License-Identifier: AGPL-3.0-only

import { StepUpPolicy } from "@dockwell/api";
import { cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useId } from "react";

import "./StepUp.css";

/** One of the three choices of how often a passkey is asked for again, as its card says it. */
export interface StepUpOption {
  value: StepUpPolicy;
  label: string;
  /** What it means, in one line. */
  hint: string;
  /** Why it can't be chosen here, in place of the hint, or nothing when it can. */
  off?: string;
}

/** The choices in order, the one that asks most first, as Profile and Settings both list them. */
export const STEP_UP_ORDER = [StepUpPolicy.EACH_TIME, StepUpPolicy.HOURLY, StepUpPolicy.AT_SIGN_IN] as const;

/** How little a choice asks, to set it beside a floor: each time asks most. */
export function looseness(policy: StepUpPolicy): number {
  switch (policy) {
    case StepUpPolicy.HOURLY:
      return 1;
    case StepUpPolicy.AT_SIGN_IN:
      return 2;
    default:
      return 0;
  }
}

/**
 * The three choices as cards, each with one line of what it means, and one the
 * install doesn't allow off with why in its place (ADR 0059). Choosing one acts
 * at once, as the passkey it asks for is the confirmation: while that's under way
 * its card says so and the others wait.
 */
export function StepUpChoices({
  label,
  options,
  value,
  pending,
  disabled,
  onChoose,
}: {
  /** The group's name, for assistive technology. */
  label: string;
  options: StepUpOption[];
  /** The choice that applies, or none while it loads. */
  value?: StepUpPolicy;
  /** The choice being confirmed, if one is. */
  pending?: StepUpPolicy;
  disabled?: boolean;
  onChoose: (policy: StepUpPolicy) => void;
}) {
  const { t } = useLingui();
  const name = useId();
  const shown = pending ?? value;
  return (
    <fieldset className="step-up-choices" aria-label={label} aria-busy={pending !== undefined} disabled={disabled}>
      {options.map((option) => {
        const on = shown === option.value;
        const waiting = pending !== undefined && !on;
        return (
          <label
            key={option.value}
            className={cx("step-up-choice", on && "is-on", (option.off || waiting) && "is-off")}
            data-policy={StepUpPolicy[option.value]}
          >
            <input
              type="radio"
              name={name}
              checked={on}
              disabled={Boolean(option.off) || waiting}
              onChange={() => onChoose(option.value)}
            />
            <span className="step-up-choice-text">
              <span className="step-up-choice-name">{option.label}</span>
              <span className={cx("step-up-choice-hint", pending === option.value && "is-pending")}>
                {pending === option.value ? t`Confirm with your passkey` : (option.off ?? option.hint)}
              </span>
            </span>
          </label>
        );
      })}
    </fieldset>
  );
}
