// SPDX-License-Identifier: AGPL-3.0-only

/** The folder a path sits in: "/" for what's at the top. */
export function parentOf(path: string): string {
  const cut = path.replace(/\/+$/, "").lastIndexOf("/");
  return cut <= 0 ? "/" : path.slice(0, cut);
}

/** What a path is called: "/" for the top. */
export function nameOf(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  return trimmed.slice(trimmed.lastIndexOf("/") + 1) || "/";
}

/** name in the folder dir. */
export function join(dir: string, name: string): string {
  return dir === "/" ? `/${name}` : `${dir}/${name}`;
}

/** Each folder on the way to a path, with what it's called and where it is, from the top. */
export function crumbs(path: string): { name: string; path: string }[] {
  const out = [{ name: "/", path: "/" }];
  for (const name of path.split("/").filter(Boolean)) {
    out.push({ name, path: join(out[out.length - 1]?.path ?? "/", name) });
  }
  return out;
}

/** Which line ending a text uses, for the editor's footer. */
export function lineEnding(text: string): "LF" | "CRLF" {
  return text.includes("\r\n") ? "CRLF" : "LF";
}

/** How many lines a text has, as an editor numbers them: a last newline starts none. */
export function lineCount(text: string): number {
  if (text === "") return 1;
  let lines = text.endsWith("\n") ? 0 : 1;
  for (let at = text.indexOf("\n"); at !== -1; at = text.indexOf("\n", at + 1)) lines++;
  return lines;
}

/** The line numbers beside a text, one to a line. */
export function lineNumbers(count: number): string {
  return Array.from({ length: count }, (_, i) => i + 1).join("\n");
}
