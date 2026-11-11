// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { envLeftOut } from "./ExportSheet";

describe("envLeftOut", () => {
  it("names each variable once, as an .env sets it, and leaves out a volume's options", () => {
    expect(envLeftOut(["web.API_KEY", "worker.API_KEY", "db.POSTGRES_PASSWORD", "volumes.data.driver_opts"])).toEqual([
      "API_KEY",
      "POSTGRES_PASSWORD",
    ]);
    expect(envLeftOut(["volumes.data.driver_opts"])).toEqual([]);
  });
});
