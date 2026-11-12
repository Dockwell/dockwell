// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The available version split where it starts to differ from the installed one,
 * at the start of the part that changed, so "3.0.13-0ubuntu3.5" against
 * "3.0.13-0ubuntu3.4" shows its last part as what's new.
 */
export function splitVersion(installed: string, available: string): [same: string, changed: string] {
  if (!installed) return ["", available];
  let i = 0;
  while (i < installed.length && i < available.length && installed[i] === available[i]) i++;
  while (i > 0 && !/[.\-~+:]/.test(available[i - 1] ?? "")) i--;
  return [available.slice(0, i), available.slice(i)];
}
