// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  type ProposeChangeRequest,
  type Resource,
  ResourceSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { applied, changeEvent, shopPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, sample, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: "srv_1", name: "hz-1", controlServer: true, stats: sample(12) });

function resource(id: string, kind: string, scope: string, name: string, displayName = ""): Resource {
  return create(ResourceSchema, { id, resource: { kind, scope, name }, displayName });
}

const defaultProject = resource("prj_default", "project", "org_1", "default");
const shopProject = resource("prj_shop", "project", "org_1", "shop", "Shop");
const production = resource("env_prod", "environment", "prj_default", "production");
const staging = resource("env_staging", "environment", "prj_default", "staging");

async function openBar(api: Api, path = "/") {
  const user = userEvent.setup();
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const rendered = renderAt(path, { ...api, servers: { watchServers: watch.stream } });
  await screen.findByRole("heading", { level: 1 });
  await user.keyboard("{Control>}k{/Control}");
  return { ...rendered, user, bar: await screen.findByRole("dialog", { name: "Command bar" }) };
}

describe("the command bar", () => {
  it("reaches Activity, and what happened on a server", async () => {
    const { user, bar, router } = await openBar({});
    await user.click(within(bar).getByRole("option", { name: "Open the activity" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/activity"));

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(within(again).getByRole("option", { name: "Show what happened on hz-1" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_1" }));
  });

  it("names projects and environments once there is a choice, and opens recent plans", async () => {
    const shop = applied(shopPlanned());
    const { user, bar, router } = await openBar({
      resources: {
        listResources: (req) => ({
          resources: req.kind === "project" ? [defaultProject, shopProject] : [production, staging],
        }),
      },
      events: { listEvents: () => ({ events: [changeEvent(shop)] }) },
      changes: { getChange: () => ({ change: shop }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const projects = await within(bar).findByRole("group", { name: "Projects" });
    expect(
      within(projects)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Show the activity of default", "Show the activity of shop Shop"]);
    const environments = within(bar).getByRole("group", { name: "Environments" });
    expect(
      within(environments)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["Show the activity of default / production", "Show the activity of default / staging"]);
    await expectAccessible(document.body);
    await user.click(within(bar).getByRole("option", { name: /Show the activity of shop/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ resource: "prj_shop" }));

    await user.keyboard("{Control>}k{/Control}");
    const again = await screen.findByRole("dialog", { name: "Command bar" });
    await user.click(await within(again).findByRole("option", { name: /Open the plan: Create the project shop/ }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
  });

  it("leaves out the only project and environment", async () => {
    const { bar } = await openBar({
      resources: { listResources: (req) => ({ resources: req.kind === "project" ? [defaultProject] : [production] }) },
    });
    await within(bar).findByRole("option", { name: /Create a project/ });
    expect(within(bar).queryByRole("group", { name: "Projects" })).toBeNull();
    expect(within(bar).queryByRole("group", { name: "Environments" })).toBeNull();
  });

  it("creates a project: it asks for the name, then opens the plan", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const { user, bar } = await openBar({
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await user.click(within(bar).getByRole("option", { name: "Create a project Opens a plan" }));
    const ask = screen.getByRole("dialog", { name: "Command bar" });
    const input = within(ask).getByRole("combobox", { name: "Name the new project, such as shop" });
    expect(input).toHaveValue("");
    await user.type(input, "Shop!");
    expect(within(ask).getByRole("option")).toHaveTextContent("Shop! can't be a name");
    await user.clear(input);
    await user.type(input, "shop");
    await user.click(within(ask).getByRole("option", { name: "Create the project shop Opens a plan" }));

    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(proposals).toHaveLength(1);
    expect(proposals[0]?.summary).toBe("Create the project shop");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "project", name: "shop" });
    expect(proposals[0]?.edits[0]?.spec?.kind.case).toBe("project");
  });

  it("creates an environment in the project you choose, once there is a choice", async () => {
    const proposals: ProposeChangeRequest[] = [];
    const { user, bar } = await openBar({
      resources: {
        listResources: (req) => ({ resources: req.kind === "project" ? [defaultProject, shopProject] : [] }),
      },
      changes: {
        proposeChange: (req) => {
          proposals.push(req);
          return { change: shopPlanned() };
        },
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });
    await within(bar).findByRole("group", { name: "Projects" });
    await user.click(within(bar).getByRole("option", { name: "Create an environment Opens a plan" }));
    const which = screen.getByRole("dialog", { name: "Command bar" });
    expect(within(which).getByRole("combobox", { name: "Which project is it for?" })).toBeVisible();
    await user.click(within(which).getByRole("option", { name: "In shop Shop" }));
    const name = within(screen.getByRole("dialog", { name: "Command bar" })).getByRole("combobox", {
      name: "Name the new environment, such as staging",
    });
    await user.type(name, "staging");
    await user.keyboard("{Enter}");

    await screen.findByRole("dialog", { name: "Review plan" });
    expect(proposals[0]?.summary).toBe("Create the environment staging in shop");
    expect(proposals[0]?.edits[0]?.resource).toMatchObject({ kind: "environment", name: "staging", scope: "prj_shop" });
  });
});
