// SPDX-License-Identifier: AGPL-3.0-only

import "@dockwell/ui/styles.css";
import "./viewer.css";

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import icon from "../../../../design/system/brand/dockwell-icon.svg";
import { App } from "./App";

document.querySelector<HTMLLinkElement>('link[rel="icon"]')?.setAttribute("href", icon);

const container = document.getElementById("root");
if (!container) throw new Error("index.html has no #root element.");
createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
