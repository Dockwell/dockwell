// SPDX-License-Identifier: AGPL-3.0-only

/**
 * xterm.js styles its rows, their colours and its scrollbar with <style> elements
 * it makes as it goes, and the panel's Content-Security-Policy refuses every
 * inline style, as it should. So the same rules go in through the CSSOM, as
 * constructed style sheets, which the policy leaves alone, and the policy stays
 * as it is. Only xterm makes style elements under host: what a terminal prints is
 * drawn as text, never as markup, so nothing it prints can reach them.
 *
 * Returns what takes the sheets away again.
 */
export function adoptStyles(host: HTMLElement): () => void {
  if (
    typeof CSSStyleSheet === "undefined" ||
    !("replaceSync" in CSSStyleSheet.prototype) ||
    !document.adoptedStyleSheets
  ) {
    return () => {};
  }
  const sheets = new Map<HTMLStyleElement, CSSStyleSheet>();
  const drop = (gone: Set<CSSStyleSheet>) => {
    if (gone.size > 0) document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => !gone.has(s));
  };
  const sync = () => {
    const seen = new Set<HTMLStyleElement>();
    for (const el of host.querySelectorAll("style")) {
      seen.add(el);
      let sheet = sheets.get(el);
      if (!sheet) {
        sheet = new CSSStyleSheet();
        sheets.set(el, sheet);
        document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
      }
      sheet.replaceSync(el.textContent ?? "");
    }
    const gone = new Set<CSSStyleSheet>();
    for (const [el, sheet] of sheets) {
      if (!seen.has(el)) {
        gone.add(sheet);
        sheets.delete(el);
      }
    }
    drop(gone);
  };
  const observer = new MutationObserver(sync);
  observer.observe(host, { subtree: true, childList: true, characterData: true });
  sync();
  return () => {
    observer.disconnect();
    drop(new Set(sheets.values()));
    sheets.clear();
  };
}
