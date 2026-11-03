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

/** Loads a locale's catalog and switches the whole panel to it. */
export async function activateLocale(locale: Locale): Promise<void> {
  const { messages } = await catalogs[locale]();
  i18n.loadAndActivate({ locale, messages });
  document.documentElement.lang = locale;
}

export { i18n };
