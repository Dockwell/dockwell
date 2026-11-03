// SPDX-License-Identifier: AGPL-3.0-only
// Applies a chosen theme before the first paint, so it never flashes. The key
// matches THEME_STORAGE_KEY in @dockwell/ui; "system" is stored as no key.
(() => {
  try {
    const theme = localStorage.getItem("dockwell.theme");
    if (theme === "light" || theme === "dark") document.documentElement.setAttribute("data-theme", theme);
  } catch (_) {
    // Storage is blocked; the system theme applies.
  }
})();
