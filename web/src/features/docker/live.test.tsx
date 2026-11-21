// SPDX-License-Identifier: AGPL-3.0-only

import { createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { DockerObjectKind, DockerService } from "@dockwell/api";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it } from "vitest";

import { untilAborted } from "../../test/render";
import { useDockerWatch } from "./live";

function show(state: DocumentVisibilityState) {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
  document.dispatchEvent(new Event("visibilitychange"));
}

afterEach(() => show("visible"));

it("releases the Compose connection in a hidden tab and reopens it on return", async () => {
  let opened = 0;
  let closed = 0;
  const transport = createRouterTransport(({ service }) => {
    service(DockerService, {
      async *watchDockerObjects(ask, ctx) {
        expect(ask.selection?.kind).toBe(DockerObjectKind.COMPOSE);
        expect(ask.visibleKeys).toHaveLength(0);
        opened++;
        yield { watchStatus: { serverId: "viewed", live: true } };
        await untilAborted(ctx.signal);
        closed++;
      },
    });
  });
  const { unmount } = renderHook(() => useDockerWatch(["viewed"], [], () => {}, DockerObjectKind.COMPOSE), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <TransportProvider transport={transport}>{children}</TransportProvider>
    ),
  });
  await waitFor(() => expect(opened).toBe(1));
  act(() => show("hidden"));
  await waitFor(() => expect(closed).toBe(1));
  act(() => show("visible"));
  await waitFor(() => expect(opened).toBe(2));
  unmount();
  await waitFor(() => expect(closed).toBe(2));
});
