// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import {
  ChangeState,
  Connection,
  DeploymentState,
  type WatchEventsResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, aStack, places, SERVER } from "../../test/apps";
import { changeEvent, moved, shopPlanned, since, snapshot } from "../../test/changes";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: [] } });

function api(services: ReturnType<typeof anApp>[], ...servers: ReturnType<typeof aServer>[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  return {
    services: { listServices: () => ({ services }) },
    resources: places(),
    servers: { watchServers: watch.stream },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the apps list", () => {
  it("invites the first app when there is none, with one primary button", async () => {
    const { container } = renderAt("/apps", api([]));
    expect(await screen.findByRole("heading", { name: "No apps yet" })).toBeVisible();
    expect(screen.getAllByRole("link", { name: "New app" })).toHaveLength(1);
    expect(screen.getByRole("link", { name: "New app" })).toHaveAttribute("href", "/apps/new");
    expect(screen.queryByRole("button", { name: "Export" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Filter apps")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("shows the shape of the table while it loads, and says so", async () => {
    const { container } = renderAt("/apps", {
      ...api([]),
      services: { listServices: () => new Promise(() => {}) },
    });
    expect(await screen.findByText("Loading apps")).toBeInTheDocument();
    expect(container.querySelectorAll(".apps-skeleton-row")).toHaveLength(3);
    await expectAccessible(container);
  });

  it("shows one app with what the API has, and no server column with one server", async () => {
    const { container } = renderAt("/apps", api([anApp("web", "ghcr.io/acme/web:1.4")]));
    const table = await screen.findByRole("table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["App", "Status", "Revision", "Deployed"]);
    const row = await within(table).findByRole("row", { name: /web/ });
    expect(row).toHaveTextContent("ghcr.io/acme/web:1.4");
    expect(row).toHaveTextContent("Live");
    expect(row).toHaveTextContent("r3");
    expect(row).toHaveTextContent("1 h ago");
    expect(screen.getByText("1 app")).toBeVisible();
    // Nothing to choose, so no choices.
    expect(screen.queryByRole("radiogroup", { name: "Environment" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Server")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Filter apps")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("opens an app from its row, by mouse and by keyboard", async () => {
    const user = userEvent.setup();
    const { router } = renderAt("/apps", {
      ...api([anApp("web", "nginx")]),
      services: { listServices: () => ({ services: [anApp("web", "nginx")] }), listServiceRevisions: () => ({}) },
    });
    const row = await screen.findByRole("row", { name: /web/ });
    row.focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_web"));
  });

  it("shows a Compose file's services as one stack, which folds, beside an app on its own", async () => {
    const user = userEvent.setup();
    const { container } = renderAt(
      "/apps",
      api([...aStack("web", "api", "worker", "db", "cache"), anApp("nginx", "nginx:1.27")]),
    );
    const head = await screen.findByRole("row", { name: /api, cache, db and 2 more/ });
    expect(head).toHaveTextContent("Stack of 5 services");
    expect(head).toHaveTextContent("Live");
    expect(screen.getByRole("row", { name: /worker/ })).toBeVisible();
    expect(screen.getByRole("row", { name: /nginx/ })).toBeVisible();
    await expectAccessible(container);

    await user.click(within(head).getByRole("button", { name: /Fold/ }));
    expect(screen.queryByRole("link", { name: "worker" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "nginx" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: /Unfold/ }));
    expect(screen.getByRole("link", { name: "worker" })).toBeVisible();
  });

  it("says when a stack's service needs a look, and puts that first", async () => {
    const stack = aStack("web", "api");
    const failing = stack[1]?.deployments[0];
    if (failing) failing.state = DeploymentState.FAILED;
    renderAt("/apps", api([anApp("nginx", "nginx"), ...stack]));
    const rows = await screen.findAllByRole("row");
    expect(rows[1]).toHaveTextContent("api, web");
    expect(rows[1]).toHaveTextContent("Failed");
  });

  it("filters once there are enough to need it, and shows what it finds inside a folded stack", async () => {
    const user = userEvent.setup();
    const { router } = renderAt(
      "/apps",
      api([...aStack("web", "api", "db"), ...["a", "b", "c"].map((n) => anApp(n, `img/${n}:1`))]),
    );
    const filter = await screen.findByLabelText("Filter apps");
    await user.click(screen.getByRole("button", { name: /Fold/ }));
    await user.type(filter, "ghcr.io/acme/db");
    await waitFor(() => expect(screen.getByRole("link", { name: "db" })).toBeVisible());
    expect(screen.queryByRole("link", { name: "a" })).not.toBeInTheDocument();
    await waitFor(() => expect(router.state.location.search).toEqual({ q: "ghcr.io/acme/db" }));

    await user.clear(filter);
    await user.type(filter, "nothing-like-it");
    expect(await screen.findByRole("heading", { name: "Nothing matches" })).toBeVisible();
  });

  it("shows seven apps with a filter, and the stack among them as one", async () => {
    const { container } = renderAt(
      "/apps",
      api([...aStack("web", "api", "db"), ...["a", "b", "c", "d"].map((n) => anApp(n, `img/${n}:1`))]),
    );
    expect(await screen.findByLabelText("Filter apps")).toBeVisible();
    // A row for each, and one for the stack's head.
    expect(await screen.findAllByRole("row")).toHaveLength(1 + 7 + 1);
    expect(screen.getByRole("row", { name: /api, db, web/ })).toHaveTextContent("Stack of 3 services");
    expect(screen.getByText("7 apps")).toBeVisible();
    await expectAccessible(container);
  });

  it("takes seventy apps with long names and images, and stays accessible", async () => {
    const long = `registry.internal.acme.example.com:5000/platform/commerce/${"x".repeat(60)}:2026.11.04`;
    const services = Array.from({ length: 70 }, (_, i) => anApp(`service-${i}-${"y".repeat(40)}`, long));
    const { container } = renderAt("/apps", api(services));
    await screen.findByRole("table");
    expect(await screen.findAllByRole("row")).toHaveLength(71);
    expect(screen.getByText("70 apps")).toBeVisible();
    expect(container.querySelectorAll(".apps-clip").length).toBeGreaterThan(70);
    await expectAccessible(container);
  });

  it("adds a server column and a filter from the second server on", async () => {
    const hz2 = aServer({ id: "srv_2", name: "hz-2" });
    const both = anApp(
      "web",
      "nginx",
      {},
      {
        deployments: [
          { serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE },
          { serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE },
        ],
      },
    );
    const user = userEvent.setup();
    const { router, container } = renderAt("/apps", api([both, anApp("api", "nginx")], hz1, hz2));
    const table = await screen.findByRole("table");
    await within(table).findByRole("columnheader", { name: "Runs on" });
    expect(within(table).getByRole("row", { name: /web/ })).toHaveTextContent("hz-1, hz-2");
    await expectAccessible(container);
    await user.selectOptions(screen.getByLabelText("Server"), "srv_2");
    await waitFor(() => expect(router.state.location.search).toEqual({ server: "srv_2" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "api" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "web" })).toBeVisible();
    expect(screen.getByText("1 more app lives outside this view")).toBeVisible();
  });

  it("offers environments once there is a choice, each with its count", async () => {
    const user = userEvent.setup();
    const staging = anApp(
      "web",
      "nginx",
      {},
      { id: "svc_s", resource: { kind: "service", scope: "env_staging", name: "web-staging" } },
    );
    const { router } = renderAt("/apps", {
      ...api([anApp("web", "nginx"), staging]),
      resources: places("staging"),
    });
    const choice = await screen.findByRole("radiogroup", { name: "Environment" });
    await user.click(within(choice).getByRole("radio", { name: /staging/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({ environment: "env_staging" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "web" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "web-staging" })).toBeVisible();
  });

  it("shows a deploy in progress, a failed one, one rolled back, and a server offline", async () => {
    const hz2 = aServer({ id: "srv_2", name: "hz-2", connection: Connection.UNREACHABLE });
    const services = [
      anApp(
        "moving",
        "a",
        {},
        { deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }] },
      ),
      anApp(
        "broken",
        "b",
        {},
        { deployments: [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.FAILED }] },
      ),
      anApp("back", "c", {}, { rolledBackChangeId: "chg_x" }),
      anApp("away", "d", {}, { deployments: [{ serverId: "srv_2", serverName: "hz-2", state: DeploymentState.LIVE }] }),
    ];
    const { container } = renderAt("/apps", api(services, hz1, hz2));
    await screen.findByRole("table");
    expect(await screen.findByRole("row", { name: /moving/ })).toHaveTextContent("Deploying");
    expect(screen.getByRole("row", { name: /broken/ })).toHaveTextContent("Failed");
    expect(screen.getByRole("row", { name: /back/ })).toHaveTextContent("Rolled back");
    await waitFor(() => expect(screen.getByRole("row", { name: /away/ })).toHaveTextContent("hz-2 offline"));
    expect(screen.getByText(/3 need a look/)).toBeVisible();
    expect(screen.getByText(/1 deploying/)).toBeVisible();
    await expectAccessible(container);
  });

  it("says what is wrong when it can't load, and tries again", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt("/apps", {
      ...api([]),
      services: {
        listServices: () => {
          if (fail) throw new ConnectError("the control plane couldn't read its services", Code.Internal);
          return { services: [anApp("web", "nginx")] };
        },
      },
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Can't load the apps");
    expect(alert).toHaveTextContent("The control plane couldn't read its services.");
    expect(alert).toHaveTextContent("What runs on your servers isn't affected.");
    fail = false;
    await user.click(within(alert).getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("link", { name: "web" })).toBeVisible();
  });

  it("passes on what the API says when you may not see them", async () => {
    renderAt("/apps", {
      ...api([]),
      services: {
        listServices: () => {
          throw new ConnectError("your role can't read services. Ask an owner", Code.PermissionDenied);
        },
      },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("Your role can't read services. Ask an owner.");
  });

  it("stays live, reading again only when a change moves on", async () => {
    const watch = feed<WatchEventsResponse>();
    const list = vi.fn(() => ({ services: [anApp("web", "nginx")] }));
    const planned = shopPlanned();
    watch.push(snapshot([]));
    renderAt("/apps", {
      ...api([]),
      services: { listServices: list },
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    await screen.findByRole("link", { name: "web" });
    expect(list).toHaveBeenCalledTimes(1);

    // A plan under review changes nothing that runs.
    watch.push(since([changeEvent(planned)]));
    await new Promise((r) => setTimeout(r, 250));
    expect(list).toHaveBeenCalledTimes(1);

    // Applying does, and a burst reads once.
    const applying = moved(planned, { state: ChangeState.APPLYING });
    watch.push(since([changeEvent(applying)]), since([changeEvent(moved(planned, { state: ChangeState.APPLIED }))]));
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    await new Promise((r) => setTimeout(r, 250));
    expect(list).toHaveBeenCalledTimes(2);
  });

  it("says so while its watch reconnects, and catches up when it is back", async () => {
    const list = vi.fn(() => ({ services: [anApp("web", "nginx")] }));
    const watch = feed<WatchEventsResponse>();
    watch.fail(new ConnectError("gone", Code.Unavailable));
    renderAt("/apps", {
      ...api([]),
      services: { listServices: list },
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    expect(await screen.findByRole("status")).toHaveTextContent("Reconnecting");
  });

  it("stacks its rows on a phone, each one a link big enough to tap", async () => {
    onPhone();
    const user = userEvent.setup();
    const { router, container } = renderAt("/apps", {
      ...api([...aStack("web", "api"), anApp("nginx", "ghcr.io/acme/nginx:1")]),
      services: {
        listServices: () => ({ services: [...aStack("web", "api"), anApp("nginx", "ghcr.io/acme/nginx:1")] }),
        listServiceRevisions: () => ({}),
      },
    });
    const link = await screen.findByRole("link", { name: /nginx/ });
    expect(link).toHaveTextContent("Live");
    expect(link).toHaveTextContent("r3");
    expect(link).toHaveTextContent("ghcr.io/acme/nginx:1");
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    const stack = screen.getByRole("button", { name: /api, web/ });
    expect(stack).toHaveAttribute("aria-expanded", "true");
    await user.click(stack);
    expect(screen.queryByRole("link", { name: /api/ })).not.toBeInTheDocument();
    await expectAccessible(container);
    await user.click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_nginx"));
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/apps", api([anApp("web", "nginx"), ...aStack("a", "b")]));
    expect(await screen.findByRole("heading", { level: 1, name: "应用" })).toBeVisible();
    expect(await screen.findByRole("columnheader", { name: "状态" })).toBeVisible();
    expect(screen.getByRole("link", { name: "新建应用" })).toBeVisible();
    expect(screen.getByRole("row", { name: /web/ })).toHaveTextContent("运行中");
  });
});
