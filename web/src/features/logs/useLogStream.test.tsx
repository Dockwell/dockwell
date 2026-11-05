// SPDX-License-Identifier: AGPL-3.0-only

import { act } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { feed, renderWithApi } from "../../test/render";
import { LogStore } from "./store";
import { useLogStream } from "./useLogStream";

// Each call to set state keeps its closure until React next renders, and one that
// changes nothing doesn't make it render, so a long follow keeps them all.
const updates = vi.hoisted(() => ({ count: 0 }));
vi.mock("react", async (actual) => {
  const react = await actual<typeof import("react")>();
  return {
    ...react,
    useState: (initial: unknown) => {
      const [value, set] = react.useState(initial);
      return [
        value,
        (next: unknown) => {
          updates.count++;
          set(next);
        },
      ];
    },
  };
});

const request = { id: "svc_web" };

function Follower({ store }: { store: LogStore }) {
  useLogStream(request, store);
  return null;
}

describe("following a log", () => {
  it("sets state once for a connection, not once for each message", async () => {
    const stream = feed<{
      chunks: never[];
      sources: never[];
      servers: never[];
      caughtUp: boolean;
      resumeToken: string;
    }>();
    renderWithApi(<Follower store={new LogStore()} />, { logs: { watchLogs: stream.stream } });
    const quiet = { chunks: [], sources: [], servers: [], caughtUp: false, resumeToken: "" };
    await act(() => stream.push(quiet));
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    const once = updates.count;
    await act(async () => {
      for (let i = 0; i < 500; i++) stream.push(quiet);
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(updates.count - once).toBeLessThan(5);
  });
});
