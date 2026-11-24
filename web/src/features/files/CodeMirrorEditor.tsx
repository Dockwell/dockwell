// SPDX-License-Identifier: AGPL-3.0-only

import { closeBrackets, closeBracketsKeymap } from "@codemirror/autocomplete";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { css } from "@codemirror/lang-css";
import { go } from "@codemirror/lang-go";
import { html } from "@codemirror/lang-html";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";
import { markdown } from "@codemirror/lang-markdown";
import { php } from "@codemirror/lang-php";
import { python } from "@codemirror/lang-python";
import { sql } from "@codemirror/lang-sql";
import { xml } from "@codemirror/lang-xml";
import { yaml } from "@codemirror/lang-yaml";
import {
  bracketMatching,
  HighlightStyle,
  indentOnInput,
  StreamLanguage,
  syntaxHighlighting,
} from "@codemirror/language";
import { dockerFile } from "@codemirror/legacy-modes/mode/dockerfile";
import { nginx } from "@codemirror/legacy-modes/mode/nginx";
import { properties } from "@codemirror/legacy-modes/mode/properties";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { toml } from "@codemirror/legacy-modes/mode/toml";
import {
  closeSearchPanel,
  findNext,
  findPrevious,
  getSearchQuery,
  highlightSelectionMatches,
  openSearchPanel,
  replaceAll,
  replaceNext,
  SearchQuery,
  search,
  searchKeymap,
  selectMatches,
  setSearchQuery,
} from "@codemirror/search";
import { Compartment, EditorState, type Extension, Prec, RangeSetBuilder } from "@codemirror/state";
import {
  Decoration,
  type DecorationSet,
  drawSelection,
  EditorView,
  highlightActiveLine,
  highlightActiveLineGutter,
  keymap,
  lineNumbers,
  type Panel,
  ViewPlugin,
  type ViewUpdate,
} from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { useLingui } from "@lingui/react/macro";
import { type Ref, useEffect, useImperativeHandle, useMemo, useRef } from "react";

export interface CodeMirrorEditorHandle {
  openSearch: () => void;
  focus: () => void;
}

export interface CodeMirrorEditorProps {
  /** File name or path, used for syntax highlighting mode detection. */
  fileName: string;
  value: string;
  readOnly?: boolean;
  wordWrap?: boolean;
  markedLine?: number | undefined;
  ariaLabel: string;
  onChange?: (next: string) => void;
  onSave?: () => void;
  ref?: Ref<CodeMirrorEditorHandle>;
}

/** Detect the CodeMirror language extension from a file name. */
function languageFor(fileName: string): Extension {
  const base = fileName.slice(fileName.lastIndexOf("/") + 1);
  const lower = base.toLowerCase();
  if (lower === "dockerfile" || lower.startsWith("dockerfile.") || lower.endsWith(".dockerfile")) {
    return StreamLanguage.define(dockerFile);
  }
  if (lower === "nginx.conf" || lower.endsWith(".nginx")) {
    return StreamLanguage.define(nginx);
  }
  if (lower === ".bashrc" || lower === ".zshrc" || lower === ".profile" || lower === ".bash_profile") {
    return StreamLanguage.define(shell);
  }
  if (lower === ".env" || lower.startsWith(".env.")) {
    return StreamLanguage.define(properties);
  }
  const dot = lower.lastIndexOf(".");
  const ext = dot > 0 ? lower.slice(dot + 1) : "";
  switch (ext) {
    case "js":
    case "mjs":
    case "cjs":
      return javascript();
    case "jsx":
      return javascript({ jsx: true });
    case "ts":
    case "mts":
    case "cts":
      return javascript({ typescript: true });
    case "tsx":
      return javascript({ jsx: true, typescript: true });
    case "json":
    case "jsonc":
      return json();
    case "html":
    case "htm":
      return html();
    case "css":
    case "scss":
    case "less":
      return css();
    case "md":
    case "mdx":
    case "markdown":
      return markdown();
    case "py":
    case "pyw":
      return python();
    case "yml":
    case "yaml":
      return yaml();
    case "xml":
    case "svg":
    case "plist":
      return xml();
    case "sql":
      return sql();
    case "go":
      return go();
    case "php":
    case "phtml":
      return php();
    case "sh":
    case "bash":
    case "zsh":
      return StreamLanguage.define(shell);
    case "toml":
      return StreamLanguage.define(toml);
    case "conf":
      return StreamLanguage.define(nginx);
    case "ini":
    case "cfg":
    case "properties":
    case "service":
    case "timer":
    case "socket":
    case "mount":
    case "target":
      return StreamLanguage.define(properties);
    default:
      return [];
  }
}

/** Highlighting palette mapped strictly to Dockwell's semantic tokens (works in light and dark). */
const dockwellHighlight = HighlightStyle.define([
  {
    tag: [tags.keyword, tags.controlKeyword, tags.moduleKeyword, tags.operatorKeyword],
    color: "var(--cobalt)",
    fontWeight: "550",
  },
  { tag: [tags.string, tags.special(tags.string)], color: "var(--healthy)" },
  { tag: [tags.number, tags.bool, tags.atom, tags.null], color: "var(--warning)" },
  { tag: [tags.comment, tags.lineComment, tags.blockComment], color: "var(--ink-muted)", fontStyle: "italic" },
  { tag: [tags.propertyName, tags.attributeName, tags.tagName], color: "var(--ink)", fontWeight: "550" },
  { tag: [tags.variableName, tags.definition(tags.variableName)], color: "var(--ink)" },
  { tag: [tags.typeName, tags.className, tags.namespace], color: "var(--cobalt)" },
  { tag: [tags.invalid], color: "var(--critical)", textDecoration: "underline" },
  { tag: [tags.heading], color: "var(--ink)", fontWeight: "600" },
  { tag: [tags.link, tags.url], color: "var(--cobalt)", textDecoration: "underline" },
]);

/** Dockwell chrome theme for CodeMirror 6, using design system tokens throughout. */
const dockwellTheme = EditorView.theme({
  "&": {
    height: "100%",
    backgroundColor: "var(--surface)",
    color: "var(--ink)",
    fontFamily: "var(--font-mono)",
    fontSize: "13px",
    lineHeight: "20px",
  },
  "&.cm-focused": {
    outline: "none",
  },
  ".cm-scroller": {
    overflow: "auto",
    fontFamily: "var(--font-mono)",
    lineHeight: "20px",
  },
  ".cm-content": {
    padding: "8px 0",
    caretColor: "var(--ink)",
  },
  ".cm-line": {
    padding: "0 12px",
  },
  ".cm-gutters": {
    backgroundColor: "var(--surface-sunken)",
    color: "var(--ink-muted)",
    borderRight: "1px solid var(--line)",
    fontSize: "12px",
    userSelect: "none",
  },
  ".cm-lineNumbers .cm-gutterElement": {
    padding: "0 10px 0 12px",
    minWidth: "36px",
  },
  ".cm-activeLine": {
    backgroundColor: "color-mix(in srgb, var(--cobalt-soft) 45%, transparent)",
  },
  ".cm-activeLineGutter": {
    backgroundColor: "var(--surface-sunken)",
    color: "var(--ink)",
    fontWeight: "550",
  },
  ".cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection": {
    backgroundColor: "var(--selection)",
  },
  ".cm-selectionMatch": {
    backgroundColor: "var(--warning-soft)",
  },
  ".cm-cursor, .cm-dropCursor": {
    borderLeftColor: "var(--ink)",
  },
  ".cm-panels": {
    backgroundColor: "var(--surface-sunken)",
    color: "var(--ink)",
    borderBottom: "1px solid var(--line)",
    fontFamily: "var(--font-sans)",
    fontSize: "13px",
  },
  ".cm-search": {
    display: "flex",
    flexDirection: "column",
    gap: "6px",
    padding: "8px 12px",
  },
  ".cm-search-row": {
    display: "flex",
    flexWrap: "wrap",
    alignItems: "center",
    gap: "6px",
  },
  ".cm-search-row[hidden]": {
    display: "none",
  },
  ".cm-search-field": {
    display: "inline-flex",
    alignItems: "center",
    flex: "1 1 200px",
    maxWidth: "300px",
    height: "28px",
    padding: "0 6px 0 8px",
    border: "1px solid var(--line-strong)",
    borderRadius: "var(--radius-sm)",
    backgroundColor: "var(--surface)",
    gap: "4px",
  },
  ".cm-search-field:focus-within": {
    borderColor: "var(--cobalt)",
    boxShadow: "0 0 0 2px var(--cobalt-soft)",
  },
  ".cm-search-field.is-invalid": {
    borderColor: "var(--critical)",
  },
  ".cm-search-field input": {
    flex: "1 1 auto",
    minWidth: "80px",
    height: "100%",
    padding: "0",
    border: "none",
    backgroundColor: "transparent",
    color: "var(--ink)",
    fontFamily: "var(--font-sans)",
    fontSize: "12.5px",
    outline: "none",
  },
  ".cm-search-field input::placeholder": {
    color: "var(--ink-muted)",
  },
  ".cm-search-count": {
    flex: "none",
    fontSize: "11.5px",
    color: "var(--ink-muted)",
    fontVariantNumeric: "tabular-nums",
    whiteSpace: "nowrap",
  },
  ".cm-search-count.is-empty": {
    color: "var(--critical)",
  },
  ".cm-search-group": {
    display: "inline-flex",
    alignItems: "center",
    gap: "2px",
  },
  ".cm-search button": {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    gap: "4px",
    height: "28px",
    padding: "0 8px",
    border: "1px solid var(--line)",
    borderRadius: "var(--radius-sm)",
    backgroundColor: "var(--surface)",
    backgroundImage: "none",
    color: "var(--ink)",
    fontFamily: "var(--font-sans)",
    fontSize: "12.5px",
    fontWeight: "500",
    lineHeight: "1",
    cursor: "pointer",
    whiteSpace: "nowrap",
  },
  ".cm-search button:hover:not(:disabled)": {
    backgroundColor: "var(--surface-raised)",
    backgroundImage: "none",
  },
  ".cm-search button:active:not(:disabled)": {
    backgroundColor: "var(--surface-sunken)",
    backgroundImage: "none",
  },
  ".cm-search button:focus-visible": {
    outline: "2px solid var(--cobalt)",
    outlineOffset: "1px",
  },
  ".cm-search button:disabled": {
    opacity: "0.45",
    cursor: "default",
  },
  ".cm-search button.cm-search-icon-btn": {
    width: "28px",
    padding: "0",
    border: "1px solid transparent",
    backgroundColor: "transparent",
    color: "var(--ink-muted)",
  },
  ".cm-search button.cm-search-icon-btn:hover:not(:disabled)": {
    backgroundColor: "var(--surface)",
    borderColor: "var(--line)",
    color: "var(--ink)",
  },
  ".cm-search button.cm-search-toggle": {
    minWidth: "28px",
    padding: "0 6px",
    border: "1px solid transparent",
    backgroundColor: "transparent",
    color: "var(--ink-muted)",
    fontFamily: "var(--font-mono)",
    fontSize: "11.5px",
    fontWeight: "600",
  },
  ".cm-search button.cm-search-toggle:hover": {
    backgroundColor: "var(--surface)",
    borderColor: "var(--line)",
    color: "var(--ink)",
  },
  ".cm-search button.cm-search-toggle[aria-pressed='true']": {
    backgroundColor: "var(--cobalt-soft)",
    borderColor: "var(--cobalt)",
    color: "var(--cobalt)",
  },
  ".cm-search button.cm-search-close": {
    marginInlineStart: "auto",
  },
  ".cm-searchMatch": {
    backgroundColor: "var(--warning-soft)",
    outline: "1px solid var(--warning)",
  },
  ".cm-searchMatch.cm-searchMatch-selected": {
    backgroundColor: "var(--cobalt-soft)",
    outline: "1px solid var(--cobalt)",
  },
  ".files-cm-marked": {
    backgroundColor: "var(--warning-soft)",
  },
});

const SVG_NS = "http://www.w3.org/2000/svg";

function makeIcon(paths: readonly string[]): SVGSVGElement {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("width", "14");
  svg.setAttribute("height", "14");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.6");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  for (const d of paths) {
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    svg.appendChild(path);
  }
  return svg;
}

/**
 * Dockwell-styled CodeMirror search and replace panel using CodeMirror's
 * EditorState.phrases facet for every label, tooltip, and placeholder.
 */
function createDockwellSearchPanel(view: EditorView): Panel {
  const dom = document.createElement("form");
  dom.className = "cm-search";
  dom.setAttribute("role", "search");

  const findRow = document.createElement("div");
  findRow.className = "cm-search-row";

  const findWrap = document.createElement("div");
  findWrap.className = "cm-search-field";

  const searchInput = document.createElement("input");
  searchInput.type = "text";
  searchInput.name = "search";
  searchInput.setAttribute("main-field", "true");
  searchInput.autocomplete = "off";
  searchInput.spellcheck = false;

  const countEl = document.createElement("span");
  countEl.className = "cm-search-count";
  countEl.setAttribute("aria-live", "polite");

  findWrap.append(searchInput, countEl);

  const toggles = document.createElement("div");
  toggles.className = "cm-search-group";

  const caseBtn = document.createElement("button");
  caseBtn.type = "button";
  caseBtn.name = "case";
  caseBtn.className = "cm-search-toggle";
  caseBtn.textContent = "Aa";

  const wordBtn = document.createElement("button");
  wordBtn.type = "button";
  wordBtn.name = "word";
  wordBtn.className = "cm-search-toggle";
  wordBtn.textContent = "ab";
  wordBtn.style.textDecoration = "underline";

  const reBtn = document.createElement("button");
  reBtn.type = "button";
  reBtn.name = "re";
  reBtn.className = "cm-search-toggle";
  reBtn.textContent = ".*";

  toggles.append(caseBtn, wordBtn, reBtn);

  const navGroup = document.createElement("div");
  navGroup.className = "cm-search-group";

  const prevBtn = document.createElement("button");
  prevBtn.type = "button";
  prevBtn.name = "prev";
  prevBtn.className = "cm-search-icon-btn";
  prevBtn.appendChild(makeIcon(["M11.5 9.5L8 6l-3.5 3.5"]));

  const nextBtn = document.createElement("button");
  nextBtn.type = "button";
  nextBtn.name = "next";
  nextBtn.className = "cm-search-icon-btn";
  nextBtn.appendChild(makeIcon(["M4.5 6.5L8 10l3.5-3.5"]));

  const selectBtn = document.createElement("button");
  selectBtn.type = "button";
  selectBtn.name = "select";

  navGroup.append(prevBtn, nextBtn, selectBtn);

  const closeBtn = document.createElement("button");
  closeBtn.type = "button";
  closeBtn.name = "close";
  closeBtn.className = "cm-search-icon-btn cm-search-close";
  closeBtn.appendChild(makeIcon(["M4 4l8 8", "M12 4l-8 8"]));

  findRow.append(findWrap, toggles, navGroup, closeBtn);

  const replaceRow = document.createElement("div");
  replaceRow.className = "cm-search-row cm-search-replace-row";

  const replaceWrap = document.createElement("div");
  replaceWrap.className = "cm-search-field";

  const replaceInput = document.createElement("input");
  replaceInput.type = "text";
  replaceInput.name = "replace";
  replaceInput.autocomplete = "off";
  replaceInput.spellcheck = false;
  replaceWrap.appendChild(replaceInput);

  const replaceBtn = document.createElement("button");
  replaceBtn.type = "button";
  replaceBtn.name = "replace";

  const replaceAllBtn = document.createElement("button");
  replaceAllBtn.type = "button";
  replaceAllBtn.name = "replaceAll";

  replaceRow.append(replaceWrap, replaceBtn, replaceAllBtn);
  dom.append(findRow, replaceRow);

  let caseSensitive = false;
  let wholeWord = false;
  let regexp = false;

  const syncLabels = (state: EditorState) => {
    const p = (key: string) => state.phrase(key);
    dom.setAttribute("aria-label", p("Find and replace"));
    searchInput.placeholder = p("Find");
    searchInput.setAttribute("aria-label", p("Find"));
    replaceInput.placeholder = p("Replace with");
    replaceInput.setAttribute("aria-label", p("Replace"));
    caseBtn.title = p("match case");
    caseBtn.setAttribute("aria-label", p("match case"));
    wordBtn.title = p("by word");
    wordBtn.setAttribute("aria-label", p("by word"));
    reBtn.title = p("regexp");
    reBtn.setAttribute("aria-label", p("regexp"));
    prevBtn.title = p("previous");
    prevBtn.setAttribute("aria-label", p("previous"));
    nextBtn.title = p("next");
    nextBtn.setAttribute("aria-label", p("next"));
    selectBtn.textContent = p("all");
    replaceBtn.textContent = p("replace");
    replaceAllBtn.textContent = p("replace all");
    closeBtn.title = p("close");
    closeBtn.setAttribute("aria-label", p("close"));
    replaceRow.hidden = state.readOnly;
  };

  const updateCount = (state: EditorState, query: SearchQuery) => {
    if (!query.search) {
      countEl.textContent = "";
      countEl.classList.remove("is-empty");
      findWrap.classList.remove("is-invalid");
      prevBtn.disabled = true;
      nextBtn.disabled = true;
      selectBtn.disabled = true;
      replaceBtn.disabled = true;
      replaceAllBtn.disabled = true;
      return;
    }
    if (!query.valid) {
      countEl.textContent = state.phrase("Invalid regular expression");
      countEl.classList.add("is-empty");
      findWrap.classList.add("is-invalid");
      prevBtn.disabled = true;
      nextBtn.disabled = true;
      selectBtn.disabled = true;
      replaceBtn.disabled = true;
      replaceAllBtn.disabled = true;
      return;
    }
    findWrap.classList.remove("is-invalid");
    const sel = state.selection.main;
    let total = 0;
    let current = 0;
    let capped = false;
    const cursor = query.getCursor(state);
    for (let next = cursor.next(); !next.done; next = cursor.next()) {
      total++;
      if (next.value.from === sel.from && next.value.to === sel.to) {
        current = total;
      }
      if (total > 1000) {
        capped = true;
        break;
      }
    }
    if (total === 0) {
      countEl.textContent = state.phrase("No matches");
      countEl.classList.add("is-empty");
    } else {
      countEl.classList.remove("is-empty");
      const totalLabel = capped ? "1000+" : String(total);
      countEl.textContent = current > 0 ? state.phrase("$ of $", String(current), totalLabel) : totalLabel;
    }
    const disabled = total === 0;
    prevBtn.disabled = disabled;
    nextBtn.disabled = disabled;
    selectBtn.disabled = disabled;
    replaceBtn.disabled = disabled;
    replaceAllBtn.disabled = disabled;
  };

  const syncFromQuery = (state: EditorState, query: SearchQuery) => {
    if (searchInput.value !== query.search) searchInput.value = query.search;
    if (replaceInput.value !== query.replace) replaceInput.value = query.replace;
    caseSensitive = query.caseSensitive;
    wholeWord = query.wholeWord;
    regexp = query.regexp;
    caseBtn.setAttribute("aria-pressed", String(caseSensitive));
    wordBtn.setAttribute("aria-pressed", String(wholeWord));
    reBtn.setAttribute("aria-pressed", String(regexp));
    syncLabels(state);
    updateCount(state, query);
  };

  const commit = () => {
    const query = new SearchQuery({
      search: searchInput.value,
      replace: replaceInput.value,
      caseSensitive,
      wholeWord,
      regexp,
    });
    if (!query.eq(getSearchQuery(view.state))) {
      view.dispatch({ effects: setSearchQuery.of(query) });
    }
  };

  syncFromQuery(view.state, getSearchQuery(view.state));

  searchInput.addEventListener("input", commit);
  replaceInput.addEventListener("input", commit);
  caseBtn.addEventListener("click", () => {
    caseSensitive = !caseSensitive;
    caseBtn.setAttribute("aria-pressed", String(caseSensitive));
    commit();
    searchInput.focus();
  });
  wordBtn.addEventListener("click", () => {
    wholeWord = !wholeWord;
    wordBtn.setAttribute("aria-pressed", String(wholeWord));
    commit();
    searchInput.focus();
  });
  reBtn.addEventListener("click", () => {
    regexp = !regexp;
    reBtn.setAttribute("aria-pressed", String(regexp));
    commit();
    searchInput.focus();
  });
  prevBtn.addEventListener("click", () => findPrevious(view));
  nextBtn.addEventListener("click", () => findNext(view));
  selectBtn.addEventListener("click", () => selectMatches(view));
  replaceBtn.addEventListener("click", () => replaceNext(view));
  replaceAllBtn.addEventListener("click", () => replaceAll(view));
  closeBtn.addEventListener("click", () => {
    closeSearchPanel(view);
    view.focus();
  });
  dom.addEventListener("submit", (event) => {
    event.preventDefault();
    findNext(view);
  });
  dom.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeSearchPanel(view);
      view.focus();
    } else if (event.key === "Enter" && event.target === searchInput) {
      event.preventDefault();
      if (event.shiftKey) findPrevious(view);
      else findNext(view);
    } else if (event.key === "Enter" && event.target === replaceInput) {
      event.preventDefault();
      replaceNext(view);
    }
  });

  return {
    dom,
    top: true,
    mount() {
      searchInput.select();
    },
    update(update) {
      const query = getSearchQuery(update.state);
      const queryChanged = update.transactions.some((tr) => tr.effects.some((e) => e.is(setSearchQuery)));
      if (queryChanged) {
        syncFromQuery(update.state, query);
      } else {
        syncLabels(update.state);
        if (update.docChanged || update.selectionSet) {
          updateCount(update.state, query);
        }
      }
    },
  };
}

const markedLineDeco = Decoration.line({ class: "files-cm-marked" });

function markLineExtension(line: number | undefined): Extension {
  if (!line || line < 1) return [];
  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      constructor(view: EditorView) {
        this.decorations = this.build(view);
      }
      update(update: ViewUpdate) {
        if (update.docChanged) this.decorations = this.build(update.view);
      }
      build(view: EditorView) {
        const builder = new RangeSetBuilder<Decoration>();
        if (line <= view.state.doc.lines) {
          const info = view.state.doc.line(line);
          builder.add(info.from, info.from, markedLineDeco);
        }
        return builder.finish();
      }
    },
    { decorations: (v) => v.decorations },
  );
}

/**
 * Lazy-loaded CodeMirror 6 editor with syntax highlighting, line numbers,
 * find and replace, word wrap toggle, marked line highlight, and Cmd/Ctrl-S save.
 */
export default function CodeMirrorEditor({
  fileName,
  value,
  readOnly = false,
  wordWrap = false,
  markedLine,
  ariaLabel,
  onChange,
  onSave,
  ref,
}: CodeMirrorEditorProps) {
  const { t, i18n } = useLingui();
  const hostRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const onSaveRef = useRef(onSave);
  onChangeRef.current = onChange;
  onSaveRef.current = onSave;

  // CodeMirror phrases facet populated through Lingui so every find/replace string is sentence case and localized.
  // biome-ignore lint/correctness/useExhaustiveDependencies: rebuild phrase map whenever the active locale changes
  const phrases = useMemo<Record<string, string>>(
    () => ({
      "Find and replace": t`Find and replace`,
      Find: t`Find`,
      Replace: t`Replace`,
      "Replace with": t`Replace with`,
      next: t`Next match`,
      previous: t`Previous match`,
      all: t`Select all`,
      "match case": t`Match case`,
      "by word": t`Whole word`,
      regexp: t`Regular expression`,
      replace: t`Replace`,
      "replace all": t`Replace all`,
      close: t`Close find bar`,
      "No matches": t`No matches`,
      "$ of $": t`$ of $`,
      "Invalid regular expression": t`Invalid regular expression`,
    }),
    [i18n.locale, t],
  );

  // Compartments let us reconfigure readOnly, wordWrap, language, aria-label, phrases, and markedLine without destroying history.
  const compartments = useRef({
    lang: new Compartment(),
    wrap: new Compartment(),
    readOnly: new Compartment(),
    attrs: new Compartment(),
    mark: new Compartment(),
    phrases: new Compartment(),
  });

  useImperativeHandle(ref, () => ({
    openSearch() {
      const view = viewRef.current;
      if (view) openSearchPanel(view);
    },
    focus() {
      viewRef.current?.focus();
    },
  }));

  // Mount CodeMirror once per editor instance. In real browsers with constructable
  // stylesheets, mount inside an open ShadowRoot so style-mod uses adoptedStyleSheets
  // instead of injecting inline <style> tags (which Dockwell's style-src 'self' CSP blocks).
  // biome-ignore lint/correctness/useExhaustiveDependencies: dynamic props are synced through compartments and doc dispatch below
  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    let parent: HTMLElement = host;
    let root: Document | ShadowRoot | undefined;
    if (typeof ShadowRoot !== "undefined" && "adoptedStyleSheets" in ShadowRoot.prototype) {
      const shadow = host.shadowRoot ?? host.attachShadow({ mode: "open" });
      shadow.replaceChildren();
      const inner = document.createElement("div");
      inner.style.height = "100%";
      inner.style.display = "flex";
      inner.style.flexDirection = "column";
      shadow.appendChild(inner);
      parent = inner;
      root = shadow;
    }
    const c = compartments.current;
    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightActiveLine(),
        history(),
        drawSelection(),
        indentOnInput(),
        bracketMatching(),
        closeBrackets(),
        highlightSelectionMatches(),
        search({ top: true, createPanel: createDockwellSearchPanel }),
        syntaxHighlighting(dockwellHighlight),
        dockwellTheme,
        c.phrases.of(EditorState.phrases.of(phrases)),
        Prec.highest(
          keymap.of([
            {
              key: "Mod-s",
              run: () => {
                onSaveRef.current?.();
                return true;
              },
            },
          ]),
        ),
        keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, indentWithTab]),
        c.lang.of(languageFor(fileName)),
        c.wrap.of(wordWrap ? EditorView.lineWrapping : []),
        c.readOnly.of([EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]),
        c.attrs.of(
          EditorView.contentAttributes.of({
            "aria-label": ariaLabel,
            ...(readOnly ? { "aria-readonly": "true" } : {}),
          }),
        ),
        c.mark.of(markLineExtension(markedLine)),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) {
            onChangeRef.current?.(update.state.doc.toString());
          }
        }),
      ],
    });
    const view = new EditorView({ state, parent, ...(root ? { root } : {}) });
    viewRef.current = view;
    if (markedLine && markedLine >= 1 && markedLine <= view.state.doc.lines) {
      const info = view.state.doc.line(markedLine);
      view.dispatch({ effects: EditorView.scrollIntoView(info.from, { y: "center" }) });
    }
    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, []);

  // Sync external document changes (Discard, Load server's version, Bring back draft).
  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const current = view.state.doc.toString();
    if (current !== value) {
      view.dispatch({
        changes: { from: 0, to: current.length, insert: value },
      });
    }
  }, [value]);

  useEffect(() => {
    viewRef.current?.dispatch({ effects: compartments.current.lang.reconfigure(languageFor(fileName)) });
  }, [fileName]);

  useEffect(() => {
    viewRef.current?.dispatch({
      effects: compartments.current.wrap.reconfigure(wordWrap ? EditorView.lineWrapping : []),
    });
  }, [wordWrap]);

  useEffect(() => {
    viewRef.current?.dispatch({
      effects: [
        compartments.current.readOnly.reconfigure([
          EditorState.readOnly.of(readOnly),
          EditorView.editable.of(!readOnly),
        ]),
        compartments.current.attrs.reconfigure(
          EditorView.contentAttributes.of({
            "aria-label": ariaLabel,
            ...(readOnly ? { "aria-readonly": "true" } : {}),
          }),
        ),
      ],
    });
  }, [readOnly, ariaLabel]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({ effects: compartments.current.mark.reconfigure(markLineExtension(markedLine)) });
    if (markedLine && markedLine >= 1 && markedLine <= view.state.doc.lines) {
      const info = view.state.doc.line(markedLine);
      view.dispatch({ effects: EditorView.scrollIntoView(info.from, { y: "center" }) });
    }
  }, [markedLine]);

  useEffect(() => {
    viewRef.current?.dispatch({
      effects: compartments.current.phrases.reconfigure(EditorState.phrases.of(phrases)),
    });
  }, [phrases]);

  return <div ref={hostRef} className="files-cm-host" />;
}
