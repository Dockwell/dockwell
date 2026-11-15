// SPDX-License-Identifier: AGPL-3.0-only
// What a page showed when a run failed. A step that times out says only what it
// waited for; this says where the page was instead, what its open sheets and the
// page showed, what it said to its console, which of its requests failed and which
// were still waiting, so a failure in CI explains itself without a screenshot.
import type { Browser, BrowserContext, Page, Request } from "playwright-core";

interface Watched {
  page: Page;
  said: string[];
  failed: string[];
  /** Requests sent and not yet answered, with when each was sent. */
  waiting: Map<Request, number>;
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
  const w: Watched = { page, said: [], failed: [], waiting: new Map() };
  page.on("request", (req) => w.waiting.set(req, Date.now()));
  page.on("requestfinished", (req) => w.waiting.delete(req));
  page.on("console", (msg) => {
    if (msg.type() === "error" || msg.type() === "warning") remember(w.said, `${msg.type()}: ${msg.text()}`);
  });
  page.on("requestfailed", (req) => {
    w.waiting.delete(req);
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
  /** What each open dialog says, by its name, as the page's text may be cut before it. */
  dialogs: string[];
}

/** How long a request waits before a failure names it: the panel's live streams never end, so they're left out. */
const waitedLong = 2_000;

/** The requests a page sent at least waitedLong ago and has no answer to, but its live streams. */
function stillWaiting(w: Watched): string[] {
  const now = Date.now();
  return [...w.waiting]
    .filter(
      ([req, at]) =>
        now - at >= waitedLong &&
        req.method() === "POST" &&
        !/\/(Watch|Follow|Stream|Subscribe)\w*$/.test(new URL(req.url()).pathname),
    )
    .map(([req, at]) => `${req.method()} ${withoutToken(req.url())}, for ${Math.round((now - at) / 1000)} s`)
    .slice(-keep);
}

/** Reads what page shows, or says it couldn't within answerWithin. */
async function read(page: Page): Promise<Shown> {
  let timer: NodeJS.Timeout | undefined;
  const late = new Promise<Shown>((done) => {
    timer = setTimeout(
      () =>
        done({ url: page.url(), title: "", text: `(it didn't answer within ${answerWithin / 1000} s)`, dialogs: [] }),
      answerWithin,
    );
  });
  const shown = await Promise.race([
    page
      .evaluate(() => ({
        url: location.href,
        title: document.title,
        text: (document.body?.innerText ?? "").replace(/\s+/g, " ").trim(),
        dialogs: [...document.querySelectorAll<HTMLElement>("[role=dialog]")].map((d) => {
          const name =
            d.getAttribute("aria-label") ??
            document.getElementById(d.getAttribute("aria-labelledby") ?? "")?.innerText ??
            "";
          return `${name || "a dialog"}: ${d.innerText.replace(/\s+/g, " ").trim()}`;
        }),
      }))
      .catch((err: unknown) => ({
        url: page.url(),
        title: "",
        text: `(couldn't read it: ${String(err)})`,
        dialogs: [],
      })),
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
    const waiting = stillWaiting(w);
    const shown = await read(w.page);
    const text = shown.text.length > 1500 ? `${shown.text.slice(0, 1500)}…` : shown.text;
    parts.push(
      [
        `the page at ${withoutToken(shown.url)} (${shown.title || "no title"}) showed: ${text || "nothing"}`,
        shown.dialogs.length > 0
          ? `  its open dialogs said:\n    ${shown.dialogs.map((d) => (d.length > 800 ? `${d.slice(0, 800)}…` : d)).join("\n    ")}`
          : "",
        w.said.length > 0 ? `  its console said:\n    ${w.said.join("\n    ")}` : "",
        w.failed.length > 0 ? `  requests that failed or were refused:\n    ${w.failed.join("\n    ")}` : "",
        waiting.length > 0 ? `  requests still waiting for an answer:\n    ${waiting.join("\n    ")}` : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
  return parts.length > 0 ? parts.join("\n") : "no page was open";
}
