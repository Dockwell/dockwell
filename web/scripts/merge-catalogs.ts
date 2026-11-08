// SPDX-License-Identifier: AGPL-3.0-only

// Merges the panel's message catalogs after a merge, a rebase or a cherry-pick,
// whether git conflicted on them or not: a merge git makes on its own can be
// wrong too. Two branches that each add one message with different
// translations leave it in twice, and a message one side stopped using, which
// the other uses somewhere new, can be lost with its translation.
//
//   make i18n-merge               while a merge, rebase or cherry-pick is stopped,
//                                 or with a merge commit checked out
//   make i18n-merge FROM=<rev>    after one that finished: a rebase's is ORIG_HEAD,
//                                 a cherry-pick's the commit picked
//
// It reads each catalog as the two sides had it and as they started, and keeps
// every message either side has, with the translation a side changed, a cleared
// one included, and the header as it was. Then it runs lingui extract, which
// puts the catalog in order for the merged code and drops what nothing uses,
// and fills what that left untranslated from either side. When both sides
// translated one message differently, or one side holds a message twice with
// two translations, as a clean merge leaves one two branches each added, it
// says which and writes nothing, as only a person can choose. Resolve conflicts in the code first: extracting
// reads it. The tests are in catalogs.test.ts
// (docs/adr/0051-files-that-merge-without-conflicts.md).

import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { parseArgs } from "node:util";

import { type Context, type Merged, mergeCatalog, refill, type Sides } from "./catalogs.ts";
import { type Sources, sources } from "./sides.ts";

const web = join(import.meta.dirname, "..");
const root = git("rev-parse", "--show-toplevel");

function git(...args: string[]): string {
  return execFileSync("git", args, { cwd: web, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}

/** A file as a commit has it, or nothing when it has none. */
function show(rev: string, path: string): string | undefined {
  try {
    return execFileSync("git", ["show", `${rev}:${path}`], { cwd: root, encoding: "utf8", stdio: "pipe" });
  } catch {
    return undefined;
  }
}

function fail(message: string): never {
  console.error(`merge-catalogs: ${message}`);
  process.exit(1);
}

const { values } = parseArgs({ options: { from: { type: "string" } } });
let found: Sources;
try {
  found = sources(web, values.from);
} catch (err) {
  fail((err as Error).message);
}
const { base, ours, theirs, what } = found;
const locales = join(web, "src", "locales");
const catalogs = readdirSync(locales)
  .map((locale) => join(locales, locale, "messages.po"))
  .filter((path) => existsSync(path));

const merged: { path: string; ctx: Context; sides: Sides; result: Merged }[] = [];
let disagree = false;
for (const path of catalogs) {
  const file = relative(root, path);
  const locale = file.split("/")[3] ?? "";
  const ctx = { locale, sourceLocale: "en", filename: file };
  const sides = { base: show(base, file), ours: show(ours, file), theirs: show(theirs, file) };
  const result = await mergeCatalog(sides, ctx);
  if (result.disagree.length > 0) {
    disagree = true;
    console.error(`merge-catalogs: in ${file}, these messages have two translations:`);
    for (const line of result.disagree) console.error(`  ${line}`);
  }
  merged.push({ path, ctx, sides, result });
}
if (disagree) {
  fail(
    "nothing was written. Choose one translation of each message above, keep it in the catalog once, then run make i18n.",
  );
}

for (const { path, result } of merged) writeFileSync(path, result.text);
try {
  execFileSync(join(web, "node_modules", ".bin", "lingui"), ["extract", "--clean"], { cwd: web, stdio: "pipe" });
} catch (err) {
  const output = (err as { stderr?: Buffer; stdout?: Buffer }).stderr?.toString() || String(err);
  fail(
    `lingui extract failed, so the catalogs hold both sides' messages unsorted. Resolve any conflict left in the code, then run make i18n.\n${output}`,
  );
}
for (const { path, ctx, sides, result } of merged) {
  const { text, filled } = await refill(readFileSync(path, "utf8"), result, sides, ctx);
  if (filled > 0) writeFileSync(path, text);
  const file = relative(root, path);
  console.log(
    `merge-catalogs: merged ${file} from ${what}${filled > 0 ? `, and translated ${filled} from either side` : ""}.`,
  );
}
console.log("merge-catalogs: check them with git diff, and git add them if git had them in conflict.");
