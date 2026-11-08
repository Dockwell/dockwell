// SPDX-License-Identifier: AGPL-3.0-only

import "@dockwell/ui/styles.css";

import { followThemeAcrossTabs } from "@dockwell/ui";
import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { createApi } from "./app/api";
import { activateLocale, startingLocale } from "./app/i18n";
import { Providers } from "./app/Providers";
import { router } from "./app/router";

const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

// A theme chosen in another tab shows here too, on any screen.
followThemeAcrossTabs();

// Translations load before the first render, so text never flashes in English.
await activateLocale(startingLocale());

const { transport, queryClient } = createApi();
createRoot(root).render(
  <StrictMode>
    <Providers transport={transport} queryClient={queryClient}>
      <RouterProvider router={router} />
    </Providers>
  </StrictMode>,
);
