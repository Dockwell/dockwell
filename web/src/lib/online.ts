// SPDX-License-Identifier: AGPL-3.0-only

import { onlineManager } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";

/** Why the panel can't read how things are: this browser is offline, or the control plane doesn't answer. */
export type Unread = "offline" | "unreachable";

/** Whether this browser thinks it's online, as reads go by it: they wait while it isn't. */
export function useOnline(): boolean {
  return useSyncExternalStore(
    (changed) => onlineManager.subscribe(changed),
    () => onlineManager.isOnline(),
  );
}
