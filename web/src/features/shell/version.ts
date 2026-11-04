// SPDX-License-Identifier: AGPL-3.0-only

// Go pseudo-versions name a commit: after a release, the next one with a
// time and hash (v0.4.1-0.20261103214000-3f2a9c1d4e5b); before any tag,
// v0.0.0 with them (v0.0.0-20261103214000-3f2a9c1d4e5b). Builds with local
// changes add +dirty.
const PSEUDO = /^v?(\d+\.\d+\.\d+)-(?:[0-9A-Za-z.]+\.)?0\.\d{14}-[0-9a-f]{12}(?:\+.*)?$/;
const UNTAGGED = /^v?\d+\.0\.0-\d{14}-[0-9a-f]{12}(?:\+.*)?$/;

/**
 * A build's version, short enough for the rail: "0.4.0" for a release, and
 * "0.4.1-dev" for a build on the way to 0.4.1, "dev" for one before any
 * release. Anything else, such as "devel", stays as it is.
 */
export function shortVersion(version: string): string {
  if (UNTAGGED.test(version)) return "dev";
  const pseudo = PSEUDO.exec(version);
  if (pseudo) return `${pseudo[1]}-dev`;
  return version.replace(/^v(?=\d)/, "");
}
