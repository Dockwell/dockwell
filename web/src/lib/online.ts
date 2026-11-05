// SPDX-License-Identifier: AGPL-3.0-only

import { onlineManager } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";

/** Whether this browser thinks it's online, as reads go by it: they wait while it isn't. */
export function useOnline(): boolean {
  return useSyncExternalStore(
    (changed) => onlineManager.subscribe(changed),
    () => onlineManager.isOnline(),
  );
}
