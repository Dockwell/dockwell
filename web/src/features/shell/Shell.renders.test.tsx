// SPDX-License-Identifier: AGPL-3.0-only

import type { WatchServersResponse } from "@dockwell/api";
import type { CommandBarProps } from "@dockwell/ui";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

import { feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

// Each part of the frame counts its renders; the command bar keeps every list it is given.
const { seen, counted } = vi.hoisted(() => {
  const seen = { rail: 0, topbar: 0, tabbar: 0, items: [] as unknown[] };
  /** The component, counting each time it renders. */
  function counted<P>(part: "rail" | "topbar" | "tabbar", Component: (props: P) => ReactNode) {
    return (props: P) => {
      seen[part]++;
      return Component(props);
    };
  }
  return { seen, counted };
});

vi.mock("./Rail", async (load) => {
  const real = await load<typeof import("./Rail")>();
  return { ...real, Rail: counted("rail", real.Rail) };
});
vi.mock("./Topbar", async (load) => {
  const real = await load<typeof import("./Topbar")>();
  return { ...real, Topbar: counted("topbar", real.Topbar) };
});
vi.mock("./TabBar", async (load) => {
  const real = await load<typeof import("./TabBar")>();
  return { ...real, TabBar: counted("tabbar", real.TabBar) };
});
vi.mock("@dockwell/ui", async (load) => {
  const real = await load<typeof import("@dockwell/ui")>();
  return {
    ...real,
    CommandBar: (props: CommandBarProps) => {
      seen.items.push(props.items);
      return real.CommandBar(props);
    },
  };
});

const hz1 = aServer({ id: "srv_1", name: "hz-1", stats: sample(10) });
const hz2 = aServer({ id: "srv_2", name: "hz-2", stats: sample(20) });

/** Renders the overview for two servers, then sends five samples from each. */
async function sendSamples(): Promise<void> {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), changed(hz2), snapshotComplete);
  renderAt("/", { servers: { watchServers: watch.stream } });
  await screen.findByText("20%");
  seen.rail = seen.topbar = seen.tabbar = 0;
  act(() => {
    for (let i = 1; i <= 5; i++) {
      watch.push(
        changed(aServer({ ...hz1, stats: sample(30 + i) })),
        changed(aServer({ ...hz2, stats: sample(40 + i) })),
      );
    }
  });
}

describe("the shell as samples arrive", () => {
  it("leaves the rail and top bar alone, while the page shows each sample", async () => {
    await sendSamples();
    expect(await screen.findByText("45%")).toBeVisible();
    expect(seen.rail).toBe(0);
    expect(seen.topbar).toBe(0);
  });

  it("leaves the tab bar alone on phones", async () => {
    onPhone();
    await sendSamples();
    expect(await screen.findByText("45%")).toBeVisible();
    expect(seen.tabbar).toBe(0);
  });

  it("keeps the open command bar's list", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), changed(hz2), snapshotComplete);
    renderAt("/", { servers: { watchServers: watch.stream } });
    await screen.findByText("20%");
    await user.keyboard("{Control>}k{/Control}");
    const first = seen.items.at(-1);
    expect(first).toBeDefined();

    act(() => watch.push(changed(aServer({ ...hz1, stats: sample(55) }))));
    expect(await screen.findByText("55%")).toBeVisible();

    expect(new Set(seen.items.slice(seen.items.indexOf(first)))).toEqual(new Set([first]));
    expect(within(screen.getByRole("dialog")).getAllByRole("option").length).toBeGreaterThan(0);
  });

  it("draws the frame again when a server joins", async () => {
    const watch = feed<WatchServersResponse>();
    watch.push(changed(hz1), snapshotComplete);
    renderAt("/", { servers: { watchServers: watch.stream } });
    await screen.findByText("10%");
    seen.rail = 0;
    act(() => watch.push(changed(hz2)));
    expect(await screen.findByText("Fleet")).toBeVisible();
    expect(seen.rail).toBeGreaterThan(0);
  });
});
