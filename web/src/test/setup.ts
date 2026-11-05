// SPDX-License-Identifier: AGPL-3.0-only

import "@testing-library/jest-dom/vitest";
import { cleanup, configure } from "@testing-library/react";
import { afterEach, beforeEach, vi } from "vitest";

import { activateLocale } from "../app/i18n";

// The default of 1 s is shorter than a first render takes on a busy machine:
// Go's and the panel's tests run side by side, and a cold run under load has
// taken over 2 s. A test that fails waits this long, and one that passes doesn't.
configure({ asyncUtilTimeout: 4_000 });

beforeEach(async () => {
  // jsdom has no scrolling; the router's scroll restoration calls it.
  vi.stubGlobal("scrollTo", () => {});
  await activateLocale("en");
});

afterEach(() => {
  cleanup();
});
