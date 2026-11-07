// SPDX-License-Identifier: AGPL-3.0-only

import { act } from "@testing-library/react";

/** What a test's router says of itself: enough to tell it has finished loading. */
interface Settling {
  state: { status: "pending" | "idle"; isLoading: boolean };
}

const open = new Set<Settling>();

/** track keeps a router a test rendered, for settleRouters to wait on. */
export function track<R extends Settling>(router: R): R {
  open.add(router);
  return router;
}

/**
 * settleRouters waits, up to 2 s, for every router a test rendered to finish
 * what it was loading, and forgets them. A test that ends as a navigation
 * begins otherwise leaves the router's transition running, and when it lands
 * after the file's window has gone, React throws, failing a run whose tests
 * all passed.
 */
export async function settleRouters(): Promise<void> {
  const deadline = Date.now() + 2_000;
  for (const router of open) {
    while ((router.state.status !== "idle" || router.state.isLoading) && Date.now() < deadline) {
      await act(() => new Promise((resolve) => setTimeout(resolve, 10)));
    }
  }
  open.clear();
}
