// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useId } from "react";

import { cx, hasContent } from "../../lib/cx";
import { STATUS, TONE } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Glyph } from "../Glyph/Glyph";
import { TextField, type TextFieldProps } from "../TextField/TextField";
import "./PortField.css";

/** What is known of a port on the server, as the field shows it beside the number. */
export type PortCheck =
  /** Still asking. */
  | { state: "checking" }
  /** Nothing holds it. */
  | { state: "free" }
  /** Something holds it: by names what, such as "redis" or "Dockwell's proxy", when that's known. */
  | { state: "taken"; by?: ReactNode }
  /** Nobody can say: the server didn't answer, or the reader may not ask. The field says nothing. */
  | { state: "unknown" };

export interface PortFieldProps
  extends Omit<TextFieldProps, "value" | "onChange" | "inputMode" | "suffix" | "prefix" | "type" | "mono"> {
  value: string;
  onChange: (value: string) => void;
  /** What is known of the port typed, checked against the server as it is typed. */
  check?: PortCheck;
  /** A free port to offer while the field is empty, or holds one that's taken. */
  suggestion?: number | undefined;
  /**
   * One field in a row of them: it says nothing under itself, so the row's
   * fields stay level, and marks itself invalid when the port is taken. The
   * row says what holds it, under the row.
   */
  inRow?: boolean;
}

/** The ports a server has, as a field takes them. */
export function validPortNumber(value: string): boolean {
  if (!/^\d{1,5}$/.test(value.trim())) return false;
  const n = Number(value);
  return n >= 1 && n <= 65535;
}

/**
 * A port on a server: a number, with whether it's free beside it as it's
 * typed, what holds it when it isn't, and a free one to take instead. The
 * form fills it with a free port to start with (defaults first), so most
 * people never type one. Only digits go in.
 */
export function PortField({
  value,
  onChange,
  check,
  suggestion,
  inRow = false,
  error,
  hint,
  className,
  ...rest
}: PortFieldProps) {
  const strings = useUIStrings();
  const words = strings.portField;
  const stateId = useId();
  const typed = value.trim();
  const outOfRange = typed !== "" && !validPortNumber(typed);
  const shown = typed === "" || outOfRange ? undefined : check;
  const offer =
    suggestion !== undefined && String(suggestion) !== typed && (typed === "" || shown?.state === "taken")
      ? suggestion
      : undefined;
  const use =
    offer === undefined ? null : (
      <button type="button" className="dw-port-use" onClick={() => onChange(String(offer))}>
        {words.use(offer)}
      </button>
    );
  const taken = shown?.state === "taken";
  const message = hasContent(error) ? (
    error
  ) : outOfRange ? (
    words.range
  ) : taken ? (
    <>
      {hasContent(shown.by) ? words.takenBy(shown.by) : words.taken}
      {use ? <> · {use}</> : null}
    </>
  ) : undefined;
  const help =
    message === undefined && use ? (
      <>
        {hasContent(hint) ? <>{hint} · </> : null}
        {use}
      </>
    ) : (
      hint
    );
  return (
    <TextField
      {...rest}
      className={cx("dw-port-field", className)}
      mono
      inputMode="numeric"
      autoComplete="off"
      maxLength={5}
      value={value}
      onChange={(event) => onChange(event.target.value.replace(/\D/g, ""))}
      error={inRow ? undefined : message}
      hint={inRow ? undefined : help}
      invalid={inRow ? message !== undefined || rest.invalid === true : rest.invalid}
      aria-describedby={cx(shown && shown.state !== "unknown" && stateId, rest["aria-describedby"]) || undefined}
      suffix={
        shown && shown.state !== "unknown" ? (
          <span id={stateId} className={cx("dw-port-state", `is-${shown.state}`)} aria-live="polite">
            {shown.state === "checking" ? (
              <Glyph shape={STATUS.deploying.shape} size={9} />
            ) : (
              <Glyph shape={shown.state === "free" ? TONE.healthy.shape : TONE.critical.shape} size={9} />
            )}
            <span>{words[shown.state]}</span>
          </span>
        ) : undefined
      }
    />
  );
}
