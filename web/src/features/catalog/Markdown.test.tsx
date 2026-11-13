// SPDX-License-Identifier: AGPL-3.0-only

import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Markdown } from "./Markdown";

function html(text: string): string {
  const { container } = render(<Markdown text={text} />);
  return (container.firstElementChild as HTMLElement).innerHTML;
}

describe("A README, as the panel shows it", () => {
  it("makes headings, paragraphs, lists, quotes, code and tables into elements, a heading below its section", () => {
    expect(
      html(
        "# Notes\n\n## Introduction\n\n**Notes** keeps\nyour notes.\n\n### More\n\n- One\n  - Under one\n- Two, with `code`\n\n1. First\n2. Second\n\n> A quote\n\n```sh\necho <b>\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |",
      ),
    ).toBe(
      "<h3>Notes</h3><h3>Introduction</h3><p><strong>Notes</strong> keeps your notes.</p><h4>More</h4>" +
        "<ul><li>One<ul><li>Under one</li></ul></li><li>Two, with <code>code</code></li></ul>" +
        "<ol><li>First</li><li>Second</li></ol><blockquote>A quote</blockquote>" +
        "<pre><code>echo &lt;b&gt;</code></pre>" +
        '<div class="dw-markdown-table"><table><thead><tr><th>A</th><th>B</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table></div>',
    );
  });

  it("opens only an HTTPS link, in a new tab with no referrer, and shows anything else as text", () => {
    expect(html("[Docs](https://example.com/docs) and <https://example.com>")).toBe(
      '<p><a href="https://example.com/docs" title="https://example.com/docs" target="_blank" rel="noreferrer noopener nofollow">Docs</a> and ' +
        '<a href="https://example.com/" target="_blank" rel="noreferrer noopener nofollow">https://example.com</a></p>',
    );
    for (const href of [
      "javascript:alert(1)",
      "http://example.com",
      "https://user:pw@example.com",
      "/catalog",
      "data:text/html,x",
    ]) {
      expect(html(`[click](${href})`)).toBe("<p><span>click</span></p>");
    }
  });

  it("shows HTML and images as text, never as markup", () => {
    expect(html('<img src="https://example.com/a.png" onerror="alert(1)">')).toBe(
      '<p>&lt;img src="https://example.com/a.png" onerror="alert(1)"&gt;</p>',
    );
    expect(html("![alt](https://example.com/a.png)")).toBe(
      '<p>!<a href="https://example.com/a.png" title="https://example.com/a.png" target="_blank" rel="noreferrer noopener nofollow">alt</a></p>',
    );
    expect(html("<script>alert(1)</script>")).toBe("<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>");
  });

  it("reads emphasis, escapes and snake_case words as a README means them", () => {
    expect(html("*one* and _two_ in snake_case_name, \\*not\\*, and **bold _nested_**")).toBe(
      "<p><em>one</em> and <em>two</em> in snake_case_name, *not*, and <strong>bold <em>nested</em></strong></p>",
    );
    expect(html("a * b * c and 2 * 3")).toBe("<p>a * b * c and 2 * 3</p>");
  });

  it("drops a rule and keeps an unclosed fence's code to the end", () => {
    expect(html("one\n\n---\n\n```\ncode")).toBe("<p>one</p><pre><code>code</code></pre>");
  });

  it("says where a link really goes when its words name another host", () => {
    expect(html("[https://bank.example.com/login](https://evil.example.net/x)")).toBe(
      '<p><a href="https://evil.example.net/x" title="https://evil.example.net/x" target="_blank" rel="noreferrer noopener nofollow">' +
        'https://bank.example.com/login<span class="dw-markdown-host"> (evil.example.net)</span></a></p>',
    );
    expect(html("[the docs at example.com](https://www.example.com/docs)")).toBe(
      '<p><a href="https://www.example.com/docs" title="https://www.example.com/docs" target="_blank" rel="noreferrer noopener nofollow">the docs at example.com</a></p>',
    );
    expect(html("[Read the docs](https://example.net/docs)")).not.toContain("dw-markdown-host");
  });
});
