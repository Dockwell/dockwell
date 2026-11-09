// SPDX-License-Identifier: Apache-2.0

// The Dockwell API for TypeScript: messages and service descriptors generated
// from proto/. Pair a service with a Connect transport to call it:
//
//   import { createClient } from "@connectrpc/connect";
//   import { SystemService } from "@dockwell/api";
//   const system = createClient(SystemService, transport);
//   const { build } = await system.getVersion({});

export * from "./gen/dockwell/catalog/v1alpha1/entry_pb";
export * from "./gen/dockwell/resource/v1alpha1/specs_pb";
export * from "./gen/dockwell/v1/audit_pb";
export * from "./gen/dockwell/v1/auth_pb";
export * from "./gen/dockwell/v1/backup_pb";
export * from "./gen/dockwell/v1/catalog_pb";
export * from "./gen/dockwell/v1/change_pb";
export * from "./gen/dockwell/v1/compose_pb";
export * from "./gen/dockwell/v1/container_pb";
export * from "./gen/dockwell/v1/database_pb";
export * from "./gen/dockwell/v1/domain_pb";
export * from "./gen/dockwell/v1/event_pb";
export * from "./gen/dockwell/v1/file_pb";
export * from "./gen/dockwell/v1/firewall_pb";
export * from "./gen/dockwell/v1/health_pb";
export * from "./gen/dockwell/v1/import_pb";
export * from "./gen/dockwell/v1/log_pb";
export * from "./gen/dockwell/v1/member_pb";
export * from "./gen/dockwell/v1/options_pb";
export * from "./gen/dockwell/v1/process_pb";
export * from "./gen/dockwell/v1/resource_pb";
export * from "./gen/dockwell/v1/server_pb";
export * from "./gen/dockwell/v1/service_pb";
export * from "./gen/dockwell/v1/settings_pb";
export * from "./gen/dockwell/v1/system_pb";
export * from "./gen/dockwell/v1/task_pb";
export * from "./gen/dockwell/v1/terminal_pb";
export * from "./gen/dockwell/v1/token_pb";
export * from "./gen/dockwell/v1/update_pb";
export * from "./gen/dockwell/v1/variable_pb";
export * from "./gen/dockwell/v1/website_pb";
