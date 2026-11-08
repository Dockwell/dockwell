// SPDX-License-Identifier: AGPL-3.0-only
// What a page showed when a run failed. A step that times out says only what it
// waited for; this says where the page was instead, what it showed, what it said
// to its console and which of its requests failed, so a failure in CI explains
// itself without a screenshot.
import type { BrowserContext, Page } from "playwright-core";

interface Watched {
  page: Page;
  said: string[];
  failed: string[];
}

const watched: Watched[] = [];

/** How many console lines and failed requests each page keeps. */
const keep = 15;

function remember(list: string[], line: string) {
  list.push(line.length > 300 ? `${line.slice(0, 300)}…` : line);
  if (list.length > keep) list.shift();
}

/** Keeps what page says to its console and which of its requests fail, for describePages. */
export function watchPage(page: Page): Page {
  if (watched.some((w) => w.page === page)) return page;
  const w: Watched = { page, said: [], failed: [] };
  page.on("console", (msg) => {
    if (msg.type() === "error" || msg.type() === "warning") remember(w.said, `${msg.type()}: ${msg.text()}`);
  });
  page.on("requestfailed", (req) => remember(w.failed, `${req.method()} ${req.url()}: ${req.failure()?.errorText}`));
  page.on("response", (res) => {
    if (res.status() >= 400) remember(w.failed, `${res.request().method()} ${res.url()}: ${res.status()}`);
  });
  watched.push(w);
  return page;
}

/** Watches every page context opens from now on, such as the ones a flow opens for itself. */
export function watchContext(context: BrowserContext): BrowserContext {
  context.on("page", (page) => watchPage(page));
  return context;
}

/** Says where each watched page that's still open is and what it shows, newest first. */
export async function describePages(): Promise<string> {
  const parts: string[] = [];
  for (const w of [...watched].reverse()) {
    if (w.page.isClosed()) continue;
    const shown = await w.page
      .evaluate(() => ({
        url: location.href,
        title: document.title,
        text: (document.body?.innerText ?? "").replace(/\s+/g, " ").trim(),
      }))
      .catch((err: unknown) => ({ url: w.page.url(), title: "", text: `(couldn't read it: ${String(err)})` }));
    const text = shown.text.length > 1500 ? `${shown.text.slice(0, 1500)}…` : shown.text;
    parts.push(
      [
        `the page at ${shown.url} (${shown.title || "no title"}) showed: ${text || "nothing"}`,
        w.said.length > 0 ? `  its console said:\n    ${w.said.join("\n    ")}` : "",
        w.failed.length > 0 ? `  requests that failed:\n    ${w.failed.join("\n    ")}` : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
  return parts.length > 0 ? parts.join("\n") : "no page was open";
}
