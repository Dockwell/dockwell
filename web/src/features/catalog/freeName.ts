// SPDX-License-Identifier: AGPL-3.0-only

/** The next name from one that's taken, past those here: mysql-2 for mysql, and mysql-3 for mysql-2. */
export function nextFree(name: string, taken: readonly string[]): string {
  const numbered = /^(.*)-([1-9]\d*)$/.exec(name);
  const stem = numbered?.[1] ?? name;
  let n = numbered ? Number(numbered[2]) + 1 : 2;
  while (taken.includes(`${stem}-${n}`)) n++;
  return `${stem}-${n}`;
}
