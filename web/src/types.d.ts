// SPDX-License-Identifier: AGPL-3.0-only

// Catalogs are compiled on import by @lingui/vite-plugin.
declare module "*.po" {
  import type { Messages } from "@lingui/core";
  export const messages: Messages;
}
