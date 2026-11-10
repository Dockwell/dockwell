// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  Connection,
  ContainerMountSchema,
  ContainerPortSchema,
  ContainerState,
  type ListContainersResponse,
  ListContainersResponseSchema,
  ResourceSchema,
  ServerContainerSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { webPlanned } from "../../test/changes";
import { feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { serviceNameFor } from "./containers";

const hz1 = aServer({ id: "srv_1", name: "hz-1" });

function container(name: string, fields: Parameters<typeof create<typeof ServerContainerSchema>>[1] = {}) {
  return create(ServerContainerSchema, {
    id: `id-of-${name}`,
    name,
    image: "nginx:1.27",
    state: ContainerState.RUNNING,
    status: "Up 3 hours",
    ...fields,
  });
}

function answer(fields: Parameters<typeof create<typeof ListContainersResponseSchema>>[1]): ListContainersResponse {
  return create(ListContainersResponseSchema, fields);
}

async function renderList(listContainers: () => ListContainersResponse | Promise<ListContainersResponse>) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  const rendered = renderAt("/servers/srv_1", {
    servers: { watchServers: watch.stream },
    containers: { listContainers },
  });
  await screen.findByRole("heading", { level: 1 });
  return rendered;
}

afterEach(() => {
  vi.useRealTimers();
  activateLocale("en");
});

describe("the name a service takes", () => {
  it("is written as a service's name is", () => {
    const named = (name: string, composeService = "") => serviceNameFor(container(name, { composeService }));
    expect(named("legacy_api")).toBe("legacy-api");
    expect(named("shop-db-1", "db")).toBe("db");
    expect(named("9lives")).toBe("app-9lives");
    expect(named("Web.Server")).toBe("web-server");
  });
});

describe("the containers Dockwell didn't make", () => {
  it("says what is listed, that looking changes nothing, and where Dockwell's own are", async () => {
    await renderList(() =>
      answer({
        ownContainers: 3,
        containers: [
          container("legacy-api", {
            ports: [create(ContainerPortSchema, { serverPort: 8080, containerPort: 80, protocol: "tcp" })],
            mounts: [create(ContainerMountSchema, { volume: "api-data", target: "/data" })],
          }),
          container("old-db", { state: ContainerState.EXITED, image: "postgres:17" }),
        ],
      }),
    );
    const panel = await screen.findByRole("region", { name: "Containers Dockwell didn't make" });
    expect(await within(panel).findByRole("link", { name: "legacy-api" })).toHaveAttribute(
      "href",
      "/servers/srv_1/containers/legacy-api",
    );
    expect(within(panel).getByText("Dockwell only reads these. Open one to take it on as an app.")).toBeVisible();
    expect(within(panel).getByText(/Dockwell runs 3 more containers here, which aren't listed/)).toBeVisible();
    expect(within(panel).getByRole("link", { name: "Apps" })).toHaveAttribute("href", "/apps");
    expect(within(panel).getByText("2 containers, 1 running")).toBeVisible();
    expect(within(panel).getByText("8080→80")).toBeVisible();
    expect(within(panel).getByText("api-data")).toBeVisible();
    expect(within(panel).getByText("Stopped")).toBeVisible();
    await expectAccessible(panel);
  });

  it("says so when there are none, and keeps its place", async () => {
    await renderList(() => answer({ ownContainers: 1 }));
    const panel = await screen.findByRole("region", { name: "Containers Dockwell didn't make" });
    expect(await within(panel).findByText("Docker runs nothing here that Dockwell didn't make.")).toBeVisible();
    expect(within(panel).getByText(/Dockwell runs 1 more container here, which isn't listed/)).toBeVisible();
    expect(within(panel).queryByRole("table")).toBeNull();
  });

  it("gives a project a column only once there is more than one", async () => {
    const one = [container("a", { composeProject: "shop" }), container("b", { composeProject: "shop" })];
    const { unmount } = await renderList(() => answer({ containers: one }));
    await screen.findByRole("link", { name: "a" });
    expect(screen.queryByRole("columnheader", { name: "Project" })).toBeNull();
    unmount();
    await renderList(() => answer({ containers: [...one, container("c")] }));
    await screen.findByRole("link", { name: "c" });
    expect(screen.getByRole("columnheader", { name: "Project" })).toBeVisible();
  });

  it("grows a filter past a glance, and a way through hundreds", async () => {
    const many = Array.from({ length: 250 }, (_, i) => container(`box-${i}`));
    await renderList(() => answer({ containers: many, unlisted: 7 }));
    const panel = await screen.findByRole("region", { name: "Containers Dockwell didn't make" });
    await within(panel).findByRole("link", { name: "box-0" });
    expect(within(panel).getAllByRole("row")).toHaveLength(101);
    const user = userEvent.setup();
    await user.click(within(panel).getByRole("button", { name: "Show more" }));
    expect(within(panel).getAllByRole("row")).toHaveLength(201);
    expect(within(panel).getByText(/This server has 7 more containers than fit here/)).toBeVisible();

    await user.type(within(panel).getByRole("textbox", { name: "Filter containers" }), "box-249");
    expect(within(panel).getAllByRole("row")).toHaveLength(2);
    await user.clear(within(panel).getByRole("textbox", { name: "Filter containers" }));
    await user.type(within(panel).getByRole("textbox", { name: "Filter containers" }), "nothing");
    expect(within(panel).getByText("No container matches this filter.")).toBeVisible();
  });

  it("has no filter for what reads at a glance", async () => {
    await renderList(() => answer({ containers: Array.from({ length: 12 }, (_, i) => container(`box-${i}`)) }));
    await screen.findByRole("link", { name: "box-0" });
    expect(screen.queryByRole("textbox", { name: "Filter containers" })).toBeNull();
  });

  it("says why it can't read them, and nothing it read before", async () => {
    await renderList(() => {
      throw new ConnectError(
        "the agent on hz-1 isn't connected, so what Docker runs there can't be read now",
        Code.Unavailable,
      );
    });
    const panel = await screen.findByRole("region", { name: "Containers Dockwell didn't make" });
    expect(await within(panel).findByText(/isn't connected/)).toBeVisible();
    expect(within(panel).queryByRole("table")).toBeNull();
  });

  it("says in its own words, in Chinese too, that a server not reporting can't say, and asks it nothing", async () => {
    await activateLocale("zh-CN");
    const listContainers = vi.fn(() => {
      throw new ConnectError("hz-1 isn't connected, so nothing can say what runs there", Code.Unavailable);
    });
    const watch = feed<WatchServersResponse>();
    watch.push(changed(aServer({ id: "srv_1", name: "hz-1", connection: Connection.UNREACHABLE })), snapshotComplete);
    renderAt("/servers/srv_1", { servers: { watchServers: watch.stream }, containers: { listContainers } });
    await screen.findByRole("heading", { level: 1 });
    const panel = await screen.findByRole("region", { name: "非 Dockwell 创建的容器" });
    expect(panel).toHaveTextContent("hz-1 没有上报数据，因此现在无法读取那里 Docker 运行的内容。");
    expect(panel).not.toHaveTextContent("isn't connected");
    expect(listContainers).not.toHaveBeenCalled();
  });

  it("says Docker isn't answering, or that the server waits for its agent, in its own words, and asks nothing", async () => {
    const listContainers = vi.fn(() => answer({ containers: [container("legacy")] }));
    for (const [server, words] of [
      [
        aServer({ id: "srv_1", name: "hz-1", dockerUnreadableSince: timestampFromDate(new Date()) }),
        "Docker isn't answering on hz-1, so what runs there can't be read now.",
      ],
      [
        aServer({ id: "srv_1", name: "hz-1", connection: Connection.PENDING }),
        "hz-1 is waiting for its agent, so what Docker runs there can't be read yet.",
      ],
    ] as const) {
      const watch = feed<WatchServersResponse>();
      watch.push(changed(server), snapshotComplete);
      const { unmount } = renderAt("/servers/srv_1", {
        servers: { watchServers: watch.stream },
        containers: { listContainers },
      });
      await screen.findByRole("heading", { level: 1 });
      expect(await screen.findByText(words)).toBeVisible();
      unmount();
    }
    expect(listContainers).not.toHaveBeenCalled();
  });

  it("stacks on a phone, with each row a way in", async () => {
    onPhone();
    await renderList(() => answer({ containers: [container("legacy-api")] }));
    const row = await screen.findByRole("link", { name: /legacy-api/ });
    expect(row).toHaveAttribute("href", "/servers/srv_1/containers/legacy-api");
    expect(within(row).getByText("Running · nginx:1.27")).toBeVisible();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows an image Docker can only name by its ID in a width that fits", async () => {
    const id = `sha256:${"ab".repeat(32)}`;
    await renderList(() => answer({ containers: [container("moved", { image: id })] }));
    expect(await screen.findByText(id.slice(0, 19))).toBeVisible();
  });
});

/** Renders a container's page, with a server to be on. */
function renderContainer(path: string, api: Parameters<typeof renderAt>[1]) {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(hz1), snapshotComplete);
  return renderAt(path, { servers: { watchServers: watch.stream }, ...api });
}

describe("a container's page", () => {
  it("says what Docker says of it, and reads its logs by its server", async () => {
    const asked: unknown[] = [];
    renderContainer("/servers/srv_1/containers/legacy-api", {
      containers: {
        listContainers: () =>
          answer({
            containers: [
              container("legacy-api", {
                composeProject: "shop",
                composeService: "api",
                mounts: [create(ContainerMountSchema, { hostPath: "/srv/app", target: "/app", readOnly: true })],
              }),
            ],
          }),
      },
      logs: {
        listLogSources: (req) => {
          asked.push({ id: req.id, server: req.server });
          return {};
        },
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "legacy-api" })).toBeVisible();
    expect(await screen.findByRole("region", { name: "What Docker says of it" })).toBeVisible();
    expect(screen.getByText("Up 3 hours")).toBeVisible();
    expect(screen.getByText("/srv/app→/app")).toBeVisible();
    expect(screen.getByText("shop")).toBeVisible();
    expect(screen.queryByRole("button", { name: /restart|stop|deploy/i })).toBeNull();
    await vi.waitFor(() => expect(asked).toContainEqual({ id: "legacy-api", server: "srv_1" }));
  });

  it("drops what it last read once the server stops answering, and says why", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let offline = false;
    renderContainer("/servers/srv_1/containers/legacy-api", {
      containers: {
        listContainers: () => {
          if (offline) throw new ConnectError("the agent on hz-1 isn't connected", Code.Unavailable);
          return answer({ containers: [container("legacy-api")] });
        },
      },
      logs: {
        listLogSources: () => ({}),
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    expect(await screen.findByText("Up 3 hours")).toBeVisible();
    expect(screen.getByRole("heading", { level: 1, name: "legacy-api" }).parentElement).toHaveTextContent("Running");

    offline = true;
    await act(() => vi.advanceTimersByTimeAsync(16_000));
    const panel = await screen.findByRole("region", { name: "What Docker says of it" });
    expect(within(panel).getByText(/isn't connected/)).toBeVisible();
    expect(screen.queryByText("Up 3 hours")).toBeNull();
    expect(screen.getByRole("heading", { level: 1, name: "legacy-api" }).parentElement).not.toHaveTextContent(
      "Running",
    );
  });

  it("says why its logs can't be read while its server is away, and reads them once it's back", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let offline = true;
    renderContainer("/servers/srv_1/containers/legacy-api", {
      containers: { listContainers: () => answer({ containers: [container("legacy-api")] }) },
      logs: {
        listLogSources: () => {
          if (offline) throw new ConnectError("the agent on hz-1 isn't connected", Code.Unavailable);
          return {};
        },
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    const logs = await screen.findByRole("region", { name: "Logs" });
    expect(within(logs).getByText("These logs can't be read now")).toBeVisible();
    expect(within(logs).getByText(/isn't connected/)).toBeVisible();
    expect(screen.queryByText("Reading logs")).toBeNull();

    offline = false;
    await act(() => vi.advanceTimersByTimeAsync(6_000));
    await vi.waitFor(() => expect(screen.queryByText("These logs can't be read now")).toBeNull());
  });

  it("plans taking it on, under a name it works out, and says nothing restarts", async () => {
    const asked: unknown[] = [];
    renderContainer("/servers/srv_1/containers/legacy_api", {
      containers: {
        listContainers: () => answer({ containers: [container("legacy_api", { composeService: "" })] }),
        adoptContainer: (req) => {
          asked.push({ id: req.id, server: req.server, service: req.service });
          return { change: webPlanned() };
        },
      },
      logs: {
        listLogSources: () => ({}),
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    const panel = await screen.findByRole("region", { name: "Take it on" });
    expect(within(panel).getByText(/doesn't restart it/)).toBeVisible();
    expect(within(panel).getByRole("textbox", { name: "Name as a service" })).toHaveValue("legacy-api");
    await userEvent.setup().click(within(panel).getByRole("button", { name: "Plan taking it on" }));
    await vi.waitFor(() => expect(asked).toEqual([{ id: "legacy_api", server: "srv_1", service: "legacy-api" }]));
  });

  it("joins the project its Compose project was taken on as", async () => {
    const asked: unknown[] = [];
    const place = (id: string, kind: string, scope: string, name: string) =>
      create(ResourceSchema, { id, resource: { kind, scope, name } });
    renderContainer("/servers/srv_1/containers/shop-late-1", {
      containers: {
        listContainers: () =>
          answer({ containers: [container("shop-late-1", { composeProject: "shop", composeService: "late" })] }),
        adoptContainer: (req) => {
          asked.push({ id: req.id, environment: req.environment });
          return { change: webPlanned() };
        },
      },
      resources: {
        listResources: (req) => ({
          resources:
            req.kind === "project"
              ? [place("prj_default", "project", "org_1", "default"), place("prj_shop", "project", "org_1", "shop")]
              : [
                  place("env_default", "environment", "prj_default", "production"),
                  place("env_shop", "environment", "prj_shop", "production"),
                ],
        }),
      },
      logs: {
        listLogSources: () => ({}),
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    const panel = await screen.findByRole("region", { name: "Take it on" });
    expect(await within(panel).findByText(/Its Compose project was taken on already, so this joins it/)).toBeVisible();
    await userEvent.setup().click(within(panel).getByRole("button", { name: "Plan taking it on" }));
    await vi.waitFor(() => expect(asked).toEqual([{ id: "shop-late-1", environment: "env_shop" }]));
  });

  it("offers its whole Compose project once there is more of it", async () => {
    const asked: unknown[] = [];
    renderContainer("/servers/srv_1/containers/shop-web-1", {
      containers: {
        listContainers: () =>
          answer({
            containers: [
              container("shop-web-1", { composeProject: "shop", composeService: "web" }),
              container("shop-db-1", { composeProject: "shop", composeService: "db" }),
              container("shop-run-1", { composeProject: "shop", oneOff: true }),
            ],
          }),
      },
      compose: {
        adoptCompose: (req) => {
          asked.push({ name: req.name, server: req.server });
          return { change: webPlanned() };
        },
      },
      logs: {
        listLogSources: () => ({}),
        async *watchLogs() {
          yield { caughtUp: true };
        },
      },
    });
    await userEvent.setup().click(await screen.findByRole("button", { name: "Plan taking on all 2 of shop" }));
    await vi.waitFor(() => expect(asked).toEqual([{ name: "shop", server: "srv_1" }]));
  });

  it("says when it has gone", async () => {
    renderContainer("/servers/srv_1/containers/gone", {
      containers: { listContainers: () => answer({}) },
      logs: {
        listLogSources: () => {
          throw new ConnectError("there's no container gone on hz-1", Code.NotFound);
        },
      },
    });
    expect(await screen.findByText("This container isn't here")).toBeVisible();
  });
});
