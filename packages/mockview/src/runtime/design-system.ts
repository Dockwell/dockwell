// SPDX-License-Identifier: AGPL-3.0-only

// Puts React on window and runs the design system's reference bundle, which
// is a classic script that reads window.React and assigns window.Dockwell.
// React 19 ships no UMD build, so the npm packages stand in for one.

import * as React from "react";
import * as ReactDOM from "react-dom";
import * as ReactDOMClient from "react-dom/client";

import bundleSource from "../../../../design/system/components/bundle.js?raw";

declare global {
  interface Window {
    React?: unknown;
    ReactDOM?: unknown;
    Dockwell?: Record<string, unknown>;
  }
}

/** Installs the globals once and returns window.Dockwell. */
export function installDesignSystem(): Record<string, unknown> {
  window.React = React;
  window.ReactDOM = { ...ReactDOM, ...ReactDOMClient };
  if (!window.Dockwell) {
    new Function(`${bundleSource}\n//# sourceURL=design/system/components/bundle.js`)();
  }
  if (!window.Dockwell) throw new Error("design/system/components/bundle.js ran but didn't define window.Dockwell.");
  return window.Dockwell;
}
