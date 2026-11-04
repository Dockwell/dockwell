// SPDX-License-Identifier: AGPL-3.0-only

/** Apple keyboards say ⌘ where everyone else's say Ctrl. */
export function onApple(): boolean {
  const platform =
    (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ?? navigator.platform;
  return /mac|iphone|ipad|ipod/i.test(platform ?? "");
}
