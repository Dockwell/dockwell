// SPDX-License-Identifier: AGPL-3.0-only

// Which commits merge-catalogs.ts merges the catalogs of: the one both sides
// started from, and each side. The tests are in sides.test.ts.

import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { resolve } from "node:path";

export interface Sources {
  base: string;
  ours: string;
  theirs: string;
  /** What the sides came from, for the report. */
  what: string;
}

/**
 * The sides in the repository at dir: from, when it's given, whatever else git
 * says is going on; else a stopped merge, rebase or cherry-pick; else the merge
 * commit checked out. Git can leave REBASE_HEAD behind after a rebase that
 * stopped and then finished, so it counts only while a rebase is under way.
 * Throws, saying what to run, when there are no two sides.
 */
export function sources(dir: string, from?: string): Sources {
  const git = (...args: string[]) =>
    execFileSync("git", args, { cwd: dir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  const commit = (name: string): string | undefined => {
    try {
      return git("rev-parse", "-q", "--verify", `${name}^{commit}`);
    } catch {
      return undefined;
    }
  };
  const head = commit("HEAD");
  if (!head) throw new Error("there's no commit checked out to merge catalogs in.");
  if (from) {
    const theirs = commit(from);
    if (!theirs) throw new Error(`${from} isn't a commit. Name the branch as it was before, such as ORIG_HEAD.`);
    return { base: git("merge-base", head, theirs), ours: head, theirs, what: `HEAD and ${from}` };
  }
  const merging = commit("MERGE_HEAD");
  if (merging) return { base: git("merge-base", head, merging), ours: head, theirs: merging, what: "the merge" };
  const rebasing = ["rebase-merge", "rebase-apply"].some((name) =>
    existsSync(resolve(dir, git("rev-parse", "--git-path", name))),
  );
  const rebased = rebasing ? commit("REBASE_HEAD") : undefined;
  if (rebased) return { base: commit(`${rebased}^`) ?? rebased, ours: head, theirs: rebased, what: "the rebase" };
  const picked = commit("CHERRY_PICK_HEAD");
  if (picked) return { base: commit(`${picked}^`) ?? picked, ours: head, theirs: picked, what: "the cherry-pick" };
  const parents = git("rev-list", "--parents", "-n", "1", head).split(" ").slice(1);
  if (parents.length === 2 && parents[0] && parents[1]) {
    const [ours, theirs] = [parents[0], parents[1]];
    return { base: git("merge-base", ours, theirs), ours, theirs, what: "the merge commit" };
  }
  throw new Error(
    "no merge, rebase or cherry-pick is stopped, and HEAD isn't a merge commit, so there are no two sides to merge. After a rebase that finished, run make i18n-merge FROM=ORIG_HEAD; after a cherry-pick, FROM=<the commit you picked>.",
  );
}
