// SPDX-License-Identifier: AGPL-3.0-only

import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { sources } from "./sides.ts";

let dir = "";
const env = {
  ...process.env,
  GIT_AUTHOR_NAME: "t",
  GIT_AUTHOR_EMAIL: "t@example.com",
  GIT_COMMITTER_NAME: "t",
  GIT_COMMITTER_EMAIL: "t@example.com",
  GIT_CONFIG_NOSYSTEM: "1",
  GIT_CONFIG_GLOBAL: "/dev/null",
};

function git(...args: string[]): string {
  return execFileSync("git", ["-c", "rerere.enabled=false", ...args], {
    cwd: dir,
    env,
    encoding: "utf8",
    stdio: "pipe",
  }).trim();
}

/** Writes file with text and commits it on the branch checked out, returning the commit. */
function commit(text: string, message: string): string {
  writeFileSync(join(dir, "file.txt"), text);
  git("add", "file.txt");
  git("commit", "-q", "-m", message);
  return git("rev-parse", "HEAD");
}

/** main with one commit, and a branch from it that changes the same line, so rebasing it stops. */
function twoBranches(): { start: string; main: string; feature: string } {
  git("init", "-q", "-b", "main");
  const start = commit("one\n", "start");
  git("checkout", "-q", "-b", "feature");
  const feature = commit("feature\n", "feature");
  git("checkout", "-q", "main");
  const main = commit("main\n", "main");
  return { start, main, feature };
}

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "sides-"));
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe("sources", () => {
  it("reads a stopped rebase's sides from REBASE_HEAD", () => {
    const { start, main, feature } = twoBranches();
    git("checkout", "-q", "feature");
    expect(() => git("rebase", "-q", "main")).toThrow();
    expect(sources(dir)).toEqual({ base: start, ours: main, theirs: feature, what: "the rebase" });
  });

  it("ignores a REBASE_HEAD left behind after a rebase finished, for the merge commit checked out", () => {
    const { main, feature } = twoBranches();
    git("merge", "-q", "--no-edit", "-X", "ours", "feature");
    // What git 2.47 leaves after a rebase that stopped and then finished.
    git("update-ref", "REBASE_HEAD", feature);
    expect(sources(dir).what).toBe("the merge commit");
    expect(sources(dir).theirs).toBe(feature);
    expect(sources(dir).ours).toBe(main);
  });

  it("takes FROM over anything git says is going on", () => {
    const { start, main, feature } = twoBranches();
    git("update-ref", "REBASE_HEAD", main);
    expect(sources(dir, feature)).toEqual({ base: start, ours: main, theirs: feature, what: `HEAD and ${feature}` });
    git("checkout", "-q", "feature");
    expect(() => git("rebase", "-q", "main")).toThrow();
    expect(sources(dir, "ORIG_HEAD").what).toBe("HEAD and ORIG_HEAD");
  });

  it("reads a stopped merge's sides from MERGE_HEAD", () => {
    const { start, main, feature } = twoBranches();
    expect(() => git("merge", "-q", "--no-edit", "feature")).toThrow();
    expect(sources(dir)).toEqual({ base: start, ours: main, theirs: feature, what: "the merge" });
  });

  it("says what to run when there are no two sides", () => {
    twoBranches();
    expect(() => sources(dir)).toThrow(/FROM=ORIG_HEAD; after a cherry-pick, FROM=<the commit you picked>/);
  });
});
