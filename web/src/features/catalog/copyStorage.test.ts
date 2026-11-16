// SPDX-License-Identifier: AGPL-3.0-only
// Copy as command names the connection the sheet shows, on required storage with
// two free connections, as the server would default to another (slice 2 re-check, N1).
import { create } from "@bufbuild/protobuf";
import { CatalogNeedOptionSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";
import { needsRequest, pickedWay } from "./needs";

describe("Copy as command on required storage with two free connections", () => {
  it("names the connection the sheet shows", () => {
    const archive = create(CatalogNeedOptionSchema, {
      need: "storage",
      choice: "connection:archive",
      connection: "archive",
    });
    const named = create(CatalogNeedOptionSchema, {
      need: "storage",
      choice: "connection:uploads-production",
      connection: "uploads-production",
    });
    const options = new Map([["storage", [archive, named]]]);
    const needs = [{ name: "storage" }] as never;
    expect(pickedWay([archive, named], undefined)?.choice).toBe("connection:archive");
    expect(needsRequest(needs, options, {}, false)).toEqual({ storage: "connection:archive" });
    expect(needsRequest(needs, options, {}, true)).toEqual({ storage: "connection:archive" });
  });
});
