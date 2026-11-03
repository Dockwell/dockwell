// SPDX-License-Identifier: AGPL-3.0-only

import "@dockwell/ui/styles.css";

import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { createQueryClient, createTransport } from "./app/api";
import { activateLocale, pickLocale } from "./app/i18n";
import { Providers } from "./app/Providers";
import { router } from "./app/router";

const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

// Translations load before the first render, so text never flashes in English.
await activateLocale(pickLocale(navigator.languages));

createRoot(root).render(
  <StrictMode>
    <Providers transport={createTransport()} queryClient={createQueryClient()}>
      <RouterProvider router={router} />
    </Providers>
  </StrictMode>,
);
