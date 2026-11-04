// SPDX-License-Identifier: Apache-2.0

// The Dockwell API for TypeScript: messages and service descriptors generated
// from proto/. Pair a service with a Connect transport to call it:
//
//   import { createClient } from "@connectrpc/connect";
//   import { SystemService } from "@dockwell/api";
//   const system = createClient(SystemService, transport);
//   const { build } = await system.getVersion({});

export * from "./gen/dockwell/v1/auth_pb";
export * from "./gen/dockwell/v1/options_pb";
export * from "./gen/dockwell/v1/server_pb";
export * from "./gen/dockwell/v1/system_pb";
export * from "./gen/dockwell/v1/token_pb";
