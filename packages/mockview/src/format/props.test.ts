// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it, vi } from "vitest";
import { Scope } from "./holes";
import { parseHtml } from "./html";
import { componentProps, elementProps, kebabToCamel, parseStyle } from "./props";

const scope = new Scope({ on: true, off: false, pick: () => {}, bg: "var(--canvas)", extra: { style: { flex: 1 } } });

function attrsOf(markup: string) {
  const [el] = parseHtml(markup).nodes;
  if (el?.kind !== "element") throw new Error("expected an element");
  return el.attrs;
}

describe("parseStyle", () => {
  it("turns declarations into a React style object", () => {
    expect(
      parseStyle("display: flex; font: 500 13px/1 var(--font-mono); --accent: red; -webkit-line-clamp: 2;"),
    ).toEqual({ display: "flex", font: "500 13px/1 var(--font-mono)", "--accent": "red", WebkitLineClamp: "2" });
  });

  it("ignores semicolons inside url() and quotes, and drops empty declarations", () => {
    expect(parseStyle(`background: url("data:image/svg+xml;utf8,<svg/>"); content: ';'; border-top: ;`)).toEqual({
      background: `url("data:image/svg+xml;utf8,<svg/>")`,
      content: "';'",
    });
  });
});

describe("elementProps", () => {
  it("uses React's names for HTML and SVG attributes", () => {
    const props = elementProps(
      "label",
      attrsOf(
        `<label class="a" for="x" tabindex="-1" colspan="2" stroke-width="1.5" vector-effect="non-scaling-stroke" aria-hidden="true" data-theme="dark" viewBox="0 0 1 1">`,
      ),
      scope,
      vi.fn(),
    );
    expect(props).toEqual({
      className: "a",
      htmlFor: "x",
      tabIndex: "-1",
      colSpan: "2",
      strokeWidth: "1.5",
      vectorEffect: "non-scaling-stroke",
      "aria-hidden": "true",
      "data-theme": "dark",
      viewBox: "0 0 1 1",
    });
  });

  it("fills style holes before parsing the style", () => {
    expect(elementProps("div", attrsOf(`<div style="background: {{bg}}; color: red">`), scope, vi.fn())).toEqual({
      style: { background: "var(--canvas)", color: "red" },
    });
  });

  it("makes form fields uncontrolled, as on a static page", () => {
    expect(elementProps("input", attrsOf(`<input value="hz-1" readonly>`), scope, vi.fn())).toEqual({
      defaultValue: "hz-1",
      readOnly: true,
    });
    expect(elementProps("input", attrsOf(`<input type="radio" value="new" checked>`), scope, vi.fn())).toEqual({
      type: "radio",
      value: "new",
      defaultChecked: true,
    });
    expect(elementProps("input", attrsOf(`<input type="checkbox" checked="{{off}}">`), scope, vi.fn())).toEqual({
      type: "checkbox",
      defaultChecked: false,
    });
  });

  it("passes handlers through and leaves out undefined values", () => {
    const props = elementProps("button", attrsOf(`<button onClick="{{pick}}" title="{{missing}}">`), scope, vi.fn());
    expect(props).toEqual({ onClick: scope.get("pick") });
  });
});

describe("componentProps", () => {
  it("turns kebab-case into camelCase, keeps aria and data, and spreads dc-props", () => {
    const props = componentProps(
      attrsOf(
        `<x-import component-from-global-scope="Dockwell.Button" on-row-click="{{pick}}" default-value="a" size="{{16}}" mono aria-label="Copy" dc-props="{{extra}}" class="c" hint-size="1,2">`,
      ),
      scope,
      vi.fn(),
      (name) => name === "component-from-global-scope" || name.startsWith("hint-"),
    );
    expect(props).toEqual({
      onRowClick: scope.get("pick"),
      defaultValue: "a",
      size: 16,
      mono: true,
      "aria-label": "Copy",
      style: { flex: 1 },
      className: "c",
    });
  });

  it("parses a style string", () => {
    expect(componentProps(attrsOf(`<x-import style="width: 200px;">`), scope, vi.fn(), () => false)).toEqual({
      style: { width: "200px" },
    });
  });
});

it("kebabToCamel", () => {
  expect(kebabToCamel("on-row-click")).toBe("onRowClick");
  expect(kebabToCamel("icon-after")).toBe("iconAfter");
});
