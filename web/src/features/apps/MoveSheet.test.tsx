// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  MoveBlockSchema,
  type MoveServiceRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, places } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

function api(asked: MoveServiceRequest[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: "srv_1", name: "hz-1" })), snapshotComplete);
  const service = anApp("web", "nginx:1.5", {
    mounts: [{ source: { case: "volume", value: "data" }, target: "/data" }],
  });
  return {
    resources: places("staging"),
    services: {
      listServices: () => ({ services: [service] }),
      listServiceRevisions: () => ({ revisions: [aRevision(1n, "nginx:1.5")] }),
      moveService: (req: MoveServiceRequest) => {
        asked.push(req);
        // The worker shares its volume, until it moves too.
        if (!req.also.includes("worker")) {
          return {
            blocks: [
              create(MoveBlockSchema, {
                resource: { kind: "volume", scope: "env_prod", name: "data" },
                why: "the volume data is worker's too, and a volume is in one place. Move worker too",
                moveToo: "worker",
              }),
            ],
          };
        }
        return { change: webPlanned() };
      },
    },
    servers: { watchServers: watch.stream },
    domains: { listDomains: () => ({}) },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
  };
}

describe("Moving an app", () => {
  it("goes to a project of its own by default, and moves what holds it back too at a click", async () => {
    const user = userEvent.setup();
    const asked: MoveServiceRequest[] = [];
    const { container } = renderAt("/apps/svc_web/settings", api(asked));
    const row = await screen.findByRole("region", { name: "Project" }, { timeout: 5000 });
    await user.click(within(row).getByRole("button", { name: "Move web" }));
    const sheet = await screen.findByRole("dialog", { name: "Move web" });
    expect(within(sheet).getByRole("combobox", { name: "Where it goes" })).toHaveValue("new");
    expect(within(sheet).getByRole("textbox", { name: "Project name" })).toHaveValue("web");
    await expectAccessible(container);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({ id: "svc_web", newProject: "web", also: [] });
    const blocks = await within(sheet).findByRole("list", { name: "What holds it back" });
    expect(blocks).toHaveTextContent("the volume data is worker's too");
    await user.click(within(blocks).getByRole("button", { name: "Move worker too" }));
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(asked[1]).toMatchObject({ newProject: "web", also: ["worker"] });
  });

  it("goes to another environment, by its project and its own name", async () => {
    const user = userEvent.setup();
    const asked: MoveServiceRequest[] = [];
    renderAt("/apps/svc_web/settings", api(asked));
    const row = await screen.findByRole("region", { name: "Project" }, { timeout: 5000 });
    await user.click(within(row).getByRole("button", { name: "Move web" }));
    const sheet = await screen.findByRole("dialog", { name: "Move web" });
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Where it goes" }), "env_staging");
    expect(within(sheet).queryByRole("textbox", { name: "Project name" })).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({ toProject: "prj_1", toEnvironment: "env_staging" });
  });
});
