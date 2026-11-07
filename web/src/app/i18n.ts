// SPDX-License-Identifier: AGPL-3.0-only

import { i18n, type Messages } from "@lingui/core";

/** The locales the panel ships, named in their own language. */
export const LOCALES = {
  en: "English",
  "zh-CN": "简体中文",
} as const;

export type Locale = keyof typeof LOCALES;

/**
 * Picks the first supported locale from the browser's preferences. Chinese
 * maps to Simplified Chinese only where that is the script people read; a
 * Traditional Chinese reader gets English until a translation exists.
 */
export function pickLocale(preferred: readonly string[]): Locale {
  for (const tag of preferred) {
    const lower = tag.toLowerCase();
    if (lower === "en" || lower.startsWith("en-")) return "en";
    if (lower === "zh" || lower === "zh-cn" || lower === "zh-sg" || lower.startsWith("zh-hans")) return "zh-CN";
  }
  return "en";
}

const catalogs: Record<Locale, () => Promise<{ messages: Messages }>> = {
  en: () => import("../locales/en/messages.po"),
  "zh-CN": () => import("../locales/zh-CN/messages.po"),
};

/** Where this browser keeps the language someone chose in Settings. */
const CHOSEN_KEY = "dockwell.locale";

/** The language this browser was told to show, or nothing when it follows the browser's own. */
export function chosenLocale(): Locale | undefined {
  try {
    const chosen = localStorage.getItem(CHOSEN_KEY);
    return chosen !== null && Object.hasOwn(LOCALES, chosen) ? (chosen as Locale) : undefined;
  } catch {
    return undefined;
  }
}

/** The language the panel starts in: the one chosen in this browser, or the browser's own. */
export function startingLocale(): Locale {
  return chosenLocale() ?? pickLocale(navigator.languages);
}

/**
 * Shows the panel in locale in this browser from now on, or in the browser's own
 * language again with nothing. It's this browser's alone: another keeps its own.
 */
export async function chooseLocale(locale: Locale | undefined): Promise<void> {
  try {
    if (locale) localStorage.setItem(CHOSEN_KEY, locale);
    else localStorage.removeItem(CHOSEN_KEY);
  } catch {
    // Storage refused, as in a private window: it applies until the page reloads.
  }
  await activateLocale(locale ?? pickLocale(navigator.languages));
}

/** Loads a locale's catalog and switches the whole panel to it. */
export async function activateLocale(locale: Locale): Promise<void> {
  const { messages } = await catalogs[locale]();
  i18n.loadAndActivate({ locale, messages });
  document.documentElement.lang = locale;
}

export { i18n };
