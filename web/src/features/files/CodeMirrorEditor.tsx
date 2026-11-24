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
import { highlightSelectionMatches, openSearchPanel, search, searchKeymap } from "@codemirror/search";
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
  ViewPlugin,
  type ViewUpdate,
} from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { type Ref, useEffect, useImperativeHandle, useRef } from "react";

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
    flexWrap: "wrap",
    alignItems: "center",
    gap: "6px",
    padding: "8px 12px",
  },
  ".cm-search input, .cm-search button": {
    fontFamily: "var(--font-sans)",
    fontSize: "12.5px",
    borderRadius: "var(--radius-xs)",
  },
  ".cm-search input": {
    height: "28px",
    padding: "0 8px",
    border: "1px solid var(--line-strong)",
    backgroundColor: "var(--surface)",
    color: "var(--ink)",
  },
  ".cm-search input:focus": {
    outline: "2px solid var(--cobalt)",
    outlineOffset: "-1px",
  },
  ".cm-search button": {
    height: "28px",
    padding: "0 8px",
    border: "1px solid var(--line)",
    backgroundColor: "var(--surface)",
    backgroundImage: "none",
    color: "var(--ink)",
    cursor: "pointer",
  },
  ".cm-search button:hover": {
    backgroundColor: "var(--surface-raised)",
    backgroundImage: "none",
  },
  ".cm-search button:active": {
    backgroundColor: "var(--surface-sunken)",
    backgroundImage: "none",
  },
  ".cm-search button[name='close']": {
    height: "24px",
    padding: "0 6px",
    border: "none",
    backgroundColor: "transparent",
    backgroundImage: "none",
    color: "var(--ink-muted)",
  },
  ".cm-search button[name='close']:hover": {
    backgroundColor: "var(--surface-raised)",
    color: "var(--ink)",
  },
  ".cm-search label": {
    display: "inline-flex",
    alignItems: "center",
    gap: "4px",
    fontSize: "12px",
    color: "var(--ink-muted)",
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
  const hostRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const onSaveRef = useRef(onSave);
  onChangeRef.current = onChange;
  onSaveRef.current = onSave;

  // Compartments let us reconfigure readOnly, wordWrap, language, aria-label, and markedLine without destroying history.
  const compartments = useRef({
    lang: new Compartment(),
    wrap: new Compartment(),
    readOnly: new Compartment(),
    attrs: new Compartment(),
    mark: new Compartment(),
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
        search({ top: true }),
        syntaxHighlighting(dockwellHighlight),
        dockwellTheme,
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

  return <div ref={hostRef} className="files-cm-host" />;
}
