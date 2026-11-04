// SPDX-License-Identifier: AGPL-3.0-only

import { applyThemePreference, Button, Icon, Logo } from "@dockwell/ui";
import { type RefObject, useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { Board } from "../canvas";
import {
  frameUrl,
  isKeyMessage,
  isOpenMessage,
  parseViewerHash,
  type Shortcut,
  shortcutFor,
  type ThemeChoice,
  type ViewerRoute,
  viewerHash,
} from "../links";
import { boards, findBoard, pages } from "../mocks";

function subscribeHash(onChange: () => void) {
  window.addEventListener("hashchange", onChange);
  return () => window.removeEventListener("hashchange", onChange);
}

function useRoute(): [ViewerRoute, (route: ViewerRoute) => void] {
  const hash = useSyncExternalStore(
    subscribeHash,
    () => location.hash,
    () => "",
  );
  const route = useMemo(() => parseViewerHash(hash), [hash]);
  const navigate = useCallback((next: ViewerRoute) => {
    location.hash = viewerHash(next);
  }, []);
  return [route, navigate];
}

interface Choice<T> {
  value: T;
  label: string;
  title: string;
}

const THEMES: Choice<ThemeChoice>[] = [
  { value: null, label: "As drawn", title: "Each artboard in the theme it was drawn in" },
  { value: "light", label: "Light", title: "Every artboard in the light theme" },
  { value: "dark", label: "Dark", title: "Every artboard in the dark theme" },
];

const SIZES: Choice<boolean>[] = [
  { value: true, label: "Fit to window", title: "Shrink wide artboards to the window's width" },
  { value: false, label: "Actual size", title: "Show the artboard at its canvas size" },
];

function Switch<T>({
  label,
  choices,
  value,
  onChange,
}: {
  label: string;
  choices: Choice<T>[];
  value: T;
  onChange: (value: T) => void;
}) {
  return (
    <fieldset className="mv-seg">
      <legend className="dw-sr">{label}</legend>
      {choices.map((c) => (
        <button
          key={c.label}
          type="button"
          className="mv-seg-item"
          aria-pressed={value === c.value}
          title={c.title}
          onClick={() => onChange(c.value)}
        >
          {c.label}
        </button>
      ))}
    </fieldset>
  );
}

function size(board: Board): string {
  return `${board.width} × ${board.height}`;
}

function Index({ route, navigate }: { route: ViewerRoute; navigate: (route: ViewerRoute) => void }) {
  const [filter, setFilter] = useState("");
  const query = filter.trim().toLowerCase();
  const shown = pages
    .map((page) => ({
      ...page,
      boards: page.boards.filter(
        (b) => !query || b.title.toLowerCase().includes(query) || b.file.toLowerCase().includes(query),
      ),
    }))
    .filter((page) => page.boards.length > 0);

  return (
    <div className="mv-page">
      <header className="mv-bar">
        <a className="mv-brand" href={viewerHash({ ...route, file: null, hash: "" })}>
          <Logo height={14} />
          <span>Mock viewer</span>
        </a>
        <span className="mv-spacer" />
        <Switch
          label="Theme"
          choices={THEMES}
          value={route.theme}
          onChange={(theme) => navigate({ ...route, theme })}
        />
      </header>
      <main className="mv-index">
        <div className="mv-intro">
          <h1 className="dw-text-title">Screen mocks</h1>
          <p>
            {boards.length} artboards from <code>design/mocks</code>, drawn from the files as they are. Open one to see
            it at its canvas size; links inside a screen open the screen they point to.
          </p>
          <input
            className="mv-filter"
            type="search"
            placeholder="Filter by title or file"
            aria-label="Filter artboards"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </div>
        {shown.length === 0 ? <p className="mv-empty">No artboard matches “{filter.trim()}”.</p> : null}
        <div className="mv-groups">
          {shown.map((page) => (
            <section key={page.id} className="mv-group" aria-labelledby={`page-${page.id}`}>
              <div className="mv-group-head">
                <h2 id={`page-${page.id}`}>{page.name}</h2>
                <span>{page.boards.length === 1 ? "1 artboard" : `${page.boards.length} artboards`}</span>
              </div>
              <ul>
                {page.boards.map((b) => (
                  <li key={b.file}>
                    <a href={viewerHash({ file: b.file, theme: route.theme, hash: "" })}>
                      <span className="mv-board-title">{b.title}</span>
                      <span className="mv-board-meta">
                        <code>{b.file.replace(/\.dc\.html$/, "")}</code>
                        <span>{size(b)}</span>
                      </span>
                    </a>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </div>
      </main>
    </div>
  );
}

function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => setWidth(entry?.contentRect.width ?? 0));
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return width;
}

function BoardView({
  board,
  route,
  navigate,
}: {
  board: Board;
  route: ViewerRoute;
  navigate: (route: ViewerRoute) => void;
}) {
  const [fit, setFit] = useState(true);
  const stage = useRef<HTMLDivElement>(null);
  const width = useWidth(stage);
  const scale = fit && width > 0 ? Math.min(1, width / board.width) : 1;
  const index = boards.indexOf(board);
  const prev = boards[index - 1];
  const next = boards[index + 1];
  const open = useCallback(
    (file: string, hash = "") => navigate({ file, theme: route.theme, hash }),
    [navigate, route.theme],
  );

  useEffect(() => {
    const run = (shortcut: Shortcut | null) => {
      if (shortcut === "previous" && prev) open(prev.file);
      else if (shortcut === "next" && next) open(next.file);
      else if (shortcut === "back") navigate({ file: null, theme: route.theme, hash: "" });
    };
    // The frame sends links to other artboards, and the shortcut keys pressed while it has focus.
    const onMessage = (event: MessageEvent) => {
      if (event.origin !== location.origin) return;
      if (isOpenMessage(event.data)) open(event.data.file, event.data.hash);
      else if (isKeyMessage(event.data)) run(event.data.shortcut);
    };
    const onKey = (event: KeyboardEvent) => run(shortcutFor(event));
    window.addEventListener("message", onMessage);
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("message", onMessage);
      window.removeEventListener("keydown", onKey);
    };
  }, [open, navigate, prev, next, route.theme]);

  const src = frameUrl(board.file, route.theme, route.hash);
  return (
    <div className="mv-view">
      <header className="mv-bar">
        <a className="dw-btn dw-btn-ghost dw-btn-sm" href={viewerHash({ file: null, theme: route.theme, hash: "" })}>
          <Icon name="arrow-left" size={14} />
          <span>All screens</span>
        </a>
        <div className="mv-view-title">
          <h1>{board.title}</h1>
          <span>
            <code>{board.file}</code> · {size(board)}
          </span>
        </div>
        <span className="mv-spacer" />
        <Button
          size="sm"
          variant="ghost"
          icon="arrow-left"
          aria-label={prev ? `Previous: ${prev.title}` : "Previous"}
          title={prev ? `Previous: ${prev.title} (←)` : undefined}
          disabled={!prev}
          onClick={() => prev && open(prev.file)}
        />
        <Button
          size="sm"
          variant="ghost"
          icon="arrow-right"
          aria-label={next ? `Next: ${next.title}` : "Next"}
          title={next ? `Next: ${next.title} (→)` : undefined}
          disabled={!next}
          onClick={() => next && open(next.file)}
        />
        <Switch
          label="Theme"
          choices={THEMES}
          value={route.theme}
          onChange={(theme) => navigate({ ...route, theme })}
        />
        <Switch label="Size" choices={SIZES} value={fit} onChange={setFit} />
        <a className="dw-btn dw-btn-ghost dw-btn-sm" href={src} target="_blank" rel="noopener">
          <Icon name="external" size={14} />
          <span>Open alone</span>
        </a>
      </header>
      <div className="mv-stage" ref={stage}>
        <div className="mv-frame" style={{ width: board.width * scale, height: board.height * scale }}>
          <iframe
            key={src}
            src={src}
            title={board.title}
            width={board.width}
            height={board.height}
            style={{ transform: scale === 1 ? undefined : `scale(${scale})` }}
          />
        </div>
      </div>
    </div>
  );
}

function Missing({ file, route }: { file: string; route: ViewerRoute }) {
  return (
    <div className="mv-page">
      <main className="mv-index">
        <h1 className="dw-text-title">No artboard called {file}</h1>
        <p>
          design/mocks/canvas.json doesn't list it. Check the name, or{" "}
          <a href={viewerHash({ file: null, theme: route.theme, hash: "" })}>go back to every screen</a>.
        </p>
      </main>
    </div>
  );
}

export function App() {
  const [route, navigate] = useRoute();
  useEffect(() => applyThemePreference(route.theme ?? "system"), [route.theme]);
  const board = route.file ? findBoard(route.file) : undefined;
  useEffect(() => {
    document.title = board ? `${board.title} · Dockwell screen mocks` : "Dockwell screen mocks";
  }, [board]);
  if (!route.file) return <Index route={route} navigate={navigate} />;
  if (!board) return <Missing file={route.file} route={route} />;
  return <BoardView board={board} route={route} navigate={navigate} />;
}
