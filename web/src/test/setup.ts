// SPDX-License-Identifier: AGPL-3.0-only

import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach, vi } from "vitest";

import { activateLocale } from "../app/i18n";

beforeEach(async () => {
  // jsdom has no scrolling; the router's scroll restoration calls it.
  vi.stubGlobal("scrollTo", () => {});
  await activateLocale("en");
});

afterEach(() => {
  cleanup();
});
