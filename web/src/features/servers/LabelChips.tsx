// SPDX-License-Identifier: AGPL-3.0-only

import { cx, Glyph, Icon, type IconName, Tag, TONE, useUIStrings } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { type ChangeEvent, type KeyboardEvent, type ReactNode, useId, useRef, useState } from "react";

import { LABEL_NAME, LABEL_PREFIX, type LabelProblem, MAX_LABELS } from "./labels";
import "./LabelChips.css";

/** Words for what's wrong with a label, as the server says it (internal/resource/labels.go). */
export function useLabelProblemWords(): (problem: LabelProblem) => string {
  const { t } = useLingui();
  return (problem) => {
    switch (problem.kind) {
      case "no-key":
        return t`A label needs a key, such as region`;
      case "empty-prefix":
        return t`Write a prefix before /, such as example.com/, or leave / out`;
      case "long-prefix": {
        const { length } = problem;
        return t`Its prefix is ${length} characters, past the ${LABEL_PREFIX} a prefix may have`;
      }
      case "prefix-not-domain":
        return t`Its prefix isn't a domain: lowercase letters, digits, - and ., such as example.com`;
      case "no-name":
        return t`It has no name after its prefix`;
      case "long": {
        const { length } = problem;
        return t`It's ${length} characters, past the ${LABEL_NAME} a label may have`;
      }
      case "ends":
        return t`A key and a value start and end with a letter or digit`;
      case "character": {
        const { character } = problem;
        return /\s/.test(character)
          ? t`It has a space, and a label holds letters, digits, -, _ and . alone`
          : t`It has ${character}, and a label holds letters, digits, -, _ and . alone`;
      }
      case "too-many":
        return t`A server can have ${MAX_LABELS} labels`;
    }
  };
}

/** A problem that more typing can't mend, so it shows as soon as it's there. */
function showsAtOnce(problem: LabelProblem, text: string): boolean {
  if (problem.kind === "character" || problem.kind === "long" || problem.kind === "long-prefix") return true;
  // A first character that can't start one, as more typing leaves it first.
  return problem.kind === "ends" && /^!?[^A-Za-z0-9]/.test(text) && !text.startsWith("!:");
}

export interface Chip {
  /** What the chip is, as it's typed: key:value, a key, or a selector's term. */
  text: string;
  key: string;
  /** Undefined shows the key alone. */
  value?: string | undefined;
}

/**
 * Labels or a selector's terms as chips, each with its remove, and a field that takes
 * another, offering what the fleet uses already: picking a whole label adds it at once,
 * picking a key leaves it to type its value. Enter, a space or a comma adds what's
 * typed; Backspace in an empty field takes the last chip back to edit. What's wrong
 * with what's typed shows under it, in the server's words.
 */
export function LabelChips({
  label,
  chips,
  onAdd,
  onRemove,
  check,
  suggestions,
  placeholder,
  icon,
  after,
  hint,
  shownLabel = false,
  optional = false,
  className,
}: {
  /** What the field is, for a screen reader. */
  label: string;
  chips: readonly Chip[];
  /** Adds what's typed, once check passes it. */
  onAdd: (text: string) => void;
  onRemove: (index: number) => void;
  /** What's wrong with what's typed, or undefined. */
  check: (text: string) => LabelProblem | undefined;
  suggestions: readonly string[];
  placeholder: string;
  icon?: IconName;
  /** At the end of the field, such as how many servers match. */
  after?: ReactNode;
  /** Under the field while nothing is wrong. */
  hint?: ReactNode;
  /** Shows label above the field, as a form's fields have theirs, rather than for a screen reader alone. */
  shownLabel?: boolean;
  /** Marks a field that may stay empty, beside its label. */
  optional?: boolean;
  className?: string;
}) {
  const words = useLabelProblemWords();
  const strings = useUIStrings();
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [tried, setTried] = useState(false);
  const problem = text.trim() === "" ? undefined : check(text.trim());
  const shown = problem && (tried || showsAtOnce(problem, text.trim())) ? words(problem) : undefined;

  const add = (value = text) => {
    const v = value.trim();
    if (v === "") return true;
    if (check(v)) {
      setText(value);
      setTried(true);
      return false;
    }
    onAdd(v);
    setText("");
    setTried(false);
    return true;
  };
  const change = (event: ChangeEvent<HTMLInputElement>) => {
    const value = event.target.value;
    // A pick from the list, which fills the field whole: a whole label goes in at once.
    const picked =
      !(event.nativeEvent instanceof InputEvent) || event.nativeEvent.inputType === "insertReplacementText";
    if (picked && suggestions.includes(value) && !value.endsWith(":")) {
      add(value);
      return;
    }
    setText(value);
    setTried(false);
  };
  const key = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter" || event.key === "," || (event.key === " " && text.trim() !== "")) {
      if (event.key === "Enter" && text.trim() === "") return;
      event.preventDefault();
      add();
    } else if (event.key === "Backspace" && text === "" && chips.length > 0) {
      event.preventDefault();
      const last = chips[chips.length - 1];
      onRemove(chips.length - 1);
      setText(last?.text ?? "");
    }
  };
  const messageId = `${id}-message`;
  const message = shown ?? hint;
  return (
    <div className={cx("dw-field", "label-chips", shown && "is-invalid", className)}>
      {shownLabel ? (
        <label className="dw-field-label" htmlFor={id}>
          <span>{label}</span>
          {optional ? <span className="dw-field-opt">{strings.optional}</span> : null}
        </label>
      ) : null}
      {/* A click anywhere in the box but on a chip's remove puts the keyboard in the field. */}
      {/* biome-ignore lint/a11y/noStaticElementInteractions: the field inside takes the keyboard; this only widens where a click lands */}
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: as above */}
      <div className="dw-input label-chips-box" onClick={(e) => e.target === e.currentTarget && input.current?.focus()}>
        {icon ? <Icon name={icon} size={16} className="label-chips-icon" /> : null}
        {chips.map((chip, i) => (
          <Tag
            key={chip.text}
            label={chip.key}
            value={chip.value}
            onRemove={() => onRemove(i)}
            removeLabel={chip.text}
          />
        ))}
        {shownLabel ? null : (
          <label htmlFor={id} className="dw-sr">
            {label}
          </label>
        )}
        <input
          ref={input}
          id={id}
          className="dw-input-el is-mono label-chips-input"
          value={text}
          list={`${id}-list`}
          placeholder={placeholder}
          spellCheck={false}
          autoCapitalize="off"
          autoComplete="off"
          enterKeyHint="done"
          aria-invalid={shown ? true : undefined}
          aria-describedby={message ? messageId : undefined}
          onChange={change}
          onKeyDown={key}
          onBlur={() => {
            if (text.trim() !== "") add();
          }}
        />
        <datalist id={`${id}-list`}>
          {suggestions.map((s) => (
            <option key={s} value={s} />
          ))}
        </datalist>
        {after}
      </div>
      {message ? (
        <div id={messageId} className={shown ? "dw-field-error" : "dw-field-hint"} role={shown ? "alert" : undefined}>
          {shown ? <Glyph shape={TONE.critical.shape} size={9} /> : null}
          <span>{message}</span>
        </div>
      ) : null}
    </div>
  );
}
