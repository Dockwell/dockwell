// SPDX-License-Identifier: AGPL-3.0-only

import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  ChangeState,
  Connection,
  DeploymentState,
  type WatchEventsResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { anApp, aProject, aWebsite, ENV, places, projectPlaces, running, SERVER } from "../../test/apps";
import { applied, changeEvent, moved, shopPlanned, since, snapshot, webPlanned } from "../../test/changes";
import { type Api, feed, onPhone, onScreen, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

const hz1 = aServer({ id: SERVER, name: "hz-1", inventory: { addresses: [] } });

function api(services: ReturnType<typeof anApp>[], ...servers: ReturnType<typeof aServer>[]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...(servers.length ? servers : [hz1]).map(changed), snapshotComplete);
  const projects = [...new Set(services.map((s) => s.resource?.scope ?? "").filter((e) => e !== ENV))];
  return {
    services: { listServices: () => ({ services }) },
    resources: projects.length > 0 ? projectPlaces(...projects.map((e) => e.replace(/^env_/, ""))) : places(),
    servers: { watchServers: watch.stream },
  };
}

afterEach(async () => {
  vi.unstubAllGlobals();
  localStorage.clear();
  await activateLocale("en");
});

/** A watch that opens with its snapshot and stays open. */
async function* snapshotWatch(_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchEventsResponse> {
  yield snapshot([]);
  await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
}

/** A watch that opens, then drops, with err or with no error at all, until told to come back. */
function dropping(err: unknown) {
  let back = () => {};
  const returned = new Promise<void>((resolve) => (back = resolve));
  let opened = 0;
  return {
    reconnect: back,
    async *stream(_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchEventsResponse> {
      opened += 1;
      if (opened === 1) {
        yield snapshot([]);
        if (err !== undefined) throw err;
        return;
      }
      await returned;
      yield snapshot([]);
      await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
    },
  };
}

describe("the apps list", () => {
  it("offers the ways to a first app when there is none, each opening its own flow", async () => {
    const { container } = renderAt("/apps", api([]));
    expect(await screen.findByRole("heading", { name: "No apps yet" })).toBeVisible();
    const ways = screen.getByRole("list", { name: "Ways to run your first app" });
    expect(within(ways).getByRole("link", { name: "From the catalog" })).toHaveAttribute("href", "/catalog");
    expect(within(ways).getByRole("link", { name: "A Git repository" })).toHaveAttribute(
      "href",
      "/apps/new?source=git",
    );
    expect(within(ways).getByRole("link", { name: "A Docker image" })).toHaveAttribute("href", "/apps/new");
    expect(within(ways).getByRole("link", { name: "A Compose file" })).toHaveAttribute(
      "href",
      "/apps/new?source=compose",
    );
    // A website lists on Websites, and the head's New app would be a second way to the same sheet.
    expect(within(ways).queryByRole("link", { name: "A website" })).toBeNull();
    expect(screen.queryByRole("link", { name: "New app" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Export" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Filter apps")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("leads with the catalog, its apps by name in two rows of four, the last saying how many more", async () => {
    const ids = ["wordpress", "postgres", "nextcloud", "gitea", "n8n", "uptime-kuma", "ghost", "a", "b", "c"];
    const entries = ids.map((id) => ({ id, name: id, icon: "" }));
    renderAt("/apps", { ...api([]), catalog: { listCatalogEntries: () => ({ entries }) } });
    const ways = await screen.findByRole("list", { name: "Ways to run your first app" });
    const lead = within(ways).getByRole("link", { name: "From the catalog" }).closest("li") as HTMLElement;
    expect(lead).toHaveClass("is-lead");
    await waitFor(() => expect(within(lead).getAllByRole("link", { name: /^Install / })).toHaveLength(7));
    expect(within(lead).getByRole("link", { name: "Install ghost" })).toHaveAttribute("href", "/catalog/ghost/install");
    expect(within(lead).queryByRole("link", { name: "Install a" })).toBeNull();
    expect(within(lead).getByText("and 3 more")).toBeInTheDocument();
  });

  it("lists an app it can't read beside the rest, and says so", async () => {
    const unreadable = anApp("web", "", {}, { spec: undefined, specUnreadable: "service svc_web: can't open it" });
    renderAt("/apps", api([unreadable, anApp("api", "ghcr.io/acme/api:1.0")]));
    expect(await screen.findByRole("row", { name: /web/ })).toHaveTextContent("Can't be read");
    const beside = screen.getByRole("row", { name: /api/ });
    expect(beside).toHaveTextContent("ghcr.io/acme/api:1.0");
    expect(beside).not.toHaveTextContent("Can't be read");
  });

  it("shows the table's heads while it loads, with nothing to jump when it lands", async () => {
    const { container } = renderAt("/apps", {
      ...api([]),
      services: { listServices: () => new Promise(() => {}) },
    });
    expect(await screen.findByText("Loading apps")).toBeInTheDocument();
    expect(container.querySelectorAll(".apps-skeleton-row")).toHaveLength(3);
    expect(
      screen
        .getAllByRole("columnheader")
        .map((h) => h.textContent)
        .slice(0, 2),
    ).toEqual(["App", "Status"]);
    // New app sits in one place or another once there is a list to say which.
    expect(screen.queryByRole("link", { name: "New app" })).not.toBeInTheDocument();
    // The line under the title is held.
    expect(container.querySelector(".page-lead")).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("holds the filter's place when it remembers there were enough apps to need it", async () => {
    localStorage.setItem("dockwell.apps.count", "70");
    renderAt("/apps", { ...api([]), services: { listServices: () => new Promise(() => {}) } });
    expect(await screen.findByLabelText("Filter apps")).toBeDisabled();
  });

  it("remembers how many apps there were", async () => {
    renderAt("/apps", api([anApp("web", "nginx")]));
    await screen.findByRole("link", { name: "web" });
    await waitFor(() => expect(localStorage.getItem("dockwell.apps.count")).toBe("1"));
  });

  it("leaves out a website's own web service, as the rail and Overview do, so a server with only a website has no apps", async () => {
    const { web, websites } = aWebsite();
    const { unmount } = renderAt("/apps", { ...api([web]), websites });
    expect(await screen.findByRole("heading", { name: "No apps yet" })).toBeVisible();
    expect(screen.queryByRole("row", { name: /web/ })).toBeNull();
    unmount();

    renderAt("/apps", { ...api([web, anApp("api", "ghcr.io/acme/api:2")]), websites });
    expect(await screen.findByRole("row", { name: /api/ })).toBeVisible();
    expect(screen.getByText("1 app")).toBeVisible();
    expect(screen.queryByRole("row", { name: /caddy/ })).toBeNull();
  });

  it("shows one app with what the API has, and no server column with one server", async () => {
    const { container } = renderAt("/apps", api([anApp("web", "ghcr.io/acme/web:1.4")]));
    const row = await screen.findByRole("row", { name: /web/ });
    const table = screen.getByRole("table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["App", "Status", "Revision", "Deployed"]);
    expect(row).toHaveTextContent("ghcr.io/acme/web:1.4");
    expect(row).not.toHaveTextContent("Live");
    expect(row).toHaveTextContent("r3");
    expect(row).toHaveTextContent("1 h ago");
    expect(screen.getByText("1 app")).toBeVisible();
    // Nothing to choose, so no choices.
    expect(screen.queryByRole("radiogroup", { name: "Environment" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Server")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Filter apps")).not.toBeInTheDocument();
    await expectAccessible(container);
  });

  it("gives where each app answers a column from about 1100 px, with its image under its name", async () => {
    onScreen("(min-width: 1100px)");
    const { container } = renderAt("/apps", {
      ...api([anApp("web", "ghcr.io/acme/web:1.4"), anApp("api", "ghcr.io/acme/api:2"), ...aProject("shop", "a", "b")]),
      domains: {
        listDomains: () => ({
          domains: [
            { name: "www.example.com", environmentId: ENV, routes: [{ service: "web", port: 80 }] },
            { name: "example.com", environmentId: ENV, routes: [{ service: "web", port: 80 }] },
          ],
        }),
      },
    });
    const row = await screen.findByRole("row", { name: /web/ });
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "App",
      "Status",
      "Revision",
      "Domain",
      "Deployed",
    ]);
    // As Apps-List has it: the image under the name, so the eye doesn't cross the row for it.
    expect(within(row).getAllByRole("cell")[0]).toHaveTextContent("ghcr.io/acme/web:1.4");
    await waitFor(() => expect(within(row).getAllByRole("cell")[3]).toHaveTextContent("www.example.com+1"));
    expect(within(screen.getByRole("row", { name: /^api/ })).getAllByRole("cell")[3]).toHaveTextContent("—");
    expect(within(screen.getByRole("row", { name: /shop/ })).getAllByRole("cell")[0]).toHaveTextContent("2 apps");
    await expectAccessible(container);
  });

  it("leaves room for an app's name on a fleet, where Runs on takes more of the row", async () => {
    const fleet = () => api([anApp("web", "ghcr.io/acme/web:1.4")], hz1, aServer({ id: "srv_2", name: "hz-2" }));
    // Below 1280 px a fleet's row has no room for where each answers: the name would get about 60 px.
    onScreen("(min-width: 1100px)");
    const narrow = renderAt("/apps", fleet());
    expect(await screen.findByRole("columnheader", { name: "Runs on" })).toBeVisible();
    expect(screen.queryByRole("columnheader", { name: "Domain" })).toBeNull();
    narrow.unmount();

    onScreen("(min-width: 1280px)");
    renderAt("/apps", fleet());
    expect(await screen.findByRole("columnheader", { name: "Domain" })).toBeVisible();
    // What the fixed columns leave the name of a 1280 px screen's table, about 1000 px past the rail.
    const row = 1280 - 232 - 48;
    const fixed = screen
      .getAllByRole("columnheader")
      .map((h) => h.style.width)
      .reduce((sum, w) => sum + (w.endsWith("%") ? (Number.parseFloat(w) / 100) * row : Number.parseFloat(w) || 0), 0);
    expect(row - fixed).toBeGreaterThanOrEqual(240);
  });

  it("leaves where each answers empty while the domains can't be read, rather than say there are none", async () => {
    onScreen("(min-width: 1100px)");
    renderAt("/apps", {
      ...api([anApp("web", "ghcr.io/acme/web:1.4")]),
      domains: {
        listDomains: () => {
          throw new ConnectError("no", Code.Unavailable);
        },
      },
    });
    const row = await screen.findByRole("row", { name: /web/ });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(within(row).getAllByRole("cell")[3]).toHaveTextContent(/^$/);
  });

  it("finds an app by where it answers, as the filter says", async () => {
    const user = userEvent.setup();
    renderAt("/apps", {
      // The filter shows from six apps.
      ...api(["web", "api", "db", "cache", "queue", "worker"].map((n) => anApp(n, `ghcr.io/acme/${n}:1`))),
      domains: {
        listDomains: () => ({
          domains: [{ name: "shop.example.com", environmentId: ENV, routes: [{ service: "web", port: 80 }] }],
        }),
      },
    });
    const filter = await screen.findByLabelText("Filter apps");
    expect(filter).toHaveAttribute("placeholder", "Filter by name, image or domain");
    await user.type(filter, "shop.example");
    await waitFor(() => expect(screen.queryByRole("row", { name: /^api/ })).toBeNull());
    expect(screen.getByRole("row", { name: /web/ })).toBeVisible();
  });

  it("keeps the end of a long image, which tells it from the others, and says the whole of it on hover", async () => {
    const long = "registry.internal.acme-corporation.example.com/platform/backend:1.4.2";
    const { container } = renderAt("/apps", api([anApp("web", long)]));
    await screen.findByRole("row", { name: /web/ });
    const image = container.querySelector(".image-name");
    expect(image).toHaveAttribute("title", long);
    expect(image).toHaveTextContent(long);
    expect(container.querySelector(".image-name-end")).toHaveTextContent(/^backend:1\.4\.2$/);
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

  it("shows a project's services as one named row that folds, beside an app of the default project", async () => {
    const user = userEvent.setup();
    const { container } = renderAt(
      "/apps",
      api([...aProject("shop", "web", "api", "worker", "db", "cache"), anApp("nginx", "nginx:1.27")]),
    );
    const head = await screen.findByRole("row", { name: /shop/ });
    expect(head).toHaveTextContent("5 apps");
    // Its revision column has nothing to say, and says so without leaving a hole.
    expect(within(head).getAllByRole("cell")[2]).toHaveTextContent("—");
    expect(screen.getByRole("row", { name: /worker/ })).toBeVisible();
    expect(screen.getByRole("row", { name: /nginx/ })).toBeVisible();
    await expectAccessible(container);

    await user.click(within(head).getByRole("button", { name: /Fold shop/ }));
    expect(screen.queryByRole("link", { name: "worker" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "nginx" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: /Unfold shop/ }));
    expect(screen.getByRole("link", { name: "worker" })).toBeVisible();
  });

  it("keeps two files' services of one name in two rows", async () => {
    renderAt("/apps", api([...aProject("blog", "db", "web"), ...aProject("git", "db", "web")]));
    expect(await screen.findByRole("row", { name: /blog/ })).toBeVisible();
    expect(screen.getByRole("row", { name: /git/ })).toBeVisible();
    expect(screen.getAllByRole("link", { name: "db" })).toHaveLength(2);
  });

  it("remembers what was folded when you come back", async () => {
    const user = userEvent.setup();
    const first = renderAt("/apps", api(aProject("shop", "web", "api")));
    await user.click(await screen.findByRole("button", { name: "Fold shop" }));
    expect(screen.queryByRole("link", { name: "web" })).not.toBeInTheDocument();
    first.unmount();

    renderAt("/apps", api(aProject("shop", "web", "api")));
    expect(await screen.findByRole("button", { name: "Unfold shop" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "web" })).not.toBeInTheDocument();
  });

  it("calls a project of one app by its name, and goes straight to the app", async () => {
    const user = userEvent.setup();
    const [one] = aProject("blog", "web");
    if (!one) throw new Error("no project");
    const { router } = renderAt("/apps", {
      ...api([one]),
      services: { listServices: () => ({ services: [one] }), listServiceRevisions: () => ({}) },
    });
    const link = await screen.findByRole("link", { name: "blog" });
    expect(screen.queryByRole("button", { name: /Fold/ })).not.toBeInTheDocument();
    await user.click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_blog_web"));
  });

  it("says when a project's service needs a look, and puts that first", async () => {
    const stack = aProject("shop", "web", "api");
    const failing = stack[1]?.deployments[0];
    if (failing) failing.state = DeploymentState.FAILED;
    renderAt("/apps", api([anApp("nginx", "nginx"), ...stack]));
    const head = await screen.findByRole("row", { name: /shop/ });
    const rows = screen.getAllByRole("row");
    expect(rows[1]).toBe(head);
    expect(head).toHaveTextContent("Failed");
  });

  it("filters once there are enough to need it, and shows what it finds inside a folded project", async () => {
    const user = userEvent.setup();
    const { router } = renderAt(
      "/apps",
      api([...aProject("shop", "web", "api", "db"), ...["a", "b", "c"].map((n) => anApp(n, `img/${n}:1`))]),
    );
    const filter = await screen.findByLabelText("Filter apps");
    await user.click(await screen.findByRole("button", { name: /Fold/ }));
    await user.type(filter, "ghcr.io/acme/db");
    await waitFor(() => expect(screen.getByRole("link", { name: "db" })).toBeVisible());
    expect(screen.queryByRole("link", { name: "a" })).not.toBeInTheDocument();
    await waitFor(() => expect(router.state.location.search).toEqual({ q: "ghcr.io/acme/db" }));

    await user.clear(filter);
    await user.type(filter, "nothing-like-it");
    expect(await screen.findByRole("heading", { name: "Nothing matches" })).toBeVisible();
    expect(screen.getByText(/in its name, image or domain/)).toHaveTextContent(
      "No app here has nothing-like-it in its name, image or domain.",
    );
    await user.click(screen.getByRole("button", { name: "Clear the filter" }));
    expect(filter).toHaveValue("");
    expect(await screen.findByRole("link", { name: "a" })).toBeVisible();
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it("names the server or environment a list is narrowed to when it holds nothing, with a button to show all", async () => {
    const user = userEvent.setup();
    const hz2 = aServer({ id: "srv_2", name: "hz-2" });
    const onServer = renderAt("/apps?server=srv_2", api([anApp("web", "nginx")], hz1, hz2));
    expect(await screen.findByRole("heading", { name: "No apps on hz-2" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show all servers" }));
    await waitFor(() => expect(onServer.router.state.location.search).toEqual({}));
    expect(await screen.findByRole("link", { name: "web" })).toBeVisible();
    onServer.unmount();

    const inStaging = renderAt("/apps?environment=env_staging", {
      ...api([anApp("web", "nginx")]),
      resources: places("staging"),
    });
    expect(await screen.findByRole("heading", { name: "No apps in staging" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Show all environments" }));
    await waitFor(() => expect(inStaging.router.state.location.search).toEqual({}));
    expect(await screen.findByRole("link", { name: "web" })).toBeVisible();
  });

  it("finds all of a project by its name", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps",
      api([...aProject("shop", "web", "api", "db"), ...["a", "b", "c"].map((n) => anApp(n, `img/${n}:1`))]),
    );
    await user.type(await screen.findByLabelText("Filter apps"), "shop");
    await waitFor(() => expect(screen.queryByRole("link", { name: "a" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "web" })).toBeVisible();
    expect(screen.getByRole("link", { name: "db" })).toBeVisible();
  });

  it("shows seven apps with a filter, and the project among them as one row", async () => {
    const { container } = renderAt(
      "/apps",
      api([...aProject("shop", "web", "api", "db"), ...["a", "b", "c", "d"].map((n) => anApp(n, `img/${n}:1`))]),
    );
    expect(await screen.findByLabelText("Filter apps")).toBeVisible();
    // A row for each, and one for the project's head.
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(1 + 7 + 1));
    expect(screen.getByRole("row", { name: /shop/ })).toHaveTextContent("3 apps");
    expect(screen.getByText("7 apps")).toBeVisible();
    await expectAccessible(container);
  });

  it("takes seventy apps with long names and images, and stays accessible", async () => {
    const long = `registry.internal.acme.example.com:5000/platform/commerce/${"x".repeat(60)}:2026.11.04`;
    const services = Array.from({ length: 70 }, (_, i) => anApp(`service-${i}-${"y".repeat(40)}`, long));
    const { container } = renderAt("/apps", api(services));
    await screen.findByRole("link", { name: /service-0-/ });
    expect(await screen.findAllByRole("row")).toHaveLength(71);
    expect(screen.getByText("70 apps")).toBeVisible();
    // Every clip says the whole of what it clips.
    expect(container.querySelectorAll(".apps-name[title]").length).toBe(70);
    expect(container.querySelectorAll(".image-name[title]").length).toBe(70);
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
    await screen.findByRole("columnheader", { name: "Runs on" });
    expect(await screen.findByRole("row", { name: /web/ })).toHaveTextContent("hz-1, hz-2");
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
    expect(await screen.findByRole("row", { name: /moving/ })).toHaveTextContent("Deploying");
    expect(screen.getByRole("row", { name: /broken/ })).toHaveTextContent("Failed");
    expect(screen.getByRole("row", { name: /back/ })).toHaveTextContent("Rolled back");
    await waitFor(() => expect(screen.getByRole("row", { name: /away/ })).toHaveTextContent("hz-2 offline"));
    expect(screen.getByText(/3 need a look/)).toBeVisible();
    expect(screen.getByText(/1 deploying/)).toBeVisible();
    await expectAccessible(container);
  });

  it("says one needs a look in the singular", async () => {
    renderAt("/apps", api([anApp("back", "c", {}, { rolledBackChangeId: "chg_x" })]));
    expect(await screen.findByText(/1 needs a look/)).toBeVisible();
  });

  it("needs nothing of the fleet to say how an app runs, and asks nothing of you until then", async () => {
    const web = anApp("web", "nginx");
    renderAt("/apps", {
      ...api([web]),
      // A watch that never answers.
      servers: { watchServers: feed<WatchServersResponse>().stream },
      health: running(web),
    });
    const row = await screen.findByRole("row", { name: /web/ });
    await waitFor(() => expect(row).toHaveTextContent("Running"));
    expect(row).not.toHaveTextContent("Live");
    expect(screen.queryByText(/need a look/)).not.toBeInTheDocument();
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

  it.each(["services", "websites"] as const)(
    "says a role refusal from %s in your language, with nothing to try again",
    async (source) => {
      const refused = () => {
        throw new ConnectError(
          "your role doesn't cover this read",
          Code.PermissionDenied,
          new Headers({ "Dockwell-Error": "needs_role" }),
        );
      };
      const endpoints = {
        ...api([]),
        ...(source === "services" ? { services: { listServices: refused } } : { websites: { listWebsites: refused } }),
      };
      const en = renderAt("/apps", endpoints);
      expect(await screen.findByText("Your role doesn't cover this")).toBeVisible();
      expect(
        screen.getByText("Your role doesn't let you do this here. Ask an admin to change your role."),
      ).toBeVisible();
      expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
      en.unmount();

      await activateLocale("zh-CN");
      const zh = renderAt("/apps", endpoints);
      const callout = (await screen.findByText("你的角色无权访问这里")).closest(".dw-callout");
      expect(callout?.textContent).not.toMatch(/[A-Za-z]/);
      expect(within(callout as HTMLElement).queryByRole("button")).not.toBeInTheDocument();
      zh.unmount();
    },
  );

  it("keeps each row where it is as a deploy starts, until the page is next opened", async () => {
    const hourAgo = timestampFromDate(new Date(Date.now() - 3_600_000));
    const dayAgo = timestampFromDate(new Date(Date.now() - 86_400_000));
    const older = (deployments = [{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.LIVE }]) =>
      anApp("older", "nginx", {}, { updateTime: dayAgo, deployments });
    const newer = anApp("newer", "redis", {}, { updateTime: hourAgo });
    let services = [older(), newer];
    const watch = feed<WatchEventsResponse>();
    const planned = shopPlanned();
    watch.push(snapshot([]));
    const seen = () =>
      screen
        .getAllByRole("link")
        .map((l) => l.textContent)
        .filter((name) => name === "older" || name === "newer");
    const view = renderAt("/apps", {
      ...api([]),
      services: { listServices: () => ({ services }) },
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    await screen.findByRole("link", { name: "older" });
    expect(seen()).toEqual(["newer", "older"]);

    // A deploy of the older one starts, which sorts above all the calm ones.
    services = [older([{ serverId: SERVER, serverName: "hz-1", state: DeploymentState.PENDING }]), newer];
    watch.push(since([changeEvent(moved(planned, { state: ChangeState.APPLYING }))]));
    await screen.findByText("Deploying");
    expect(seen()).toEqual(["newer", "older"]);

    // Opening the page again sorts it.
    view.unmount();
    renderAt("/apps", { ...api(services) });
    await screen.findByRole("link", { name: "older" });
    expect(seen()).toEqual(["older", "newer"]);
  });

  it("says it waits for a server in as many words as its column has", async () => {
    const waiting = anApp(
      "web",
      "nginx",
      {},
      {
        deployments: [{ serverId: "srv_edge", serverName: "edge", state: DeploymentState.PENDING }],
        changeState: ChangeState.APPLYING,
      },
    );
    const edge = aServer({ id: "srv_edge", name: "edge", connection: Connection.UNREACHABLE });
    renderAt("/apps", api([waiting], edge));
    expect(await screen.findByText("Waiting for server")).toBeVisible();
    expect(screen.queryByText(/Waiting for edge/)).not.toBeInTheDocument();
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
    const watch = dropping(new ConnectError("gone", Code.Unavailable));
    renderAt("/apps", {
      ...api([]),
      services: { listServices: list },
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    await screen.findByRole("link", { name: "web" });
    expect(await screen.findByRole("status")).toHaveTextContent("Reconnecting");
    const before = list.mock.calls.length;
    watch.reconnect();
    await waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(before), { timeout: 5000 });
    await waitFor(() => expect(screen.queryByText("Reconnecting")).not.toBeInTheDocument());
  }, 10_000);

  it("takes a stream that ends with no error for a drop too, as when the control plane stops", async () => {
    const list = vi.fn(() => ({ services: [anApp("web", "nginx")] }));
    const watch = dropping(undefined);
    renderAt("/apps", {
      ...api([]),
      services: { listServices: list },
      events: { watchEvents: watch.stream, listEvents: () => ({}) },
    });
    await screen.findByRole("link", { name: "web" });
    expect(await screen.findByRole("status")).toHaveTextContent("Reconnecting");
    const before = list.mock.calls.length;
    watch.reconnect();
    await waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(before), { timeout: 5000 });
    await waitFor(() => expect(screen.queryByText("Reconnecting")).not.toBeInTheDocument());
  }, 10_000);

  it("reads again on coming back once a change applied while it was away, as the shell watches everywhere", async () => {
    const list = vi.fn(() => ({ services: [anApp("web", "nginx")] }));
    let apply = () => {};
    const applying = new Promise<void>((resolve) => (apply = resolve));
    const events = {
      watchEvents: async function* (_req: unknown, ctx: HandlerContext): AsyncGenerator<WatchEventsResponse> {
        yield snapshot([]);
        await applying;
        yield since([changeEvent(applied(webPlanned()))]);
        await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve(), { once: true }));
      },
      listEvents: () => ({}),
    };
    // As the panel's own cache has it: a list stays fresh for 30 s.
    const { router } = renderAt(
      "/apps",
      { ...api([]), services: { listServices: list }, events },
      { staleTime: 30_000 },
    );
    await screen.findByRole("link", { name: "web" });

    // Away and back with nothing applied in between: the list is still fresh, and the watch saw nothing.
    await router.navigate({ to: "/servers" });
    await screen.findByRole("heading", { level: 1, name: "Servers" });
    const before = list.mock.calls.length;
    await router.navigate({ to: "/apps" });
    await screen.findByRole("link", { name: "web" });
    await new Promise((r) => setTimeout(r, 300));
    expect(list.mock.calls.length).toBe(before);

    // A deploy finishes while Servers is open: the shell's watch marks the list old, and coming back reads it.
    await router.navigate({ to: "/servers" });
    await screen.findByRole("heading", { level: 1, name: "Servers" });
    act(() => apply());
    await new Promise((r) => setTimeout(r, 300));
    await router.navigate({ to: "/apps" });
    await waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(before));
  });

  it("reads nothing moving straight from the list to an app", async () => {
    const list = vi.fn(() => ({ services: [anApp("web", "nginx")] }));
    const events = { watchEvents: snapshotWatch, listEvents: () => ({}) };
    const { router } = renderAt(
      "/apps",
      {
        ...api([]),
        services: { listServices: list, listServiceRevisions: () => ({}) },
        events,
      },
      { staleTime: 30_000 },
    );
    await screen.findByRole("link", { name: "web" });
    await new Promise((r) => setTimeout(r, 250));
    const before = list.mock.calls.length;
    expect(before).toBe(1);
    await router.navigate({ to: "/apps/$serviceId", params: { serviceId: "svc_web" } });
    await screen.findByRole("heading", { level: 1, name: "web" });
    await new Promise((r) => setTimeout(r, 300));
    expect(list.mock.calls.length).toBe(before);
  });

  it("stacks its rows on a phone, each one a link big enough to tap", async () => {
    onPhone();
    const user = userEvent.setup();
    const { router, container } = renderAt("/apps", {
      ...api([...aProject("shop", "web", "api"), anApp("nginx", "registry.acme.example.com/platform/nginx:1")]),
      services: {
        listServices: () => ({
          services: [...aProject("shop", "web", "api"), anApp("nginx", "registry.acme.example.com/platform/nginx:1")],
        }),
        listServiceRevisions: () => ({}),
      },
    });
    const link = await screen.findByRole("link", { name: /nginx/ });
    expect(link).not.toHaveTextContent("Live");
    expect(link).toHaveTextContent("r3");
    // A phone shows the last part and its tag, and keeps the whole for those who ask.
    expect(link).toHaveTextContent("nginx:1");
    expect(link).not.toHaveTextContent("registry.acme");
    expect(link.querySelector(".apps-phone-image")).toHaveAttribute(
      "title",
      "registry.acme.example.com/platform/nginx:1",
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    const stack = await screen.findByRole("button", { name: /shop/ });
    expect(stack).toHaveAttribute("aria-expanded", "true");
    expect(stack).toHaveTextContent("2 apps");
    await user.click(stack);
    expect(screen.queryByRole("link", { name: /api/ })).not.toBeInTheDocument();
    await expectAccessible(container);
    await user.click(link);
    await waitFor(() => expect(router.state.location.pathname).toBe("/apps/svc_nginx"));
  });

  it("speaks Chinese", async () => {
    await activateLocale("zh-CN");
    const web = anApp("web", "nginx");
    renderAt("/apps", { ...api([web, ...aProject("shop", "a", "b")]), health: running(web) });
    expect(await screen.findByRole("heading", { level: 1, name: "应用" })).toBeVisible();
    expect(await screen.findByRole("columnheader", { name: "状态" })).toBeVisible();
    expect(screen.getByRole("link", { name: "新建应用" })).toBeVisible();
    await waitFor(() => expect(screen.getByRole("row", { name: /web/ })).toHaveTextContent("正在运行"));
  });
});
