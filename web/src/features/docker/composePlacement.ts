// SPDX-License-Identifier: AGPL-3.0-only

import type { ComposeWorkspaceResource } from "@dockwell/api";

export function onComposeServer(resources: readonly ComposeWorkspaceResource[], serverId: string) {
  return resources.filter((item) => item.serverIds.includes(serverId));
}

export function composePlacementGroups(resources: readonly ComposeWorkspaceResource[], current: string) {
  const groups = new Map<string, ComposeWorkspaceResource[]>([[current, []]]);
  for (const resource of resources) {
    for (const server of resource.serverIds) {
      const group = groups.get(server) ?? [];
      group.push(resource);
      groups.set(server, group);
    }
  }
  return [...groups].filter(([, resources]) => resources.length > 0);
}

export function copyKey(resource: ComposeWorkspaceResource, server: string) {
  return `${resource.resourceId}/${server}`;
}

export function selectedVolumeCopies(resources: readonly ComposeWorkspaceResource[], checked: ReadonlySet<string>) {
  const volumes = resources.filter((resource) => resource.kind === "volume");
  const all = volumes.filter((volume) => volume.serverIds.every((server) => checked.has(copyKey(volume, server))));
  return { all, partial: all.reduce((count, volume) => count + volume.serverIds.length, 0) !== checked.size };
}
