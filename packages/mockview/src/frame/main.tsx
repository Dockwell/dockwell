// SPDX-License-Identifier: AGPL-3.0-only

// Renders one artboard on its own page: frame.html?board=Rail.dc.html&theme=dark.
// The viewer shows this page in an iframe at the artboard's canvas size, and
// the snapshot script loads it directly. It loads only the design system's
// reference files, so nothing from the viewer's own styles leaks in.

import "../../../../design/system/tokens.css";
import "../../../../design/system/components/bundle.css";

import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";

import { MockLibrary, type Problem } from "../format/render";
import { artboardLink, frameUrl, OPEN_MESSAGE, type OpenMessage, parseTheme } from "../links";
import { loadMock } from "../mocks";
import { installDesignSystem } from "../runtime/design-system";

/** What the snapshot script reads once the frame settles. */
export interface FrameStatus {
  status: "loading" | "ready" | "failed";
  file: string;
  problems: { file: string; message: string }[];
}

declare global {
  interface Window {
    __mockview?: FrameStatus;
  }
}

const params = new URLSearchParams(location.search);
const file = params.get("board") || "Main.dc.html";
const choice = parseTheme(params.get("theme"));
const status: FrameStatus = { status: "loading", file, problems: [] };
window.__mockview = status;

function record(problem: Problem) {
  status.problems.push({ file: problem.file, message: problem.message });
  console.error(`${problem.file}: ${problem.message}`, ...(problem.error instanceof Error ? [problem.error] : []));
}

function nextFrame(): Promise<void> {
  return new Promise((done) => requestAnimationFrame(() => done()));
}

// Links between artboards open the target inside the viewer. Links to other
// sites are sample data, so they go nowhere; in-page anchors work as usual.
document.addEventListener("click", (event) => {
  if (event.defaultPrevented || event.button !== 0 || !(event.target instanceof Element)) return;
  const anchor = event.target.closest("a[href]");
  const href = anchor?.getAttribute("href");
  if (href == null || href.startsWith("#")) return;
  event.preventDefault();
  const link = artboardLink(href);
  if (!link) return;
  if (event.metaKey || event.ctrlKey || event.shiftKey) {
    window.open(frameUrl(link.file, choice, link.hash), "_blank", "noopener");
  } else if (window.parent !== window) {
    const message: OpenMessage = { type: OPEN_MESSAGE, file: link.file, hash: link.hash };
    window.parent.postMessage(message, location.origin);
  } else {
    location.assign(frameUrl(link.file, choice, link.hash));
  }
});

async function main() {
  const container = document.getElementById("root");
  if (!container) throw new Error("frame.html has no #root element.");
  installDesignSystem();
  const library = new MockLibrary({ load: loadMock, onProblem: record });
  const artboard = await library.load(file);
  const drawn = artboard.props["theme"]?.default === "dark" ? "dark" : "light";
  const theme = choice ?? drawn;
  // The shell pieces have no theme prop and inherit it, so the page carries it too.
  document.documentElement.dataset["theme"] = theme;
  document.title = artboard.title;

  const root = createRoot(container);
  flushSync(() => root.render(library.element(file, "theme" in artboard.props && choice ? { theme } : {})));
  await document.fonts.ready;
  await nextFrame();
  await nextFrame();
  if (location.hash) document.getElementById(decodeURIComponent(location.hash.slice(1)))?.scrollIntoView();
}

main()
  .catch((error: unknown) => {
    record({ file, message: error instanceof Error ? error.message : String(error), error });
    const box = document.createElement("pre");
    box.setAttribute("role", "alert");
    box.style.cssText = "margin:24px;font:13px/20px var(--font-mono);color:var(--critical);white-space:pre-wrap";
    box.textContent = status.problems.map((p) => p.message).join("\n");
    document.body.append(box);
  })
  .finally(() => {
    status.status = status.problems.length ? "failed" : "ready";
    document.documentElement.dataset["mockview"] = status.status;
  });
