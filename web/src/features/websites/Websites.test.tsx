// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  CertificateState,
  ChangeSchema,
  ChangeState,
  type CreateWebsiteRequest,
  FileType,
  HealthState,
  PlanAction,
  type UpdateWebsiteRequest,
  type WatchChangeResponse,
  type WatchServersResponse,
  type WebsiteInfo,
  WebsiteInfoSchema,
  WebsiteReachSchema,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, places, SERVER } from "../../test/apps";
import { type Api, feed, onPhone, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { changedLines } from "./website";

const SITE_ENV = "env_blog";
const PORT_ENV = "env_tools";

const blog = create(WebsiteInfoSchema, {
  id: "prj_blog",
  name: "blog-example-com",
  domain: "blog.example.com",
  spec: { serves: { case: "proxy", value: { url: "https://backend.example.com" } } },
  environmentId: SITE_ENV,
  serviceId: "svc_blogweb",
  routeId: "rte_blog",
  updateTime: timestampFromDate(new Date(2026, 10, 1, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const tools = create(WebsiteInfoSchema, {
  id: "prj_tools",
  name: "tools-example-com",
  domain: "tools.example.com",
  spec: { serves: { case: "proxy", value: { port: 3000 } } },
  environmentId: PORT_ENV,
  routeId: "rte_tools",
  updateTime: timestampFromDate(new Date(2026, 9, 29, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

const docs = create(WebsiteInfoSchema, {
  id: "prj_docs",
  name: "docs-example-com",
  domain: "docs.example.com",
  spec: { serves: { case: "folder", value: { root: "/var/www/docs", index: [], singlePageApp: false } } },
  environmentId: "env_docs",
  serviceId: "svc_docsweb",
  routeId: "rte_docs",
  updateTime: timestampFromDate(new Date(2026, 9, 20, 12)),
  serverId: SERVER,
  serverName: "hz-1",
});

/** A folder's entries as ListFiles has them: a name and its mode. */
const entry = (name: string, mode = "-rw-r--r--", type = FileType.FILE) => ({ name, mode, type });

/**
 * A server's disk, as ListFiles reads it: the folders given, each with its mode and
 * what's in it, and the folders above them, which hold them. What isn't there is
 * refused, as the agent refuses it, and listed in asked.
 */
function disk(folders: Record<string, { mode: string; entries: ReturnType<typeof entry>[] }>, asked: string[] = []) {
  return (req: { path: string }) => {
    asked.push(req.path);
    const folder = folders[req.path];
    if (folder)
      return { path: req.path, directory: { mode: folder.mode, type: FileType.DIRECTORY }, entries: folder.entries };
    const children = Object.entries(folders).filter(
      ([path]) =>
        path.slice(0, path.lastIndexOf("/")) === req.path || (req.path === "/" && path.lastIndexOf("/") === 0),
    );
    if (children.length > 0 || req.path === "/srv") {
      return {
        path: req.path,
        directory: { mode: "drwxr-xr-x", type: FileType.DIRECTORY },
        entries: children.map(([path, f]) => entry(path.slice(path.lastIndexOf("/") + 1), f.mode, FileType.DIRECTORY)),
      };
    }
    const err = new ConnectError(`there's nothing at ${req.path}`, Code.NotFound);
    err.metadata.set("Dockwell-Error", "not_found");
    throw err;
  };
}

const days = (n: number) => timestampFromDate(new Date(Date.now() + n * 86_400_000 - 60_000));

const domains = [
  {
    name: "blog.example.com",
    environmentId: SITE_ENV,
    url: "https://blog.example.com",
    routes: [{ id: "rte_blog", name: "site", service: "web", port: 8080 }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: { domain: "blog.example.com", certificate: { state: CertificateState.ISSUED, expireTime: days(61) } },
      },
    ],
  },
  {
    name: "tools.example.com",
    environmentId: PORT_ENV,
    url: "http://tools.example.com",
    routes: [{ id: "rte_tools", name: "site", hostPort: 3000, tls: "off" }],
    servers: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        status: { domain: "tools.example.com", certificate: { state: CertificateState.NONE } },
      },
    ],
  },
];

const webOfBlog = anApp(
  "web",
  "caddy:2.11.7-alpine@sha256:e70b",
  {},
  {
    id: "svc_blogweb",
    resource: { kind: "service", scope: SITE_ENV, name: "web" },
  },
);

const config = `{
\tadmin off
}

:8080 {
\treverse_proxy https://backend.example.com
}
`;

function api(websites: WebsiteInfo[], more: Api = {}): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    resources: places(),
    servers: { watchServers: watch.stream },
    services: { listServices: () => ({ services: [anApp("shop", "ghcr.io/acme/shop:1.4"), webOfBlog] }) },
    health: { listHealth: () => ({ services: [aHealth("svc_blogweb", { state: HealthState.HEALTHY })] }) },
    domains: { listDomains: () => ({ domains }) },
    websites: {
      listWebsites: () => ({ websites }),
      previewWebsite: (req) => {
        const proxy = req.website?.serves.case === "proxy" ? req.website.serves.value : undefined;
        if (proxy?.url === "http://localhost") {
          const err = new ConnectError(
            "localhost is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL",
            Code.InvalidArgument,
          );
          err.metadata.set("Dockwell-Error", "invalid");
          throw err;
        }
        if (req.website?.serves.case === "folder") {
          return { website: req.website, config: ":8080 {\n\troot * /srv\n\tfile_server\n}\n" };
        }
        return {
          website: req.website,
          config: proxy?.port ? "" : config.replace("https://backend.example.com", proxy?.url ?? ""),
        };
      },
      ...more.websites,
    },
    ...Object.fromEntries(Object.entries(more).filter(([key]) => key !== "websites")),
  };
}

const planned = create(ChangeSchema, {
  id: "chg_site",
  summary: "Add the website shop.example.com",
  state: ChangeState.PLANNED,
  plan: {
    fingerprint: "feedfacefeedfacefeedfacefeedface",
    summary: { create: 3 },
    resources: [
      { id: "svc_web", resource: { kind: "service", scope: "env_new", name: "web" }, action: PlanAction.CREATE },
    ],
  },
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const rowOf = (name: string | RegExp) => screen.findByRole("row", { name });

/** A screen as wide as the mock's, where every column has room. */
function onWideScreen() {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.startsWith("(min-width"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

describe("The websites", () => {
  it("lists each with what it sends to, how it serves, its certificate and when it changed", async () => {
    onWideScreen();
    const { container } = renderAt("/websites", api([blog, tools]));
    expect(await screen.findByRole("heading", { level: 1, name: "Websites" })).toBeVisible();
    const first = await rowOf(/blog\.example\.com/);
    expect(first).toHaveTextContent("https://backend.example.com");
    expect(first).toHaveTextContent("Reverse proxy");
    await waitFor(() => expect(first).toHaveTextContent("Valid · 61 days left"));
    expect(first).toHaveTextContent("1 Nov");
    const second = await rowOf(/tools\.example\.com/);
    expect(second).toHaveTextContent("Port 3000 on the server");
    await waitFor(() => expect(second).toHaveTextContent("TLS ends in front"));
    expect(container.querySelector(".page-lead")).toHaveTextContent("2 sites on hz-1");
    expect(screen.getByRole("link", { name: "Files" })).toHaveAttribute("href", `/servers/${SERVER}/files`);
    expect(screen.getByRole("link", { name: "New website" })).toHaveAttribute("href", "/websites/new");
    await expectAccessible(container);
  });

  it("gives up what a website is before its domain, on a narrower screen", async () => {
    renderAt("/websites", api([blog]));
    const row = await rowOf(/blog\.example\.com/);
    expect(row).toHaveTextContent("https://backend.example.com");
    expect(screen.queryByRole("columnheader", { name: "Type" })).not.toBeInTheDocument();
  });

  it("says there are none yet, and how to start", async () => {
    renderAt("/websites", api([]));
    expect(await screen.findByRole("heading", { name: "No websites yet" })).toBeVisible();
    expect(screen.getByRole("link", { name: "New website" })).toHaveAttribute("href", "/websites/new");
  });

  it("says when it can't read them, and tries again", async () => {
    const user = userEvent.setup();
    let calls = 0;
    renderAt(
      "/websites",
      api([], {
        websites: {
          listWebsites: () => {
            calls++;
            throw new ConnectError("the control plane couldn't complete the request", Code.Internal);
          },
        },
      }),
    );
    expect(await screen.findByText("Can't load the websites")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(calls).toBe(2));
  });

  it("keeps a website's own services out of the apps", async () => {
    renderAt("/apps", api([blog]));
    expect(await screen.findByRole("row", { name: /shop/ })).toBeVisible();
    expect(screen.queryByRole("row", { name: /^web\b/ })).not.toBeInTheDocument();
  });

  it("says it can't load the apps when it can't tell which are a website's own", async () => {
    renderAt(
      "/apps",
      api([], {
        websites: {
          listWebsites: () => {
            throw new ConnectError("the control plane couldn't complete the request", Code.Internal);
          },
        },
      }),
    );
    expect(await screen.findByText("Can't load the apps")).toBeVisible();
    expect(screen.queryByRole("row", { name: /^web\b/ })).not.toBeInTheDocument();
  });

  it("leads from a website's domain in Domains to the website, not its web server", async () => {
    onWideScreen();
    renderAt("/domains", api([blog]));
    const row = await screen.findByRole("row", { name: /blog\.example\.com/ }, { timeout: 5_000 });
    const links = within(row).getAllByRole("link");
    expect(links.some((a) => a.getAttribute("href") === "/websites/prj_blog")).toBe(true);
    expect(links.some((a) => a.getAttribute("href")?.startsWith("/apps/"))).toBe(false);
  });

  it("keeps a website's own services out of the command bar", async () => {
    const user = userEvent.setup();
    renderAt("/websites", api([blog]));
    await rowOf(/blog\.example\.com/);
    await user.keyboard("{Control>}k{/Control}");
    const menu = await screen.findByRole("dialog");
    expect(await within(menu).findByRole("option", { name: /Open shop/ })).toBeVisible();
    expect(within(menu).queryByRole("option", { name: /^Open web\b/ })).not.toBeInTheDocument();
    expect(within(menu).getByRole("option", { name: /Open the apps/ })).toHaveTextContent("1");
  });

  it("says whether what each sends to answers, and never Healthy before its server has", async () => {
    onWideScreen();
    const reach = (answered: boolean, said: string) =>
      create(WebsiteReachSchema, { answered, said, checkTime: timestampFromDate(new Date()) });
    const cases: [WebsiteInfo, RegExp, string][] = [
      [create(WebsiteInfoSchema, { ...blog, reach: reach(true, "200 OK") }), /blog\.example\.com/, "Healthy"],
      [blog, /blog\.example\.com/, "Running"],
      [tools, /tools\.example\.com/, "Not checked"],
      [
        create(WebsiteInfoSchema, { ...blog, reach: reach(false, "connection refused") }),
        /blog\.example\.com/,
        "Can't reach backend.example.com",
      ],
      [
        create(WebsiteInfoSchema, { ...blog, reach: reach(false, "503 Service Unavailable") }),
        /blog\.example\.com/,
        "backend.example.com answers 503",
      ],
      [
        create(WebsiteInfoSchema, { ...tools, reach: reach(false, "nothing listens on port 3000") }),
        /tools\.example\.com/,
        "Nothing listens on port 3000",
      ],
    ];
    for (const [site, name, says] of cases) {
      const { unmount } = renderAt("/websites", api([site]));
      const row = await screen.findByRole("row", { name }, { timeout: 5_000 });
      await waitFor(() => expect(row).toHaveTextContent(says), { timeout: 5_000 });
      if (says !== "Healthy") expect(row).not.toHaveTextContent("Healthy");
      unmount();
    }
  }, 60_000);

  it("stacks on a phone, a row to each website", async () => {
    onPhone();
    renderAt("/websites", api([blog, tools]));
    const row = await screen.findByRole("link", { name: /blog\.example\.com/ });
    expect(row).toHaveAttribute("href", "/websites/prj_blog");
    expect(row).toHaveTextContent("Reverse proxy");
  });
});

describe("New website", () => {
  it("sends a domain to a URL, and plans it", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    const { container } = renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await expectAccessible(container);
    await user.type(within(sheet).getByLabelText("Domain"), "Shop.Example.com");
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.type(within(sheet).getByLabelText("URL"), "https://backend.example.com/shop");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.domain).toBe("shop.example.com");
    expect(asked?.website?.serves).toEqual({
      case: "proxy",
      value: expect.objectContaining({ url: "https://backend.example.com/shop", sendTargetHost: false }),
    });
    expect(asked?.tls).toBe("");
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
  });

  it("sends a domain to a port on the server behind a tunnel", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByLabelText("Domain"), "tools.example.com");
    await user.click(within(sheet).getByRole("radio", { name: /A port on this server/ }));
    await user.type(within(sheet).getByLabelText("Port on the server"), "3000");
    await user.click(within(sheet).getByRole("switch", { name: /tunnel/i }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({ case: "proxy", value: expect.objectContaining({ port: 3000 }) });
    expect(asked?.tls).toBe("off");
    expect(asked?.trustedProxies).toEqual(["127.0.0.1"]);
  });

  it("says at the field what's missing, and what the server refused", async () => {
    const user = userEvent.setup();
    // The refusal is waited on by the request that brings it, not by the clock, as a
    // slow runner takes its time getting there.
    let refused!: () => void;
    const asked = new Promise<void>((resolve) => {
      refused = resolve;
    });
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: () => {
            refused();
            const err = new ConnectError(
              "the path /a;b has a character a website can't send on. Use letters, digits, -, ., _, ~ and /",
              Code.InvalidArgument,
            );
            err.metadata.set("Dockwell-Error", "invalid");
            throw err;
          },
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(within(sheet).getByLabelText("Domain")).toHaveAttribute("aria-invalid", "true");
    expect(within(sheet).getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
    await user.click(within(sheet).getByLabelText("Domain"));
    await user.paste("shop.example.com");
    await user.click(within(sheet).getByLabelText("URL"));
    await user.paste("https://backend.example.com/a;b");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await asked;
    expect(await within(sheet).findByText(/a character a website can't send on/)).toBeVisible();
    expect(within(sheet).getByLabelText("URL")).toHaveAttribute("aria-invalid", "true");
  });
});

describe("A website that serves a folder", () => {
  it("starts from a folder that follows the domain, says what's in it, and plans it", async () => {
    const user = userEvent.setup();
    let asked: CreateWebsiteRequest | undefined;
    const read: string[] = [];
    renderAt(
      "/websites/new",
      api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        files: {
          listFiles: disk(
            {
              "/srv/site": {
                mode: "drwxr-xr-x",
                entries: [entry("index.html"), entry("style.css"), entry("img", "drwxr-xr-x", FileType.DIRECTORY)],
              },
            },
            read,
          ),
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    expect(within(sheet).getByRole("radio", { name: /^Files in a folder/ })).toBeChecked();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(within(sheet).getByLabelText("Folder")).toHaveAttribute("aria-invalid", "true");
    await user.type(within(sheet).getByLabelText("Domain"), "shop.example.com");
    expect(within(sheet).getByLabelText("Folder")).toHaveValue("/var/www/shop.example.com");
    // The domain gave it a folder, so what it said of none goes.
    expect(within(sheet).getByLabelText("Folder")).not.toHaveAttribute("aria-invalid", "true");
    expect(await within(sheet).findByText(/Nothing is at/, {}, { timeout: 3_000 })).toBeVisible();

    await user.clear(within(sheet).getByLabelText("Folder"));
    await user.type(within(sheet).getByLabelText("Folder"), "/srv/site");
    expect(await within(sheet).findByText("Holds index.html and 2 more.", {}, { timeout: 3_000 })).toBeVisible();
    expect(within(sheet).getByRole("link", { name: "Open in Files" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fsrv%2Fsite`,
    );
    // The domain doesn't take a folder that was typed.
    await user.type(within(sheet).getByLabelText("Domain"), "x");
    expect(within(sheet).getByLabelText("Folder")).toHaveValue("/srv/site");

    await user.click(within(sheet).getByRole("switch", { name: /Single-page app/ }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.website?.serves).toEqual({
      case: "folder",
      value: expect.objectContaining({ root: "/srv/site", index: [], singlePageApp: true }),
    });
    expect(read).toContain("/srv/site");
    // A folder that isn't there is seen from the folder above, as asking for it would go on Activity as refused.
    expect(read).not.toContain("/var/www/shop.example.com");
  });

  it("says when the web server can't read the folder, or there's no index in it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/websites/new",
      api([], {
        files: {
          listFiles: disk({
            "/srv/closed": { mode: "drwx------", entries: [entry("index.html")] },
            "/srv/empty": { mode: "drwxr-xr-x", entries: [entry("readme.md")] },
            "/srv/shut": { mode: "drwxr-xr-x", entries: [entry("index.html", "-rw-------")] },
          }),
        },
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    const folder = within(sheet).getByLabelText("Folder");
    await user.type(folder, "/srv/closed");
    expect(
      await within(sheet).findByText(/closed to everyone else, so it answers 403/, {}, { timeout: 3_000 }),
    ).toBeVisible();
    await user.clear(folder);
    await user.type(folder, "/srv/shut");
    expect(await within(sheet).findByText(/closed to everyone but its owner/, {}, { timeout: 3_000 })).toBeVisible();
    await user.clear(folder);
    await user.type(folder, "/srv/empty");
    expect(
      await within(sheet).findByText(/so its address answers 404 until there is/, {}, { timeout: 3_000 }),
    ).toBeVisible();
  });

  it("shows what serves it, and leads to its files", async () => {
    onWideScreen();
    renderAt(
      "/websites/prj_docs/configuration",
      api([docs], {
        files: {
          listFiles: disk({ "/var/www/docs": { mode: "drwxr-xr-x", entries: [entry("index.html")] } }),
        },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "docs.example.com" })).toBeVisible();
    expect(screen.getByText(/Static files from \/var\/www\/docs/)).toBeVisible();
    expect(screen.getByRole("link", { name: "Edit files" })).toHaveAttribute(
      "href",
      `/servers/${SERVER}/files?path=%2Fvar%2Fwww%2Fdocs`,
    );
    expect(screen.getByLabelText("Serves")).toHaveValue("folder");
    expect(screen.getByLabelText("Folder")).toHaveValue("/var/www/docs");
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("file_server"));
  });

  it("lists as static files, with its folder under its domain", async () => {
    onWideScreen();
    renderAt("/websites", api([docs]));
    const row = await rowOf(/docs\.example\.com/);
    expect(row).toHaveTextContent("/var/www/docs");
    expect(row).toHaveTextContent("Static files");
  });
});

describe("New website on a fleet", () => {
  it("asks which server, and takes you to it", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchServersResponse>();
    watch.push(
      changed(aServer({ id: SERVER, name: "hz-1" })),
      changed(aServer({ id: "srv_2", name: "hz-2" })),
      snapshotComplete,
    );
    let asked: CreateWebsiteRequest | undefined;
    renderAt("/websites/new", {
      ...api([], {
        websites: {
          createWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
      servers: { watchServers: watch.stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "New website" });
    await user.type(within(sheet).getByRole("textbox", { name: "Domain" }), "shop.example.com");
    await user.click(within(sheet).getByRole("radio", { name: /^A URL/ }));
    await user.type(within(sheet).getByRole("textbox", { name: "URL" }), "https://backend.example.com");
    const server = await within(sheet).findByRole("combobox", { name: "Server" });
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText("Choose the server it runs on.")).toBeVisible();
    await waitFor(() => expect(server).toHaveFocus());
    await user.selectOptions(server, "hz-2");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked?.servers).toEqual(["srv_2"]));
  });
});

describe("A website's configuration", () => {
  it("shows what its web server runs, marks what an edit changes, and plans it", async () => {
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    const { container } = renderAt(
      "/websites/prj_blog",
      api([blog, tools], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    expect(await screen.findByRole("heading", { level: 1, name: "blog.example.com" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Visit site" })).toHaveAttribute("href", "https://blog.example.com");
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://backend.example.com"));
    expect(code.querySelector(".is-changed")).toBeNull();
    const review = screen.getByRole("button", { name: /Review plan/ });
    expect(review).toBeDisabled();
    await expectAccessible(container);

    const url = screen.getByLabelText("URL");
    await user.clear(url);
    await user.type(url, "https://new.example.com");
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://new.example.com"));
    expect(code.querySelectorAll(".is-changed")).toHaveLength(1);
    expect(screen.getByText("1 change")).toBeVisible();
    expect(screen.getByText("Was https://backend.example.com")).toBeVisible();
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.id).toBe("prj_blog");
    expect(asked?.website?.serves).toEqual({
      case: "proxy",
      value: expect.objectContaining({ url: "https://new.example.com" }),
    });

    // Discard puts it back as it is.
    await user.keyboard("{Escape}");
    await user.click(await screen.findByRole("button", { name: "Discard" }));
    expect(screen.getByLabelText("URL")).toHaveValue("https://backend.example.com");
  });

  it("shows what its web server really runs, and plans its settings back when that differs", async () => {
    const user = userEvent.setup();
    let asked: UpdateWebsiteRequest | undefined;
    const odd = ':8080 {\n\trespond "elsewhere"\n}\n';
    renderAt(
      "/websites/prj_blog",
      api([create(WebsiteInfoSchema, { ...blog, config: odd, configDiffers: true })], {
        websites: {
          updateWebsite: (req) => {
            asked = req;
            return { change: planned };
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      }),
    );
    const code = await screen.findByRole("region", { name: "The web server's config" });
    await waitFor(() => expect(code).toHaveTextContent("reverse_proxy https://backend.example.com"));
    expect(code.querySelectorAll(".is-changed").length).toBeGreaterThan(0);
    expect(screen.getByText(/runs a config these settings don't write/)).toBeVisible();
    const review = screen.getByRole("button", { name: /Review plan/ });
    await waitFor(() => expect(review).toBeEnabled());
    await user.click(review);
    await waitFor(() =>
      expect(asked?.website?.serves).toEqual({
        case: "proxy",
        value: expect.objectContaining({ url: "https://backend.example.com" }),
      }),
    );
  });

  it("says what won't do, as you type, and holds the plan back", async () => {
    const user = userEvent.setup();
    renderAt("/websites/prj_blog/configuration", api([blog]));
    const url = await screen.findByLabelText("URL");
    await user.clear(url);
    await user.type(url, "http://localhost");
    expect(await screen.findByText("Not valid")).toBeVisible();
    expect(url).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", { name: /Review plan/ })).toBeDisabled();
  });

  it("says how the ingress reaches a port, which has no config of its own, and no logs", async () => {
    const user = userEvent.setup();
    renderAt("/websites/prj_tools/configuration", api([blog, tools]));
    expect(await screen.findByRole("heading", { level: 2, name: "Route" })).toBeVisible();
    expect(screen.getByText("127.0.0.1:3000")).toBeVisible();
    expect(screen.getByLabelText("Port on the server")).toHaveValue("3000");
    await user.click(screen.getByRole("link", { name: "Logs" }));
    expect(await screen.findByRole("heading", { name: "Nothing here writes logs" })).toBeVisible();
  });

  it("says when there's no website by that ID", async () => {
    renderAt("/websites/prj_gone/configuration", api([blog]));
    expect(await screen.findByRole("heading", { name: "No website with this ID" })).toBeVisible();
  });
});

describe("Marking what a config's edit changes", () => {
  it("marks the lines that are new, and none when nothing was there to compare", () => {
    expect(changedLines("", "a\nb\n").map((l) => l.changed)).toEqual([false, false]);
    expect(changedLines("a\nb\nc\n", "a\nx\nc\n").map((l) => [l.n, l.text, l.changed])).toEqual([
      [1, "a", false],
      [2, "x", true],
      [3, "c", false],
    ]);
    expect(changedLines("a\nc\n", "a\nb\nc\n").map((l) => l.changed)).toEqual([false, true, false]);
  });
});
