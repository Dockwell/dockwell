// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { ComposeWorkspaceResourceSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { composePlacementGroups, copyKey, onComposeServer, selectedVolumeCopies } from "./composePlacement";

describe("Compose changes on a server", () => {
  const volume = create(ComposeWorkspaceResourceSchema, {
    kind: "volume",
    name: "data",
    resourceId: "data-id",
    serverIds: ["other", "viewed"],
  });
  const network = create(ComposeWorkspaceResourceSchema, {
    kind: "network",
    name: "db",
    resourceId: "network-id",
    serverIds: ["viewed"],
  });
  const shared = create(ComposeWorkspaceResourceSchema, {
    kind: "service",
    name: "db",
    resourceId: "db-id",
    serverIds: ["other", "viewed"],
  });
  const remote = create(ComposeWorkspaceResourceSchema, {
    kind: "service",
    name: "worker",
    resourceId: "worker-id",
    serverIds: ["other"],
  });
  const resources = [volume, network, shared, remote];
  const viewed = onComposeServer(resources, "viewed");

  it("selects this server's resources and shows every affected copy with this server first", () => {
    expect(composePlacementGroups(viewed, "viewed")).toEqual([
      ["viewed", [volume, network, shared]],
      ["other", [volume, shared]],
    ]);
  });

  it("keeps volumes by default and requires consent for each copy of a shared volume", () => {
    expect(selectedVolumeCopies(viewed, new Set())).toEqual({ all: [], partial: false });
    expect(selectedVolumeCopies(viewed, new Set([copyKey(volume, "viewed")]))).toEqual({ all: [], partial: true });
    expect(selectedVolumeCopies(viewed, new Set([copyKey(volume, "viewed"), copyKey(volume, "other")]))).toEqual({
      all: [volume],
      partial: false,
    });
  });
});
