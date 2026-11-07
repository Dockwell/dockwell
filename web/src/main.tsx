// SPDX-License-Identifier: AGPL-3.0-only

import "@dockwell/ui/styles.css";

import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { createApi } from "./app/api";
import { activateLocale, pickLocale } from "./app/i18n";
import { Providers } from "./app/Providers";
import { router } from "./app/router";

const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

// Translations load before the first render, so text never flashes in English.
await activateLocale(pickLocale(navigator.languages));

const { transport, queryClient } = createApi();
createRoot(root).render(
  <StrictMode>
    <Providers transport={transport} queryClient={queryClient}>
      <RouterProvider router={router} />
    </Providers>
  </StrictMode>,
);
// A change for testing which CI jobs run. Not for merging.
