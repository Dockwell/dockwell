// SPDX-License-Identifier: AGPL-3.0-only
// What a page showed when a run failed. A step that times out says only what it
// waited for; this says where the page was instead, what it showed, what it said
// to its console and which of its requests failed, so a failure in CI explains
// itself without a screenshot.
import type { Browser, BrowserContext, Page } from "playwright-core";

interface Watched {
  page: Page;
  said: string[];
  failed: string[];
}

const watched: Watched[] = [];

/** How many console lines and failed requests each page keeps. */
const keep = 15;

/** How long describePages waits for a page to answer: a page stuck in a loop never does. */
const answerWithin = 5_000;

function remember(list: string[], line: string) {
  list.push(line.length > 300 ? `${line.slice(0, 300)}…` : line);
  if (list.length > keep) list.shift();
}

/** An address without its fragment, which carries a claim or sign-in link's token until the page takes it. */
function withoutToken(url: string): string {
  return url.replace(/#.+$/, "#<token>");
}

function watchPage(page: Page) {
  if (watched.some((w) => w.page === page)) return;
  const w: Watched = { page, said: [], failed: [] };
  page.on("console", (msg) => {
    if (msg.type() === "error" || msg.type() === "warning") remember(w.said, `${msg.type()}: ${msg.text()}`);
  });
  page.on("requestfailed", (req) => {
    const why = req.failure()?.errorText;
    // The panel cancels its live streams as it moves between pages; those say nothing.
    if (why !== "net::ERR_ABORTED") remember(w.failed, `${req.method()} ${withoutToken(req.url())}: ${why}`);
  });
  page.on("response", (res) => {
    if (res.status() >= 400)
      remember(w.failed, `${res.request().method()} ${withoutToken(res.url())}: ${res.status()}`);
  });
  watched.push(w);
}

function watchContext(context: BrowserContext) {
  for (const page of context.pages()) watchPage(page);
  context.on("page", watchPage);
}

/**
 * Keeps, for every page browser opens from now on, in any of its contexts, what it
 * says to its console and which of its requests fail or are refused, for describePages.
 * Call it right after launching the browser.
 */
export function watchBrowser(browser: Browser): void {
  for (const context of browser.contexts()) watchContext(context);
  browser.on("context", watchContext);
}

interface Shown {
  url: string;
  title: string;
  text: string;
}

/** Reads what page shows, or says it couldn't within answerWithin. */
async function read(page: Page): Promise<Shown> {
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<Shown>((done) => {
    timer = setTimeout(
      () => done({ url: page.url(), title: "", text: `(it didn't answer within ${answerWithin / 1000} s)` }),
      answerWithin,
    );
  });
  const shown = await Promise.race([
    page
      .evaluate(() => ({
        url: location.href,
        title: document.title,
        text: (document.body?.innerText ?? "").replace(/\s+/g, " ").trim(),
      }))
      .catch((err: unknown) => ({ url: page.url(), title: "", text: `(couldn't read it: ${String(err)})` })),
    late,
  ]);
  clearTimeout(timer);
  return shown;
}

/** Says where each watched page that's still open is and what it shows, newest first. */
export async function describePages(): Promise<string> {
  const parts: string[] = [];
  for (const w of [...watched].reverse()) {
    if (w.page.isClosed()) continue;
    const shown = await read(w.page);
    const text = shown.text.length > 1500 ? `${shown.text.slice(0, 1500)}…` : shown.text;
    parts.push(
      [
        `the page at ${withoutToken(shown.url)} (${shown.title || "no title"}) showed: ${text || "nothing"}`,
        w.said.length > 0 ? `  its console said:\n    ${w.said.join("\n    ")}` : "",
        w.failed.length > 0 ? `  requests that failed or were refused:\n    ${w.failed.join("\n    ")}` : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
  return parts.length > 0 ? parts.join("\n") : "no page was open";
}
