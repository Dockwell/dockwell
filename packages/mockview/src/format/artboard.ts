// SPDX-License-Identifier: AGPL-3.0-only

import { attr, type ElementNode, findElements, hasAttr, parseHtml, type TemplateNode, textContent } from "./html";

/** One entry of data-props: how Claude Design's tweak panel edits a prop, and its starting value. */
export interface PropSpec {
  editor?: string | null;
  default?: unknown;
  options?: unknown[];
}

export interface Artboard {
  /** File name inside design/mocks, such as "Overview-Single.dc.html". */
  file: string;
  title: string;
  /** Children of <helmet>: page styles and any stylesheet links. */
  head: ElementNode[];
  /** Everything inside <x-dc> except <helmet>. */
  template: TemplateNode[];
  /** Source of `class Component extends DCLogic`, or null when the artboard has no logic. */
  script: string | null;
  props: Record<string, PropSpec>;
  /** Files this artboard pulls in with <dc-import>, in document order and without repeats. */
  imports: string[];
  /** Markup the parser had to guess at. Empty for a well-formed file. */
  problems: string[];
}

export class ArtboardError extends Error {
  constructor(file: string, message: string) {
    super(`${file}: ${message}`);
    this.name = "ArtboardError";
  }
}

/** The file a <dc-import name="…"> refers to. */
export function importFile(name: string): string {
  return name.endsWith(".dc.html") ? name : `${name}.dc.html`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseProps(file: string, raw: string | null | undefined): Artboard["props"] {
  if (raw == null || raw.trim() === "") return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (error) {
    throw new ArtboardError(file, `data-props is not valid JSON (${(error as Error).message}). Fix the attribute.`);
  }
  if (!isRecord(parsed)) throw new ArtboardError(file, "data-props must be a JSON object. Fix the attribute.");
  const props: Record<string, PropSpec> = {};
  for (const [name, spec] of Object.entries(parsed)) {
    // $preview is the artboard's size, which the viewer reads from canvas.json instead. It isn't a prop.
    if (name === "$preview") continue;
    props[name] = isRecord(spec) ? (spec as PropSpec) : {};
  }
  return props;
}

/** Reads one .dc.html file. Throws ArtboardError when the file is not an artboard at all. */
export function parseArtboard(file: string, source: string): Artboard {
  const { nodes, problems } = parseHtml(source);
  const [dc] = findElements(nodes, (el) => el.tag === "x-dc");
  if (!dc) throw new ArtboardError(file, "there is no <x-dc> element, so this is not a Claude Design artboard.");

  const [titleEl] = findElements(nodes, (el) => el.tag === "title");
  const title = titleEl ? textContent(titleEl.children).trim() : file.replace(/\.dc\.html$/, "");

  const head: ElementNode[] = [];
  const template: TemplateNode[] = [];
  for (const node of dc.children) {
    if (node.kind === "element" && node.tag === "helmet") {
      for (const child of node.children) if (child.kind === "element") head.push(child);
    } else {
      template.push(node);
    }
  }

  const [scriptEl] = findElements(nodes, (el) => el.tag === "script" && hasAttr(el, "data-dc-script"));
  const script = scriptEl ? textContent(scriptEl.children) : null;
  const props = parseProps(file, scriptEl ? attr(scriptEl, "data-props") : null);

  const imports: string[] = [];
  for (const el of findElements(template, (e) => e.tag === "dc-import")) {
    const name = attr(el, "name");
    if (name && !name.includes("{{")) {
      const target = importFile(name);
      if (!imports.includes(target)) imports.push(target);
    }
  }

  return { file, title, head, template, script, props, imports, problems };
}

/** The props an artboard starts with: every data-props default. */
export function propDefaults(artboard: Artboard): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, spec] of Object.entries(artboard.props)) {
    if (spec.default !== undefined) out[name] = spec.default;
  }
  return out;
}
