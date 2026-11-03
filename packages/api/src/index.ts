// SPDX-License-Identifier: AGPL-3.0-only

// The Dockwell API for TypeScript: messages and service descriptors generated
// from proto/. Pair a service with a Connect transport to call it:
//
//   import { createClient } from "@connectrpc/connect";
//   import { SystemService } from "@dockwell/api";
//   const system = createClient(SystemService, transport);
//   const { build } = await system.getVersion({});

export * from "./gen/dockwell/v1/options_pb";
export * from "./gen/dockwell/v1/system_pb";
