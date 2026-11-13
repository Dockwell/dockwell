// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from "react";

import { projectLink } from "./media";

/**
 * A catalog's README, as the subset of Markdown the panel shows (ADR 0083): headings,
 * paragraphs, lists, quotes, tables, emphasis, code and links, each made into an
 * element, never into HTML. A link opens only as an HTTPS address, in a new tab, with
 * no referrer; anything else it names, and any HTML or image the text holds, reads as
 * text. A heading is a level below the section it's in.
 */
export function Markdown({ text, className }: { text: string; className?: string }) {
  return <div className={className}>{blocks(text)}</div>;
}

type Block =
  | { kind: "heading"; level: number; text: string }
  | { kind: "paragraph"; text: string }
  | { kind: "code"; text: string }
  | { kind: "quote"; text: string }
  | { kind: "list"; ordered: boolean; items: Item[] }
  | { kind: "table"; head: string[]; rows: string[][] };

interface Item {
  text: string;
  /** Items under it, one level down, as a README nests them at most. */
  items: Item[];
}

const FENCE = /^ {0,3}(`{3,}|~{3,})/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const BULLET = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const RULE = /^ {0,3}([-*_])(\s*\1){2,}\s*$/;
const TABLE_RULE = /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$/;

/** Reads the text's blocks, line by line. */
export function parse(text: string): Block[] {
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i] ?? "";
    if (line.trim() === "" || RULE.test(line)) {
      i++;
      continue;
    }
    const fence = FENCE.exec(line);
    if (fence) {
      const close = fence[1] ?? "```";
      const code: string[] = [];
      i++;
      while (i < lines.length && !(lines[i] ?? "").trimStart().startsWith(close)) code.push(lines[i++] ?? "");
      i++;
      out.push({ kind: "code", text: code.join("\n") });
      continue;
    }
    const heading = HEADING.exec(line);
    if (heading) {
      out.push({ kind: "heading", level: heading[1]?.length ?? 1, text: heading[2] ?? "" });
      i++;
      continue;
    }
    if (line.trimStart().startsWith(">")) {
      const quote: string[] = [];
      while (i < lines.length && (lines[i] ?? "").trimStart().startsWith(">"))
        quote.push((lines[i++] ?? "").trimStart().replace(/^>\s?/, ""));
      out.push({ kind: "quote", text: quote.join(" ") });
      continue;
    }
    if (line.includes("|") && TABLE_RULE.test(lines[i + 1] ?? "")) {
      const head = cells(line);
      const rows: string[][] = [];
      i += 2;
      while (i < lines.length && (lines[i] ?? "").includes("|") && (lines[i] ?? "").trim() !== "")
        rows.push(cells(lines[i++] ?? ""));
      out.push({ kind: "table", head, rows });
      continue;
    }
    const bullet = BULLET.exec(line);
    if (bullet) {
      const ordered = /\d/.test(bullet[2] ?? "");
      const base = (bullet[1] ?? "").length;
      const items: Item[] = [];
      while (i < lines.length) {
        const l = lines[i] ?? "";
        const b = BULLET.exec(l);
        if (b) {
          const item = { text: b[3] ?? "", items: [] };
          const last = items.at(-1);
          if ((b[1] ?? "").length > base && last) last.items.push(item);
          else items.push(item);
          i++;
        } else if (l.trim() !== "" && /^\s+/.test(l) && items.length > 0) {
          // A line that carries on the item above it.
          const last = items.at(-1) as Item;
          const into = last.items.at(-1) ?? last;
          into.text += ` ${l.trim()}`;
          i++;
        } else break;
      }
      out.push({ kind: "list", ordered, items });
      continue;
    }
    const paragraph: string[] = [];
    while (i < lines.length) {
      const l = lines[i] ?? "";
      if (l.trim() === "" || FENCE.test(l) || HEADING.test(l) || BULLET.test(l) || l.trimStart().startsWith(">")) break;
      paragraph.push(l.trim());
      i++;
    }
    out.push({ kind: "paragraph", text: paragraph.join(" ") });
  }
  return out;
}

/** A table row's cells, without the pipes at its ends. */
function cells(line: string): string[] {
  return line
    .trim()
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .split("|")
    .map((c) => c.trim());
}

function blocks(text: string): ReactNode[] {
  return parse(text).map((b, n) => {
    const key = `${n}`;
    switch (b.kind) {
      case "heading":
        // The section the README is in has its own heading, so these sit under it.
        return b.level <= 2 ? <h3 key={key}>{inline(b.text)}</h3> : <h4 key={key}>{inline(b.text)}</h4>;
      case "code":
        return (
          <pre key={key}>
            <code>{b.text}</code>
          </pre>
        );
      case "quote":
        return <blockquote key={key}>{inline(b.text)}</blockquote>;
      case "list":
        return list(b.ordered, b.items, key);
      case "table":
        return (
          <div key={key} className="dw-markdown-table">
            <table>
              <thead>
                <tr>
                  {b.head.map((c, j) => (
                    // biome-ignore lint/suspicious/noArrayIndexKey: a table's columns are in a fixed order
                    <th key={j}>{inline(c)}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {b.rows.map((row, r) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: a table's rows are in a fixed order
                  <tr key={r}>
                    {row.map((c, j) => (
                      // biome-ignore lint/suspicious/noArrayIndexKey: a table's columns are in a fixed order
                      <td key={j}>{inline(c)}</td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        );
      default:
        return <p key={key}>{inline(b.text)}</p>;
    }
  });
}

function list(ordered: boolean, items: Item[], key: string): ReactNode {
  const children = items.map((item, j) => (
    // biome-ignore lint/suspicious/noArrayIndexKey: a list's items are in a fixed order
    <li key={j}>
      {inline(item.text)}
      {item.items.length > 0 ? list(false, item.items, `${key}.${j}`) : null}
    </li>
  ));
  return ordered ? <ol key={key}>{children}</ol> : <ul key={key}>{children}</ul>;
}

/** A host as written, such as example.com in "https://www.example.com/docs" or "example.com". */
const HOST = /(?:^|[\s/@(])((?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63})(?=$|[\s/:)?#.,])/i;

/** The address's host where a link's words name another, as "example.org" for [example.com](https://example.org). */
export function otherHost(words: string, href: string): string | undefined {
  const named = HOST.exec(` ${words}`)?.[1]?.toLowerCase();
  if (!named) return undefined;
  const real = new URL(href).hostname.toLowerCase();
  const bare = (host: string) => host.replace(/^www\./, "");
  return bare(named) === bare(real) ? undefined : real;
}

/** What the text says inline: code, links, strong and emphasised words, and the rest as text. */
export function inline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let plain = "";
  const flush = () => {
    if (plain) out.push(plain);
    plain = "";
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i] as string;
    if (c === "\\" && i + 1 < text.length && /[\\`*_[\]()<>#!|-]/.test(text[i + 1] as string)) {
      plain += text[i + 1];
      i += 2;
      continue;
    }
    if (c === "`") {
      const run = /^`+/.exec(text.slice(i))?.[0] ?? "`";
      const end = text.indexOf(run, i + run.length);
      if (end > 0) {
        flush();
        out.push(<code key={out.length}>{text.slice(i + run.length, end).trim()}</code>);
        i = end + run.length;
        continue;
      }
    }
    if (c === "[") {
      // An address may hold parentheses that pair up, as a wiki's links do.
      const link = /^\[([^\]]*)\]\(\s*((?:[^()\s]|\([^()\s]*\))+)(?:\s+"[^"]*")?\s*\)/.exec(text.slice(i));
      if (link) {
        flush();
        const href = projectLink(link[2] ?? "");
        const words = inline(link[1] || link[2] || "");
        // Words that name one host over an address of another say where it really goes.
        const other = href ? otherHost(link[1] ?? "", href) : undefined;
        out.push(
          href ? (
            <a key={out.length} href={href} title={href} target="_blank" rel="noreferrer noopener nofollow">
              {words}
              {other ? <span className="dw-markdown-host"> ({other})</span> : null}
            </a>
          ) : (
            <span key={out.length}>{words}</span>
          ),
        );
        i += link[0].length;
        continue;
      }
    }
    if (c === "<") {
      const auto = /^<(https:\/\/[^>\s]+)>/.exec(text.slice(i));
      const href = auto ? projectLink(auto[1] ?? "") : undefined;
      if (auto && href) {
        flush();
        out.push(
          <a key={out.length} href={href} target="_blank" rel="noreferrer noopener nofollow">
            {auto[1]}
          </a>,
        );
        i += auto[0].length;
        continue;
      }
    }
    if ((c === "*" || c === "_") && text[i + 1] === c) {
      const end = text.indexOf(c + c, i + 2);
      if (end > i + 2) {
        flush();
        out.push(<strong key={out.length}>{inline(text.slice(i + 2, end))}</strong>);
        i = end + 2;
        continue;
      }
    }
    if ((c === "*" || c === "_") && text[i + 1] !== " " && (c === "*" || i === 0 || /\W/.test(text[i - 1] as string))) {
      const end = text.indexOf(c, i + 1);
      if (end > i + 1 && text[end - 1] !== " " && (c === "*" || !/\w/.test(text[end + 1] ?? ""))) {
        flush();
        out.push(<em key={out.length}>{inline(text.slice(i + 1, end))}</em>);
        i = end + 1;
        continue;
      }
    }
    plain += c;
    i++;
  }
  flush();
  return out;
}
