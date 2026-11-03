// SPDX-License-Identifier: AGPL-3.0-only

// A small, strict HTML parser for the .dc.html mock format. The browser's own
// parser would rearrange the template before we could read it: it moves
// <sc-for> out of tables, drops unknown tags inside <select> and lowercases
// attribute names. This one keeps the markup exactly as written.

export interface Attribute {
  name: string;
  /** null when the attribute is written without a value, as in <input checked>. */
  value: string | null;
}

export interface ElementNode {
  kind: "element";
  /** As written, so SVG names such as linearGradient keep their case. */
  tag: string;
  attrs: Attribute[];
  children: TemplateNode[];
  line: number;
}

export interface TextNode {
  kind: "text";
  text: string;
}

export type TemplateNode = ElementNode | TextNode;

export interface ParseResult {
  nodes: TemplateNode[];
  /** Markup the parser had to guess at, such as a stray closing tag. Empty for well-formed files. */
  problems: string[];
}

const VOID = new Set([
  "area",
  "base",
  "br",
  "col",
  "embed",
  "hr",
  "img",
  "input",
  "link",
  "meta",
  "source",
  "track",
  "wbr",
]);

/** Elements whose content is text up to their closing tag. */
const RAW_TEXT = new Set(["script", "style"]);
/** Like raw text, but character references are decoded. */
const ESCAPABLE_RAW_TEXT = new Set(["textarea", "title"]);

const NAMED_ENTITIES: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
  middot: "·",
  times: "×",
  ndash: "–",
  mdash: "—",
  hellip: "…",
  rsquo: "’",
  lsquo: "‘",
  rdquo: "”",
  ldquo: "“",
  copy: "©",
  rarr: "→",
  larr: "←",
  bull: "•",
};

/** Decodes character references. Unknown named references stay as written, as browsers do. */
export function decodeEntities(text: string): string {
  if (!text.includes("&")) return text;
  return text.replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z][a-z0-9]*);/gi, (match, ref: string) => {
    if (ref[0] === "#") {
      const code =
        ref[1] === "x" || ref[1] === "X" ? Number.parseInt(ref.slice(2), 16) : Number.parseInt(ref.slice(1), 10);
      return Number.isFinite(code) && code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : match;
    }
    return NAMED_ENTITIES[ref.toLowerCase()] ?? match;
  });
}

const TAG_NAME = /[A-Za-z][A-Za-z0-9:-]*/y;
const ATTR_NAME = /[^\s"'<>/=]+/y;
const SPACE = /\s*/y;
const UNQUOTED = /[^\s>]+/y;

function matchAt(re: RegExp, source: string, at: number): string | null {
  re.lastIndex = at;
  const m = re.exec(source);
  return m ? m[0] : null;
}

/** Parses markup into a tree. It never throws; anything it had to guess at is in problems. */
export function parseHtml(source: string): ParseResult {
  const root: ElementNode = { kind: "element", tag: "#root", attrs: [], children: [], line: 1 };
  const stack: ElementNode[] = [root];
  const problems: string[] = [];
  const lineStarts: number[] = [0];
  for (let i = 0; i < source.length; i++) if (source.charCodeAt(i) === 10) lineStarts.push(i + 1);
  const lineOf = (at: number): number => {
    let lo = 0;
    let hi = lineStarts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if ((lineStarts[mid] ?? 0) <= at) lo = mid;
      else hi = mid - 1;
    }
    return lo + 1;
  };
  const current = (): ElementNode => stack[stack.length - 1] ?? root;
  const pushText = (text: string) => {
    if (text === "") return;
    const siblings = current().children;
    const last = siblings[siblings.length - 1];
    if (last?.kind === "text") last.text += text;
    else siblings.push({ kind: "text", text });
  };

  let at = 0;
  while (at < source.length) {
    const lt = source.indexOf("<", at);
    if (lt === -1) {
      pushText(decodeEntities(source.slice(at)));
      break;
    }
    if (lt > at) pushText(decodeEntities(source.slice(at, lt)));
    at = lt;

    if (source.startsWith("<!--", at)) {
      const end = source.indexOf("-->", at + 4);
      if (end === -1) problems.push(`Line ${lineOf(at)}: a comment is never closed.`);
      at = end === -1 ? source.length : end + 3;
      continue;
    }
    if (source.startsWith("<!", at) || source.startsWith("<?", at)) {
      const end = source.indexOf(">", at);
      at = end === -1 ? source.length : end + 1;
      continue;
    }
    if (source[at + 1] === "/") {
      const name = matchAt(TAG_NAME, source, at + 2);
      if (!name) {
        pushText("<");
        at += 1;
        continue;
      }
      const end = source.indexOf(">", at);
      at = end === -1 ? source.length : end + 1;
      const lower = name.toLowerCase();
      const index = stack.findLastIndex((el) => el.tag.toLowerCase() === lower);
      if (index <= 0) {
        problems.push(`Line ${lineOf(lt)}: </${name}> has no matching opening tag.`);
        continue;
      }
      for (let i = stack.length - 1; i > index; i--) {
        const open = stack[i];
        if (open) problems.push(`Line ${open.line}: <${open.tag}> is never closed before </${name}>.`);
      }
      stack.length = index;
      continue;
    }

    const name = matchAt(TAG_NAME, source, at + 1);
    if (!name) {
      pushText("<");
      at += 1;
      continue;
    }
    const element: ElementNode = { kind: "element", tag: name, attrs: [], children: [], line: lineOf(at) };
    at += 1 + name.length;
    let selfClosing = false;
    for (;;) {
      at += matchAt(SPACE, source, at)?.length ?? 0;
      if (at >= source.length) {
        problems.push(`Line ${element.line}: <${name}> is cut off.`);
        break;
      }
      if (source[at] === ">") {
        at += 1;
        break;
      }
      if (source.startsWith("/>", at)) {
        selfClosing = true;
        at += 2;
        break;
      }
      const attrName = matchAt(ATTR_NAME, source, at);
      if (!attrName) {
        problems.push(`Line ${lineOf(at)}: unexpected "${source[at]}" inside <${name}>.`);
        at += 1;
        continue;
      }
      at += attrName.length;
      const afterSpace = at + (matchAt(SPACE, source, at)?.length ?? 0);
      if (source[afterSpace] !== "=") {
        element.attrs.push({ name: attrName, value: null });
        continue;
      }
      at = afterSpace + 1;
      at += matchAt(SPACE, source, at)?.length ?? 0;
      const quote = source[at];
      let raw: string;
      if (quote === '"' || quote === "'") {
        const end = source.indexOf(quote, at + 1);
        if (end === -1) {
          problems.push(`Line ${lineOf(at)}: the value of ${attrName} is never closed.`);
          raw = source.slice(at + 1);
          at = source.length;
        } else {
          raw = source.slice(at + 1, end);
          at = end + 1;
        }
      } else {
        raw = matchAt(UNQUOTED, source, at) ?? "";
        at += raw.length;
      }
      element.attrs.push({ name: attrName, value: decodeEntities(raw) });
    }

    current().children.push(element);
    const lower = name.toLowerCase();
    if (selfClosing || VOID.has(lower)) continue;
    if (RAW_TEXT.has(lower) || ESCAPABLE_RAW_TEXT.has(lower)) {
      const close = new RegExp(`</${lower}\\s*>`, "ig");
      close.lastIndex = at;
      const m = close.exec(source);
      const end = m ? m.index : source.length;
      if (!m) problems.push(`Line ${element.line}: <${name}> is never closed.`);
      const text = source.slice(at, end);
      if (text) element.children.push({ kind: "text", text: RAW_TEXT.has(lower) ? text : decodeEntities(text) });
      at = m ? end + m[0].length : end;
      continue;
    }
    stack.push(element);
  }
  for (let i = stack.length - 1; i > 0; i--) {
    const open = stack[i];
    if (open) problems.push(`Line ${open.line}: <${open.tag}> is never closed.`);
  }
  return { nodes: root.children, problems };
}

export function attr(element: ElementNode, name: string): string | null | undefined {
  return element.attrs.find((a) => a.name === name)?.value;
}

export function hasAttr(element: ElementNode, name: string): boolean {
  return element.attrs.some((a) => a.name === name);
}

/** Depth-first search for elements, in document order. */
export function findElements(nodes: TemplateNode[], test: (el: ElementNode) => boolean): ElementNode[] {
  const out: ElementNode[] = [];
  const walk = (list: TemplateNode[]) => {
    for (const node of list) {
      if (node.kind !== "element") continue;
      if (test(node)) out.push(node);
      walk(node.children);
    }
  };
  walk(nodes);
  return out;
}

export function textContent(nodes: TemplateNode[]): string {
  return nodes.map((n) => (n.kind === "text" ? n.text : textContent(n.children))).join("");
}
