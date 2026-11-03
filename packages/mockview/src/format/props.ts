// SPDX-License-Identifier: AGPL-3.0-only

// Turns template attributes into React props. HTML and SVG attribute names
// become the names React expects (class → className, stroke-width →
// strokeWidth), inline styles become style objects, and form fields become
// uncontrolled, the way a static page behaves.

import type { CSSProperties } from "react";
import { attributeValue, type HoleError, type Scope, wholeHole } from "./holes";
import type { Attribute } from "./html";

const REACT_NAMES: Record<string, string> = {
  class: "className",
  for: "htmlFor",
  accesskey: "accessKey",
  allowfullscreen: "allowFullScreen",
  autocapitalize: "autoCapitalize",
  autocomplete: "autoComplete",
  autofocus: "autoFocus",
  autoplay: "autoPlay",
  cellpadding: "cellPadding",
  cellspacing: "cellSpacing",
  charset: "charSet",
  colspan: "colSpan",
  contenteditable: "contentEditable",
  crossorigin: "crossOrigin",
  datetime: "dateTime",
  enterkeyhint: "enterKeyHint",
  fetchpriority: "fetchPriority",
  formaction: "formAction",
  formnovalidate: "formNoValidate",
  frameborder: "frameBorder",
  inputmode: "inputMode",
  maxlength: "maxLength",
  minlength: "minLength",
  novalidate: "noValidate",
  playsinline: "playsInline",
  popovertarget: "popoverTarget",
  readonly: "readOnly",
  referrerpolicy: "referrerPolicy",
  rowspan: "rowSpan",
  spellcheck: "spellCheck",
  srcset: "srcSet",
  tabindex: "tabIndex",
  usemap: "useMap",
  "xlink:href": "xlinkHref",
  "xml:lang": "xmlLang",
  "xml:space": "xmlSpace",
  "xmlns:xlink": "xmlnsXlink",
};

/** Attributes whose presence means true, whatever their value. */
const BOOLEAN = new Set([
  "allowfullscreen",
  "async",
  "autofocus",
  "autoplay",
  "checked",
  "controls",
  "default",
  "defer",
  "disabled",
  "formnovalidate",
  "hidden",
  "inert",
  "itemscope",
  "loop",
  "multiple",
  "muted",
  "nomodule",
  "novalidate",
  "open",
  "playsinline",
  "readonly",
  "required",
  "reversed",
  "selected",
]);

/** Input types whose value attribute is a label, not something the person types. */
const FIXED_VALUE_INPUTS = new Set(["button", "checkbox", "hidden", "image", "radio", "reset", "submit"]);

export function kebabToCamel(name: string): string {
  return name.replace(/-([a-z0-9])/g, (_, c: string) => c.toUpperCase());
}

function styleKey(property: string): string {
  if (property.startsWith("--")) return property;
  const lower = property.toLowerCase();
  if (lower.startsWith("-ms-")) return kebabToCamel(lower.slice(1));
  if (lower.startsWith("-")) {
    const camel = kebabToCamel(lower.slice(1));
    return camel.charAt(0).toUpperCase() + camel.slice(1);
  }
  return kebabToCamel(lower);
}

/** Splits CSS declarations on semicolons that are outside quotes and parentheses. */
function declarations(css: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let quote: string | null = null;
  let start = 0;
  for (let i = 0; i < css.length; i++) {
    const c = css[i];
    if (quote) {
      if (c === "\\") i++;
      else if (c === quote) quote = null;
    } else if (c === '"' || c === "'") quote = c;
    else if (c === "(") depth++;
    else if (c === ")") depth = Math.max(0, depth - 1);
    else if (c === ";" && depth === 0) {
      out.push(css.slice(start, i));
      start = i + 1;
    }
  }
  out.push(css.slice(start));
  return out;
}

/** Parses an inline style attribute into a React style object. Empty declarations are dropped. */
export function parseStyle(css: string): CSSProperties {
  const style: Record<string, string> = {};
  for (const decl of declarations(css)) {
    const colon = decl.indexOf(":");
    if (colon === -1) continue;
    const property = decl.slice(0, colon).trim();
    const value = decl.slice(colon + 1).trim();
    if (property && value) style[styleKey(property)] = value;
  }
  return style as CSSProperties;
}

function styleValue(value: unknown): CSSProperties | undefined {
  if (value == null || value === "") return undefined;
  if (typeof value === "string") return parseStyle(value);
  if (typeof value === "object") return value as CSSProperties;
  return undefined;
}

function reactName(name: string): string {
  if (name.startsWith("aria-") || name.startsWith("data-")) return name;
  const known = REACT_NAMES[name.toLowerCase()];
  if (known) return known;
  return name.includes("-") ? kebabToCamel(name) : name;
}

type OnHoleError = (error: HoleError) => void;

/** Props for a plain HTML or SVG element. */
export function elementProps(
  tag: string,
  attrs: readonly Attribute[],
  scope: Scope,
  onError: OnHoleError,
): Record<string, unknown> {
  const props: Record<string, unknown> = {};
  for (const { name, value } of attrs) {
    const lower = name.toLowerCase();
    if (lower === "style") {
      const style = styleValue(value === null ? undefined : attributeValue(value, scope, onError));
      if (style) props["style"] = style;
      continue;
    }
    let v: unknown;
    if (value === null) v = BOOLEAN.has(lower) ? true : "";
    else if (BOOLEAN.has(lower) && wholeHole(value) === null) v = true;
    else v = attributeValue(value, scope, onError);
    if (v === undefined) continue;
    props[reactName(name)] = v;
  }

  const lowerTag = tag.toLowerCase();
  const controlled = "onChange" in props || "onInput" in props;
  if (lowerTag === "input" && !controlled) {
    const type = typeof props["type"] === "string" ? props["type"].toLowerCase() : "text";
    if ("value" in props && !FIXED_VALUE_INPUTS.has(type)) {
      props["defaultValue"] = props["value"];
      delete props["value"];
    }
    if ("checked" in props) {
      props["defaultChecked"] = props["checked"];
      delete props["checked"];
    }
  }
  if (lowerTag === "select" && !controlled && "value" in props) {
    props["defaultValue"] = props["value"];
    delete props["value"];
  }
  return props;
}

/**
 * Props for a design-system component or an imported artboard. Names go from
 * kebab-case to camelCase (on-row-click → onRowClick), except aria-* and
 * data-*, and dc-props="{{object}}" spreads an object of props in place.
 */
export function componentProps(
  attrs: readonly Attribute[],
  scope: Scope,
  onError: OnHoleError,
  skip: (name: string) => boolean,
): Record<string, unknown> {
  const props: Record<string, unknown> = {};
  for (const { name, value } of attrs) {
    if (skip(name)) continue;
    const v = value === null ? true : attributeValue(value, scope, onError);
    if (name === "dc-props") {
      if (v && typeof v === "object") Object.assign(props, v);
      continue;
    }
    if (name === "style") {
      const style = styleValue(v);
      if (style) props["style"] = style;
      continue;
    }
    if (name === "class") props["className"] = v;
    else if (name.startsWith("aria-") || name.startsWith("data-")) props[name] = v;
    else props[kebabToCamel(name)] = v;
  }
  return props;
}
