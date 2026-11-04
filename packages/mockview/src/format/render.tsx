// SPDX-License-Identifier: AGPL-3.0-only

// Renders artboards as React trees. Each artboard becomes a class component
// that extends the artboard's own `class Component extends DCLogic`, so
// this.props, state and lifecycle methods work as they do in Claude Design;
// its render() fills the template with what renderVals() returns.

import * as ReactModule from "react";
import {
  type ComponentType,
  cloneElement,
  createContext,
  createElement,
  type ErrorInfo,
  Fragment,
  isValidElement,
  type ReactElement,
  type ReactNode,
  useContext,
} from "react";

import { type Artboard, ArtboardError, importFile, parseArtboard, propDefaults } from "./artboard";
import { attributeValue, type HoleError, pieces, Scope } from "./holes";
import { attr, type ElementNode, type TemplateNode } from "./html";
import { componentProps, elementProps } from "./props";

export type Props = Record<string, unknown>;

/** Something in an artboard that did not render as written. */
export interface Problem {
  /** The artboard whose markup or script has the problem. */
  file: string;
  message: string;
  error?: unknown;
}

/** The base class of every artboard's logic. Artboards override renderVals(), never render(). */
export class DCLogic extends ReactModule.Component<Props, Props> {
  override state: Props = {};

  renderVals(): Props {
    return {};
  }
}

type LogicClass = typeof DCLogic;

export interface LibraryOptions {
  /** Returns an artboard's source by file name, such as "Rail.dc.html". */
  load: (file: string) => string | Promise<string>;
  /** Resolves component-from-global-scope, such as "Dockwell.Button". Defaults to a lookup on globalThis. */
  resolveComponent?: (path: string) => unknown;
  /** Called once for each distinct problem. */
  onProblem?: (problem: Problem) => void;
}

/** Whitespace between these elements' children is not allowed in the DOM tree React builds. */
const NO_WHITESPACE = new Set(["table", "thead", "tbody", "tfoot", "tr", "colgroup", "select", "optgroup"]);
const CONTROL_PREFIXES = ["sc-", "dc-", "x-"];
const MAX_IMPORT_DEPTH = 16;

function lookupGlobal(path: string): unknown {
  let value: unknown = globalThis;
  for (const key of path.split(".")) {
    if (value == null) return undefined;
    value = (value as Record<string, unknown>)[key];
  }
  return value;
}

function isComponentType(value: unknown): value is ComponentType<Props> {
  return typeof value === "function" || (typeof value === "object" && value !== null && "$$typeof" in value);
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/**
 * The visible stand-in for anything that failed to render. It's a span, so
 * it's valid wherever the failed part was, even inside a <p>; inline, it sits
 * in a line of text the way the text hole it replaces would have.
 */
function ProblemBox({ text, inline = false }: { text: string; inline?: boolean }) {
  return (
    <span
      role="alert"
      data-mockview-problem=""
      style={{
        display: inline ? "inline-block" : "block",
        boxSizing: "border-box",
        padding: "6px 8px",
        border: "1px dashed var(--critical, #c62828)",
        borderRadius: 4,
        color: "var(--critical, #c62828)",
        background: "var(--critical-soft, #fdecea)",
        font: "500 12px/16px var(--font-mono, monospace)",
        whiteSpace: "pre-wrap",
      }}
    >
      {text}
    </span>
  );
}

interface BoundaryProps {
  label: string;
  file: string;
  report: (problem: Problem) => void;
  children?: ReactNode;
}

/** Keeps one broken component from blanking the whole artboard, and says which one broke. */
class Boundary extends ReactModule.Component<BoundaryProps, { error: unknown }> {
  override state: { error: unknown } = { error: null };

  static getDerivedStateFromError(error: unknown) {
    return { error };
  }

  override componentDidCatch(error: unknown, _info: ErrorInfo) {
    this.props.report({
      file: this.props.file,
      message: `${this.props.label} failed to render: ${message(error)}`,
      error,
    });
  }

  override render() {
    if (this.state.error != null) return <ProblemBox text={`${this.props.label}: ${message(this.state.error)}`} />;
    return this.props.children;
  }
}

/** The chain of artboards being imported, outermost first, to catch an artboard importing itself. */
const ImportChain = createContext<readonly string[]>([]);

/** Holds parsed artboards and the React component for each. */
export class MockLibrary {
  readonly #options: LibraryOptions;
  readonly #artboards = new Map<string, Artboard>();
  /** One read and parse per file, shared by every load() that needs it. A failed one is dropped, so it can be retried. */
  readonly #parsing = new Map<string, Promise<Artboard>>();
  readonly #components = new Map<string, ComponentType<Props>>();
  readonly #reported = new Set<string>();

  constructor(options: LibraryOptions) {
    this.#options = options;
  }

  readonly report = (problem: Problem): void => {
    const key = `${problem.file}\n${problem.message}`;
    if (this.#reported.has(key)) return;
    this.#reported.add(key);
    this.#options.onProblem?.(problem);
  };

  /**
   * Loads and parses an artboard and, transitively, everything it imports. It
   * resolves once all of them are parsed, however many loads run at once. An
   * import that fails is reported, and the artboard shows a problem in its place.
   */
  async load(file: string): Promise<Artboard> {
    const artboard = await this.#parse(file);
    const seen = new Set([file]);
    let level = [artboard];
    // Breadth first, each file once, so an artboard that imports itself doesn't wait on itself.
    while (level.length > 0) {
      const imported = await Promise.all(
        level.flatMap((from) =>
          from.imports.map((child) =>
            this.#parse(child).catch((error: unknown) => {
              this.report({ file: from.file, message: `Can't import ${child}: ${message(error)}`, error });
              return undefined;
            }),
          ),
        ),
      );
      level = [];
      for (const next of imported) {
        if (next && !seen.has(next.file)) {
          seen.add(next.file);
          level.push(next);
        }
      }
    }
    return artboard;
  }

  #parse(file: string): Promise<Artboard> {
    let pending = this.#parsing.get(file);
    if (!pending) {
      pending = (async () => {
        const artboard = parseArtboard(file, await this.#options.load(file));
        this.#artboards.set(file, artboard);
        for (const problem of artboard.problems) this.report({ file, message: problem });
        return artboard;
      })();
      this.#parsing.set(file, pending);
      pending.catch(() => this.#parsing.delete(file));
    }
    return pending;
  }

  artboard(file: string): Artboard | undefined {
    return this.#artboards.get(file);
  }

  /** The props an artboard renders with: its data-props defaults, overridden by the ones given. */
  props(file: string, given: Props = {}): Props {
    const artboard = this.#artboards.get(file);
    return artboard ? { ...propDefaults(artboard), ...given } : given;
  }

  /** The root element for one artboard, for createRoot().render(). */
  element(file: string, props: Props = {}): ReactElement {
    return (
      <ImportChain.Provider value={[file]}>
        <Boundary label={file} file={file} report={this.report}>
          {this.#renderArtboard(file, props)}
        </Boundary>
      </ImportChain.Provider>
    );
  }

  #renderArtboard(file: string, props: Props): ReactNode {
    const artboard = this.#artboards.get(file);
    if (!artboard) {
      const text = `${file} isn't loaded. Call load("${file}") before rendering it.`;
      this.report({ file, message: text });
      return <ProblemBox text={text} />;
    }
    return createElement(this.component(file), this.props(file, props));
  }

  /** The class component for an artboard, built once from its logic script. */
  component(file: string): ComponentType<Props> {
    const cached = this.#components.get(file);
    if (cached) return cached;
    const found = this.#artboards.get(file);
    if (!found) throw new ArtboardError(file, "the artboard isn't loaded yet.");
    const artboard: Artboard = found;
    let Logic: LogicClass;
    try {
      Logic = evaluateLogic(artboard);
    } catch (error) {
      const text = `The logic script doesn't run: ${message(error)}`;
      this.report({ file, message: text, error });
      Logic = DCLogic;
    }
    const library = this;
    class BoundArtboard extends Logic {
      static displayName = file.replace(/\.dc\.html$/, "");

      override render(): ReactNode {
        return library.renderTemplate(artboard, this.renderVals() ?? {});
      }
    }
    this.#components.set(file, BoundArtboard);
    return BoundArtboard;
  }

  /** Fills an artboard's template with values, as its render() does. */
  renderTemplate(artboard: Artboard, vals: Props): ReactNode {
    const ctx = new RenderContext(this, artboard.file);
    const scope = new Scope(vals);
    const head = artboard.head.flatMap((el, i) => ctx.head(el, scope, `mockview:${artboard.file}:${i}`));
    const body = ctx.nodes(artboard.template, scope, "#root");
    return createElement(Fragment, null, ...head, ...body);
  }

  resolveComponent(path: string): unknown {
    return (this.#options.resolveComponent ?? lookupGlobal)(path);
  }

  /** Renders <dc-import>: the imported artboard, with its defaults and the given props. */
  readonly DcImport = ({ file, from, props }: { file: string; from: string; props: Props }): ReactNode => {
    const chain = useContext(ImportChain);
    if (chain.includes(file) || chain.length >= MAX_IMPORT_DEPTH) {
      const text = `${from} imports ${file} inside itself (${[...chain, file].join(" → ")}). Remove one of the imports.`;
      this.report({ file: from, message: text });
      return <ProblemBox text={text} />;
    }
    if (!this.#artboards.has(file)) {
      const text = `${from} imports ${file}, which doesn't exist or isn't loaded.`;
      this.report({ file: from, message: text });
      return <ProblemBox text={text} />;
    }
    return (
      <ImportChain.Provider value={[...chain, file]}>
        <Boundary label={`<dc-import name="${file.replace(/\.dc\.html$/, "")}">`} file={from} report={this.report}>
          {this.#renderArtboard(file, props)}
        </Boundary>
      </ImportChain.Provider>
    );
  };
}

/** Gives unkeyed elements in an array child their index as key. */
function keyArray(child: unknown): unknown {
  if (!Array.isArray(child)) return child;
  return child.map((c, i) => {
    if (Array.isArray(c)) return keyArray(c);
    // biome-ignore lint/suspicious/noArrayIndexKey: these lists are static sample data and never reorder.
    return isValidElement(c) && c.key == null ? cloneElement(c, { key: `.${i}` }) : c;
  });
}

/**
 * React as artboard scripts see it. Mock scripts build static lists such as
 * h('button', null, [icon, label]) without keys; keys only matter when a list
 * changes, so createElement adds index keys instead of letting React warn.
 */
const scriptReact: typeof ReactModule = Object.freeze({
  ...ReactModule,
  createElement: ((type: Parameters<typeof createElement>[0], props: Props | null, ...children: unknown[]) =>
    createElement(type, props, ...(children.map(keyArray) as ReactNode[]))) as typeof createElement,
});

/** Evaluates `class Component extends DCLogic` from an artboard's script. */
export function evaluateLogic(artboard: Artboard): LogicClass {
  const source = artboard.script?.trim();
  if (!source) return DCLogic;
  const factory = new Function(
    "DCLogic",
    "React",
    `"use strict";\n${source}\n;return typeof Component === "function" ? Component : undefined;\n//# sourceURL=mocks/${artboard.file}.js`,
  ) as (base: LogicClass, react: typeof ReactModule) => unknown;
  const Logic = factory(DCLogic, scriptReact);
  if (typeof Logic !== "function" || !(Logic.prototype instanceof DCLogic)) {
    throw new ArtboardError(artboard.file, "the script doesn't define class Component extends DCLogic.");
  }
  return Logic as LogicClass;
}

class RenderContext {
  readonly #library: MockLibrary;
  readonly #file: string;

  constructor(library: MockLibrary, file: string) {
    this.#library = library;
    this.#file = file;
  }

  #problem(text: string, inline = false): ReactNode {
    this.#library.report({ file: this.#file, message: text });
    return <ProblemBox text={text} inline={inline} />;
  }

  readonly #onHole = (error: HoleError) => {
    this.#library.report({ file: this.#file, message: error.message, error });
  };

  /**
   * A <helmet> child. Styles and stylesheets go to the document head, once per
   * artboard however often it is imported, so they never sit between the
   * elements of a layout.
   */
  head(el: ElementNode, scope: Scope, id: string): ReactNode[] {
    const lower = el.tag.toLowerCase();
    if (lower === "style") {
      const css = el.children.map((c) => (c.kind === "text" ? c.text : "")).join("");
      return [createElement("style", { href: id, precedence: "mockview" }, css)];
    }
    if (lower === "link" && attr(el, "rel") === "stylesheet") {
      return [
        createElement("link", { ...elementProps(el.tag, el.attrs, scope, this.#onHole), precedence: "mockview" }),
      ];
    }
    return this.element(el, scope, "#head");
  }

  /** Renders a list of nodes. The result is spread into createElement, so it needs no keys. */
  nodes(nodes: readonly TemplateNode[], scope: Scope, parent: string): ReactNode[] {
    const out: ReactNode[] = [];
    const dropWhitespace = NO_WHITESPACE.has(parent.toLowerCase()) || parent === "#root";
    for (const node of nodes) {
      if (node.kind === "text") {
        // Children of a component are React children, so they follow JSX's whitespace rules.
        const text = parent === "x-import" ? jsxText(node.text) : node.text;
        if (text === "" || (dropWhitespace && /^\s*$/.test(text))) continue;
        out.push(...this.text(text, scope));
      } else {
        out.push(...this.element(node, scope, parent));
      }
    }
    return mergeStrings(out);
  }

  text(text: string, scope: Scope): ReactNode[] {
    const out: ReactNode[] = [];
    for (const piece of pieces(text, scope, this.#onHole)) {
      if ("text" in piece) {
        out.push(piece.text);
        continue;
      }
      const value = piece.value;
      if (value == null || typeof value === "boolean") continue;
      if (typeof value === "string" || typeof value === "number" || typeof value === "bigint") out.push(String(value));
      else if (isValidElement(value)) out.push(value);
      else if (Array.isArray(value) && value.every((v) => v == null || typeof v !== "object" || isValidElement(v))) {
        out.push(...(value as ReactNode[]));
      } else {
        out.push(
          this.#problem(
            `A text hole holds ${typeof value === "function" ? "a function" : "an object"}, which can't be shown as text.`,
            true,
          ),
        );
      }
    }
    return out;
  }

  element(el: ElementNode, scope: Scope, parent: string): ReactNode[] {
    switch (el.tag) {
      case "sc-for":
        return this.#loop(el, scope, parent);
      case "sc-if":
        return this.#condition(el, scope) ? this.nodes(el.children, scope, parent) : [];
      case "x-import":
        return [this.#component(el, scope)];
      case "dc-import":
        return [this.#import(el, scope)];
      case "helmet":
        return el.children.flatMap((child, i) =>
          child.kind === "element" ? this.head(child, scope, `mockview:${this.#file}:${el.line}:${i}`) : [],
        );
    }
    if (CONTROL_PREFIXES.some((p) => el.tag.startsWith(p))) {
      return [this.#problem(`<${el.tag}> on line ${el.line} isn't part of the format, so it renders as nothing.`)];
    }
    return [this.#html(el, scope)];
  }

  #loop(el: ElementNode, scope: Scope, parent: string): ReactNode[] {
    const listAttr = attr(el, "list");
    const as = attr(el, "as") || "item";
    const list = listAttr == null ? undefined : attributeValue(listAttr, scope, this.#onHole);
    if (list == null) return [];
    let items: unknown[];
    if (Array.isArray(list)) items = list;
    else if (typeof list === "object" && Symbol.iterator in list) items = Array.from(list as Iterable<unknown>);
    else return [this.#problem(`<sc-for list="${listAttr}"> on line ${el.line} needs an array, not ${typeof list}.`)];
    return items.flatMap((item, index) => this.nodes(el.children, scope.child({ [as]: item, $index: index }), parent));
  }

  #condition(el: ElementNode, scope: Scope): boolean {
    const value = attr(el, "value");
    return value == null ? false : Boolean(attributeValue(value, scope, this.#onHole));
  }

  #component(el: ElementNode, scope: Scope): ReactNode {
    const path = attr(el, "component-from-global-scope");
    if (!path) return this.#problem(`<x-import> on line ${el.line} has no component-from-global-scope.`);
    const type = this.#library.resolveComponent(path);
    if (!isComponentType(type)) {
      return this.#problem(`${path} isn't a component on window, so <x-import> on line ${el.line} renders as nothing.`);
    }
    const props = componentProps(
      el.attrs,
      scope,
      this.#onHole,
      (name) => name === "component-from-global-scope" || name.startsWith("hint-"),
    );
    const children = this.nodes(el.children, scope, "x-import");
    return (
      <Boundary label={path} file={this.#file} report={this.#library.report}>
        {createElement(type, props, ...children)}
      </Boundary>
    );
  }

  #import(el: ElementNode, scope: Scope): ReactNode {
    const nameAttr = attr(el, "name");
    const name = nameAttr == null ? undefined : attributeValue(nameAttr, scope, this.#onHole);
    if (typeof name !== "string" || name === "") {
      return this.#problem(`<dc-import> on line ${el.line} has no name, so it can't pick an artboard.`);
    }
    const props = componentProps(el.attrs, scope, this.#onHole, (n) => n === "name" || n.startsWith("hint-"));
    return createElement(this.#library.DcImport, { file: importFile(name), from: this.#file, props });
  }

  #html(el: ElementNode, scope: Scope): ReactNode {
    const tag = el.tag;
    const lower = tag.toLowerCase();
    const props = elementProps(tag, el.attrs, scope, this.#onHole);
    let childNodes = el.children;
    // As in the HTML parser, a newline straight after <pre> is not content.
    const first = childNodes[0];
    if (lower === "pre" && first?.kind === "text" && /^\r?\n/.test(first.text)) {
      childNodes = [{ kind: "text", text: first.text.replace(/^\r?\n/, "") }, ...childNodes.slice(1)];
    }
    let children = this.nodes(childNodes, scope, tag);
    if (lower === "textarea") {
      props["defaultValue"] = children.filter((c) => typeof c === "string").join("");
      children = [];
    }
    if (lower === "select") {
      const result = takeSelected(children, Boolean(props["multiple"]));
      children = result.children;
      if (result.value !== undefined && !("defaultValue" in props) && !("value" in props)) {
        props["defaultValue"] = result.value;
      }
    }
    return createElement(tag, props, ...children);
  }
}

/**
 * Text the way JSX reads it: a run that spans lines loses the indentation
 * and line breaks around each line, lines join with one space, and empty
 * lines disappear. Text on one line stays as written.
 */
export function jsxText(text: string): string {
  const lines = text.split(/\r\n|\n|\r/);
  if (lines.length === 1) return text;
  const last = lines.length - 1;
  return lines
    .map((line, i) => {
      let out = line;
      if (i !== 0) out = out.replace(/^[ \t]+/, "");
      if (i !== last) out = out.replace(/[ \t]+$/, "");
      return out;
    })
    .filter((line) => line !== "")
    .join(" ");
}

function mergeStrings(nodes: ReactNode[]): ReactNode[] {
  const out: ReactNode[] = [];
  for (const node of nodes) {
    const last = out[out.length - 1];
    if (typeof node === "string" && typeof last === "string") out[out.length - 1] = last + node;
    else out.push(node);
  }
  return out;
}

/**
 * React picks a select's options with defaultValue, not with selected on an
 * option, so this moves the choice there, as an HTML page would make it:
 * every selected option for <select multiple>, otherwise the last one. The
 * options come back without selected, whatever its value.
 */
function takeSelected(
  children: ReactNode[],
  multiple: boolean,
): { children: ReactNode[]; value: string | string[] | undefined } {
  const values: string[] = [];
  const visit = (nodes: ReactNode[]): ReactNode[] =>
    nodes.map((node) => {
      if (!isValidElement<Props>(node)) return node;
      if (node.type === "option" && "selected" in node.props) {
        const { selected, ...rest } = node.props;
        if (selected) values.push(optionValue(rest));
        return createElement("option", node.key == null ? rest : { ...rest, key: node.key });
      }
      if (node.type === "optgroup") {
        const { children: options, ...rest } = node.props;
        return createElement("optgroup", rest, ...visit(ReactModule.Children.toArray(options as ReactNode)));
      }
      return node;
    });
  const out = visit(children);
  if (multiple) return { children: out, value: values.length ? values : undefined };
  return { children: out, value: values.at(-1) };
}

/** An option's value: its value attribute, or else its text with the whitespace collapsed, as the DOM reads it. */
function optionValue(props: Props): string {
  if (props["value"] != null) return String(props["value"]);
  return childText(props["children"])
    .replace(/[\t\n\f\r ]+/g, " ")
    .replace(/^ | $/g, "");
}

function childText(children: unknown): string {
  if (typeof children === "string" || typeof children === "number") return String(children);
  if (Array.isArray(children)) return children.map(childText).join("");
  return "";
}
