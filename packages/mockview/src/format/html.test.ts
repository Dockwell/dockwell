// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { loadMock, mockFiles } from "../mocks";
import { decodeEntities, type ElementNode, findElements, parseHtml, type TemplateNode } from "./html";

function only(nodes: TemplateNode[]): ElementNode {
  const elements = nodes.filter((n): n is ElementNode => n.kind === "element");
  expect(elements).toHaveLength(1);
  return elements[0] as ElementNode;
}

describe("parseHtml", () => {
  it("reads quoted, unquoted and valueless attributes, keeping their case", () => {
    const { nodes, problems } = parseHtml(`<svg viewBox="0 0 16 16" data-x='a "b"' width=16 hidden></svg>`);
    expect(problems).toEqual([]);
    expect(only(nodes).attrs).toEqual([
      { name: "viewBox", value: "0 0 16 16" },
      { name: "data-x", value: 'a "b"' },
      { name: "width", value: "16" },
      { name: "hidden", value: null },
    ]);
  });

  it("keeps template elements where they are written, even inside tables and selects", () => {
    const { nodes } = parseHtml(
      `<table><tbody><sc-for list="{{rows}}" as="r"><tr><td>{{r}}</td></tr></sc-for></tbody></table>` +
        `<select><sc-for list="{{o}}" as="x"><option>{{x}}</option></sc-for></select>`,
    );
    const loops = findElements(nodes, (el) => el.tag === "sc-for");
    expect(loops.map((l) => l.children.map((c) => (c.kind === "element" ? c.tag : "#text")))).toEqual([
      ["tr"],
      ["option"],
    ]);
  });

  it("treats style and script content as text", () => {
    const { nodes } = parseHtml(`<style>a > b { color: red }</style><script>if (a < b) "</div>";</script>`);
    expect(nodes.map((n) => (n.kind === "element" ? n.children : null))).toEqual([
      [{ kind: "text", text: "a > b { color: red }" }],
      [{ kind: "text", text: 'if (a < b) "</div>";' }],
    ]);
  });

  it("closes void and self-closed elements on the spot", () => {
    const { nodes, problems } = parseHtml(`<p><input type="checkbox" checked><path d="M0 0"/>text</p>`);
    expect(problems).toEqual([]);
    const p = only(nodes);
    expect(p.children.map((c) => (c.kind === "element" ? c.tag : c.text))).toEqual(["input", "path", "text"]);
  });

  it("ends an unquoted value before the /> that closes the tag", () => {
    const { nodes, problems } = parseHtml(`<p><x-import name=check/><a href=a/b>x</a></p>`);
    expect(problems).toEqual([]);
    const [importEl, link] = only(nodes).children as ElementNode[];
    expect(importEl?.attrs).toEqual([{ name: "name", value: "check" }]);
    expect(importEl?.children).toEqual([]);
    expect(link?.attrs).toEqual([{ name: "href", value: "a/b" }]);
    expect(link?.children).toEqual([{ kind: "text", text: "x" }]);
  });

  it("skips the doctype and comments and decodes character references", () => {
    const { nodes } = parseHtml(`<!doctype html><!-- note --><b title="a &amp; b">x &lt; y&nbsp;&#8594;&#x2192;</b>`);
    const b = only(nodes);
    expect(b.attrs).toEqual([{ name: "title", value: "a & b" }]);
    expect(b.children).toEqual([{ kind: "text", text: "x < y →→" }]);
  });

  it("reports stray and unclosed tags instead of throwing", () => {
    const { problems } = parseHtml(`<div><span></div></b><em>`);
    expect(problems).toEqual([
      "Line 1: <span> is never closed before </div>.",
      "Line 1: </b> has no matching opening tag.",
      "Line 1: <em> is never closed.",
    ]);
  });

  it("parses every mock without guessing", async () => {
    for (const file of mockFiles) {
      expect({ file, problems: parseHtml(await loadMock(file)).problems }).toEqual({ file, problems: [] });
    }
  });
});

describe("decodeEntities", () => {
  it("leaves unknown references as written", () => {
    expect(decodeEntities("&bogus; &amp")).toBe("&bogus; &amp");
  });
});
