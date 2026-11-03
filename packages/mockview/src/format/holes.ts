// SPDX-License-Identifier: AGPL-3.0-only

// {{hole}} evaluation. A hole is a dotted lookup ({{user.name}}, {{$index}})
// or a literal ({{16}}, {{true}}, {{'text'}}), never an expression.

/** The names a template can see: renderVals() at the root, then one layer per <sc-for>. */
export class Scope {
  readonly #vars: Readonly<Record<string, unknown>>;
  readonly #parent: Scope | null;

  constructor(vars: Readonly<Record<string, unknown>>, parent: Scope | null = null) {
    this.#vars = vars;
    this.#parent = parent;
  }

  child(vars: Readonly<Record<string, unknown>>): Scope {
    return new Scope(vars, this);
  }

  has(name: string): boolean {
    return name in this.#vars || (this.#parent?.has(name) ?? false);
  }

  get(name: string): unknown {
    if (name in this.#vars) return this.#vars[name];
    return this.#parent?.get(name);
  }
}

/** Reported for a hole that is neither a lookup nor a literal. Claude Design renders those as nothing. */
export class HoleError extends Error {
  constructor(expression: string) {
    super(
      `{{${expression}}} is not a dotted lookup or a literal, so it renders as nothing. Compute it in renderVals().`,
    );
    this.name = "HoleError";
  }
}

const NUMBER = /^[+-]?(\d+\.?\d*|\.\d+)(e[+-]?\d+)?$/i;
const PATH = /^[A-Za-z_$][\w$]*(\.[\w$]+)*$/;
const QUOTED = /^(['"])(.*)\1$/s;

const LITERALS: Record<string, unknown> = { true: true, false: false, null: null, undefined: undefined };

/** Evaluates the inside of one {{…}}. Throws HoleError for anything that is not a lookup or a literal. */
export function evaluate(expression: string, scope: Scope): unknown {
  const expr = expression.trim();
  if (expr in LITERALS) return LITERALS[expr];
  if (NUMBER.test(expr)) return Number(expr);
  const quoted = QUOTED.exec(expr);
  if (quoted) return quoted[2];
  if (!PATH.test(expr)) throw new HoleError(expr);
  const [head = "", ...rest] = expr.split(".");
  let value = scope.get(head);
  for (const key of rest) {
    if (value == null) return undefined;
    value = (value as Record<string, unknown>)[key];
  }
  return value;
}

const HOLE = /\{\{(.*?)\}\}/gs;

export type Piece = { text: string } | { value: unknown };

/** Splits text into literal runs and evaluated holes, in order. */
export function pieces(text: string, scope: Scope, onError: (error: HoleError) => void): Piece[] {
  const out: Piece[] = [];
  let last = 0;
  for (const m of text.matchAll(HOLE)) {
    if (m.index > last) out.push({ text: text.slice(last, m.index) });
    try {
      out.push({ value: evaluate(m[1] ?? "", scope) });
    } catch (error) {
      if (error instanceof HoleError) onError(error);
      else throw error;
      out.push({ value: undefined });
    }
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push({ text: text.slice(last) });
  return out;
}

export function hasHoles(text: string): boolean {
  return text.includes("{{");
}

/** True when the whole string is one hole, so the attribute takes the raw value instead of a string. */
export function wholeHole(text: string): string | null {
  const m = /^\{\{(.*?)\}\}$/s.exec(text);
  return m && !m[1]?.includes("}}") ? (m[1] ?? "") : null;
}

/** How a value reads inside text or an interpolated attribute: nothing for null, undefined and booleans. */
export function toText(value: unknown): string {
  if (value == null || typeof value === "boolean") return "";
  return String(value);
}

/** Evaluates an attribute: the raw value for a whole hole, otherwise a string. */
export function attributeValue(value: string, scope: Scope, onError: (error: HoleError) => void): unknown {
  const whole = wholeHole(value);
  if (whole !== null) {
    try {
      return evaluate(whole, scope);
    } catch (error) {
      if (error instanceof HoleError) onError(error);
      else throw error;
      return undefined;
    }
  }
  if (!hasHoles(value)) return value;
  return pieces(value, scope, onError)
    .map((p) => ("text" in p ? p.text : toText(p.value)))
    .join("");
}
