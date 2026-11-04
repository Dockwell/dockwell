// SPDX-License-Identifier: AGPL-3.0-only

import { createRouterTransport } from "@connectrpc/connect";
import { createQueryOptions } from "@connectrpc/connect-query";
import { AuthService } from "@dockwell/api";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";

import { authState } from "../test/auth";
import { recheckAuth } from "./api";

/** A query client whose sign-in state is on screen, read from a control plane that answers when asked to. */
function watched() {
  const held: Array<() => void> = [];
  const getAuthState = vi.fn(async () => {
    await new Promise<void>((resolve) => held.push(resolve));
    return authState();
  });
  const transport = createRouterTransport(({ service }) => service(AuthService, { getAuthState }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const observer = new QueryObserver(client, createQueryOptions(AuthService.method.getAuthState, {}, { transport }));
  const stop = observer.subscribe(() => {});
  const answer = async () => {
    await vi.waitFor(() => expect(held.length).toBeGreaterThan(0));
    for (const resolve of held.splice(0)) resolve();
    await vi.waitFor(() => expect(observer.getCurrentResult().fetchStatus).toBe("idle"));
  };
  return { client, getAuthState, answer, stop };
}

describe("recheckAuth", () => {
  it("starts a read of its own, rather than share one that may have left before the session ended", async () => {
    const { client, getAuthState, answer, stop } = watched();
    await answer();
    // A read is under way, as after coming back to the tab.
    void client.refetchQueries();
    await vi.waitFor(() => expect(getAuthState).toHaveBeenCalledTimes(2));

    recheckAuth(client);
    await vi.waitFor(() => expect(getAuthState).toHaveBeenCalledTimes(3));
    await answer();
    stop();
  });

  it("shares its read with calls that find the session ended while it's under way", async () => {
    const { client, getAuthState, answer, stop } = watched();
    await answer();

    recheckAuth(client);
    recheckAuth(client);
    recheckAuth(client);
    await answer();
    expect(getAuthState).toHaveBeenCalledTimes(2);

    // Once it's done, the next call that finds the session ended reads again.
    recheckAuth(client);
    await answer();
    expect(getAuthState).toHaveBeenCalledTimes(3);
    stop();
  });
});
